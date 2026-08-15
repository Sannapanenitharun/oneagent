// Package aggregate turns high-volume per-event envelopes into per-interval
// summaries before they reach the exporter.
//
// The motivating case is the access log collector. It emits one api_call
// envelope per HTTP request, and the OTLP exporter turns each of those into an
// individual log record. A web server handling a modest 200 requests/second
// therefore produces 17 million log records a day, per host, to answer
// questions ("how many requests, how slow, how many errors") that are
// fundamentally about counts and distributions rather than individual events.
//
// Aggregating first collapses that to a few hundred metric points per
// interval — commonly a 100-1000x reduction — while answering those questions
// better, because a count computed here is exact whereas a count derived from
// sampled or dropped log records is not.
//
// What this deliberately does NOT do is aggregate host metrics. Those are
// sampled once per interval from /proc, so each (source, labels) already
// appears exactly once per window: there is nothing to combine, and a sampler
// would add machinery for no reduction.
package aggregate

import (
	"log"
	"strconv"
	"time"

	"github.com/oneagent/agent/internal/collector"
)

// Config controls aggregation. Zero values are replaced with defaults by New.
type Config struct {
	Enabled bool
	// Interval is the summary window. It is independent of the collection
	// interval: raw requests arrive continuously, and this decides how often
	// they are summarised.
	Interval time.Duration
	// MaxContexts caps distinct (method, path, status) combinations tracked in
	// one window. This is the backstop against a path pattern the normalizer
	// does not recognise; beyond it, everything collapses into one overflow
	// context rather than growing without bound.
	MaxContexts int
	// MaxSamples caps retained latency samples per context. Percentiles come
	// from a reservoir of this size rather than every observation.
	MaxSamples int
	// KeepRawEvents forwards the original per-request envelopes as well as the
	// summaries. Off by default — forwarding both defeats the purpose — but
	// available when per-request detail is genuinely needed.
	KeepRawEvents bool
}

const (
	defaultInterval    = 60 * time.Second
	defaultMaxContexts = 2000
	defaultMaxSamples  = 2048

	// overflowPath is the label used once MaxContexts is reached. It is
	// deliberately visible in the output: silently dropping data is worse than
	// showing a bucket that says "everything else".
	overflowPath = "{overflow}"
)

type contextKey struct {
	method string
	path   string
	status string
}

type bucket struct {
	count  int64
	errors int64
	sum    float64
	min    float64
	max    float64

	// lat holds a bounded sample of latencies for percentiles. Counts above
	// are exact regardless of what the reservoir retains.
	lat *Reservoir
}

func newBucket(maxSamples int) *bucket {
	return &bucket{lat: NewReservoir(maxSamples)}
}

func (b *bucket) observe(durationMs float64, isError bool) {
	if b.count == 0 {
		b.min, b.max = durationMs, durationMs
	} else {
		if durationMs < b.min {
			b.min = durationMs
		}
		if durationMs > b.max {
			b.max = durationMs
		}
	}
	b.count++
	if isError {
		b.errors++
	}
	b.sum += durationMs
	b.lat.Observe(durationMs)
}

// Aggregator is used from a single goroutine (the daemon's drain loop), so it
// holds no locks. Add and Flush are both called from that loop.
type Aggregator struct {
	cfg     Config
	agentID string

	contexts map[contextKey]*bucket
	overflow *bucket
	// overflowLogged keeps the cardinality warning to once per window rather
	// than once per event.
	overflowLogged bool
}

func New(agentID string, cfg Config) *Aggregator {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultInterval
	}
	if cfg.MaxContexts <= 0 {
		cfg.MaxContexts = defaultMaxContexts
	}
	if cfg.MaxSamples <= 0 {
		cfg.MaxSamples = defaultMaxSamples
	}
	return &Aggregator{
		cfg:      cfg,
		agentID:  agentID,
		contexts: make(map[contextKey]*bucket, 64),
	}
}

// Interval is the flush period the caller should tick at.
func (a *Aggregator) Interval() time.Duration { return a.cfg.Interval }

// Add offers an envelope to the aggregator. It reports whether the envelope
// was absorbed: false means the caller should export it unchanged, which is
// the path every non-api_call signal takes.
func (a *Aggregator) Add(e collector.Envelope) bool {
	if !a.cfg.Enabled || e.Kind != collector.KindAPICall {
		return false
	}

	key := contextKey{
		method: orUnknown(e.Labels["method"]),
		path:   NormalizePath(e.Labels["path"]),
		status: orUnknown(e.Labels["status"]),
	}

	b, ok := a.contexts[key]
	if !ok {
		if len(a.contexts) >= a.cfg.MaxContexts {
			b = a.overflowBucket()
		} else {
			b = newBucket(a.cfg.MaxSamples)
			a.contexts[key] = b
		}
	}

	// Value carries request duration in milliseconds; 0 means the access log
	// format did not include timing, which is still a countable request.
	b.observe(e.Value, isServerError(key.status))

	return !a.cfg.KeepRawEvents
}

func (a *Aggregator) overflowBucket() *bucket {
	if a.overflow == nil {
		a.overflow = newBucket(a.cfg.MaxSamples)
	}
	if !a.overflowLogged {
		a.overflowLogged = true
		log.Printf("aggregate: reached %d distinct request contexts in one window; "+
			"further combinations are summarised under path=%q. If this persists, a path "+
			"pattern is not being normalized — check the paths in your access log.",
			a.cfg.MaxContexts, overflowPath)
	}
	return a.overflow
}

// Flush ends the current window and returns the summary envelopes for it. The
// aggregator is empty afterwards and ready for the next window.
func (a *Aggregator) Flush(now time.Time) []collector.Envelope {
	if len(a.contexts) == 0 && a.overflow == nil {
		return nil
	}

	// 7 series per context is the shape below; preallocating avoids repeated
	// growth on hosts with many endpoints.
	out := make([]collector.Envelope, 0, (len(a.contexts)+1)*7)
	for key, b := range a.contexts {
		out = append(out, a.summarize(key, b, now)...)
	}
	if a.overflow != nil {
		key := contextKey{method: "*", path: overflowPath, status: "*"}
		out = append(out, a.summarize(key, a.overflow, now)...)
	}

	a.contexts = make(map[contextKey]*bucket, len(a.contexts))
	a.overflow = nil
	a.overflowLogged = false
	return out
}

// summarize turns one bucket into the series describing it. Each is a separate
// metric envelope so the existing OTLP exporter maps them to gauge data points
// with no changes on its side.
func (a *Aggregator) summarize(key contextKey, b *bucket, now time.Time) []collector.Envelope {
	if b.count == 0 {
		return nil
	}
	labels := map[string]string{
		"method": key.method,
		"path":   key.path,
		"status": key.status,
	}

	sorted := b.lat.Sorted()

	series := []struct {
		name  string
		value float64
	}{
		{"http.server.requests", float64(b.count)},
		{"http.server.errors", float64(b.errors)},
		{"http.server.duration.avg", b.sum / float64(b.count)},
		{"http.server.duration.p50", Percentile(sorted, 0.50)},
		{"http.server.duration.p95", Percentile(sorted, 0.95)},
		{"http.server.duration.p99", Percentile(sorted, 0.99)},
		{"http.server.duration.max", b.max},
	}

	out := make([]collector.Envelope, 0, len(series))
	for _, s := range series {
		out = append(out, collector.Envelope{
			Kind:      collector.KindMetric,
			AgentID:   a.agentID,
			Source:    s.name,
			Timestamp: now,
			Labels:    copyLabels(labels),
			Value:     s.value,
		})
	}
	return out
}

// copyLabels gives every envelope its own map. Sharing one map across seven
// envelopes would mean a later mutation by any consumer silently rewrote the
// labels of the other six.
func copyLabels(m map[string]string) map[string]string {
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func isServerError(status string) bool {
	code, err := strconv.Atoi(status)
	return err == nil && code >= 500
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
