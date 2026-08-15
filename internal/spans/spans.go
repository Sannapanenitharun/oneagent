// Package spans computes statistics over trace spans and decides which spans
// are actually forwarded to the backend.
//
// The ordering of those two jobs is the important part, and it is deliberate:
// statistics are computed over EVERY span, before any sampling decision. If you
// sample first and count afterwards, your request counts and error rates
// silently become estimates scaled by whatever sampling rate happened to be in
// effect — and the whole point of keeping RED metrics is that they are exact
// even when you cannot afford to store every trace.
//
// Sampling then reduces what is stored. The decision is a deterministic
// function of the trace ID, so every span belonging to a trace is kept or
// dropped as a unit; sampling spans independently produces traces with holes
// in them, which are worse than no trace at all.
package spans

import (
	"hash/fnv"
	"log"
	"time"

	"github.com/oneagent/agent/internal/aggregate"
	"github.com/oneagent/agent/internal/collector"
)

type Config struct {
	// StatsEnabled turns on RED metrics computed over 100% of spans.
	StatsEnabled bool
	// SamplingEnabled turns on span reduction. With it off every span is
	// forwarded, which is the previous behaviour.
	SamplingEnabled bool
	// Rate is the fraction of ordinary traces kept, in [0,1].
	Rate float64
	// KeepErrors forwards error spans regardless of Rate. Errors are rare and
	// disproportionately what you go looking for, so sampling them at the same
	// rate as successes is almost never what anyone wants.
	KeepErrors bool
	// SlowThresholdMs forwards spans at least this slow regardless of Rate.
	// Zero disables the rule.
	SlowThresholdMs float64

	Interval    time.Duration
	MaxContexts int
	MaxSamples  int
}

const (
	defaultInterval    = 60 * time.Second
	defaultMaxContexts = 2000
	defaultMaxSamples  = 2048

	// statusCodeError is OTLP's STATUS_CODE_ERROR.
	statusCodeError = "2"

	// overflowName labels the bucket used once MaxContexts is reached.
	overflowName = "{overflow}"

	// samplingPrecision is the granularity of the rate comparison. 1/10,000
	// is finer than any rate anyone configures in practice.
	samplingPrecision = 10000
)

type contextKey struct {
	service string
	name    string
	status  string
}

type bucket struct {
	count  int64
	errors int64
	sum    float64
	max    float64
	lat    *aggregate.Reservoir
}

func (b *bucket) observe(durationMs float64, isErr bool) {
	if durationMs > b.max || b.count == 0 {
		b.max = durationMs
	}
	b.count++
	if isErr {
		b.errors++
	}
	b.sum += durationMs
	b.lat.Observe(durationMs)
}

// Processor is used from a single goroutine (the daemon's drain loop) and so
// holds no locks.
type Processor struct {
	cfg     Config
	agentID string

	contexts       map[contextKey]*bucket
	overflow       *bucket
	overflowLogged bool

	kept    int64
	dropped int64
}

func New(agentID string, cfg Config) *Processor {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultInterval
	}
	if cfg.MaxContexts <= 0 {
		cfg.MaxContexts = defaultMaxContexts
	}
	if cfg.MaxSamples <= 0 {
		cfg.MaxSamples = defaultMaxSamples
	}
	return &Processor{
		cfg:      cfg,
		agentID:  agentID,
		contexts: make(map[contextKey]*bucket, 64),
	}
}

func (p *Processor) Interval() time.Duration { return p.cfg.Interval }

// Enabled reports whether the processor does anything at all. When neither
// stats nor sampling is on, the daemon skips it entirely.
func (p *Processor) Enabled() bool { return p.cfg.StatsEnabled || p.cfg.SamplingEnabled }

// Process records a span in the statistics and reports whether it should be
// forwarded. Non-trace envelopes are not this type's business and are always
// forwarded untouched.
func (p *Processor) Process(e collector.Envelope) (forward bool) {
	if e.Kind != collector.KindTrace {
		return true
	}

	isErr := e.Labels["status.code"] == statusCodeError

	if p.cfg.StatsEnabled {
		p.record(e, isErr)
	}

	keep := p.shouldKeep(e, isErr)
	if keep {
		p.kept++
	} else {
		p.dropped++
	}
	return keep
}

func (p *Processor) record(e collector.Envelope, isErr bool) {
	status := "ok"
	if isErr {
		status = "error"
	}
	key := contextKey{
		service: orUnknown(e.Labels["service.name"]),
		name:    orUnknown(e.Labels["name"]),
		status:  status,
	}

	b, ok := p.contexts[key]
	if !ok {
		if len(p.contexts) >= p.cfg.MaxContexts {
			b = p.overflowBucket()
		} else {
			b = &bucket{lat: aggregate.NewReservoir(p.cfg.MaxSamples)}
			p.contexts[key] = b
		}
	}
	// Value carries span duration in milliseconds.
	b.observe(e.Value, isErr)
}

func (p *Processor) overflowBucket() *bucket {
	if p.overflow == nil {
		p.overflow = &bucket{lat: aggregate.NewReservoir(p.cfg.MaxSamples)}
	}
	if !p.overflowLogged {
		p.overflowLogged = true
		log.Printf("spans: reached %d distinct (service, operation, status) combinations in one window; "+
			"further combinations are summarised under name=%q. Span names are supposed to be "+
			"low-cardinality — if this persists, an instrumented service is putting identifiers in them.",
			p.cfg.MaxContexts, overflowName)
	}
	return p.overflow
}

// shouldKeep applies the sampling rules in priority order: the always-keep
// rules first, then the rate.
func (p *Processor) shouldKeep(e collector.Envelope, isErr bool) bool {
	if !p.cfg.SamplingEnabled {
		return true
	}
	if p.cfg.KeepErrors && isErr {
		return true
	}
	if p.cfg.SlowThresholdMs > 0 && e.Value >= p.cfg.SlowThresholdMs {
		return true
	}
	if p.cfg.Rate >= 1 {
		return true
	}
	if p.cfg.Rate <= 0 {
		return false
	}
	return keepByTraceID(e.Labels["trace_id"], p.cfg.Rate)
}

// keepByTraceID makes the sampling decision a pure function of the trace ID,
// so that every span of a trace reaches the same verdict on every host that
// sees it. A per-span random draw would keep a scattering of spans from many
// traces and produce none that are complete.
func keepByTraceID(traceID string, rate float64) bool {
	if traceID == "" {
		// Without an ID there is no way to be consistent, and a partial trace
		// is the thing we are avoiding — so keep it.
		return true
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(traceID))
	return float64(h.Sum64()%samplingPrecision) < rate*samplingPrecision
}

// Flush ends the window, returning the statistics for it plus counters
// describing what sampling did.
func (p *Processor) Flush(now time.Time) []collector.Envelope {
	var out []collector.Envelope

	if p.cfg.StatsEnabled {
		out = make([]collector.Envelope, 0, (len(p.contexts)+1)*6)
		for key, b := range p.contexts {
			out = append(out, p.summarize(key, b, now)...)
		}
		if p.overflow != nil {
			out = append(out, p.summarize(contextKey{service: "*", name: overflowName, status: "*"}, p.overflow, now)...)
		}
		p.contexts = make(map[contextKey]*bucket, len(p.contexts))
		p.overflow = nil
		p.overflowLogged = false
	}

	// Emit what sampling did even when stats are off, so the effective keep
	// rate is observable rather than something you have to infer from backend
	// volume.
	if p.cfg.SamplingEnabled && (p.kept > 0 || p.dropped > 0) {
		out = append(out,
			p.metric("trace.spans.kept", nil, float64(p.kept), now),
			p.metric("trace.spans.dropped", nil, float64(p.dropped), now),
		)
	}
	p.kept, p.dropped = 0, 0

	return out
}

func (p *Processor) summarize(key contextKey, b *bucket, now time.Time) []collector.Envelope {
	if b.count == 0 {
		return nil
	}
	labels := map[string]string{
		"service":   key.service,
		"operation": key.name,
		"status":    key.status,
	}
	sorted := b.lat.Sorted()

	series := []struct {
		name  string
		value float64
	}{
		{"trace.spans", float64(b.count)},
		{"trace.errors", float64(b.errors)},
		{"trace.duration.avg", b.sum / float64(b.count)},
		{"trace.duration.p50", aggregate.Percentile(sorted, 0.50)},
		{"trace.duration.p95", aggregate.Percentile(sorted, 0.95)},
		{"trace.duration.p99", aggregate.Percentile(sorted, 0.99)},
		{"trace.duration.max", b.max},
	}

	out := make([]collector.Envelope, 0, len(series))
	for _, s := range series {
		out = append(out, p.metric(s.name, labels, s.value, now))
	}
	return out
}

// metric builds one metric envelope, giving each its own label map so a later
// mutation by any consumer cannot rewrite its siblings' labels.
func (p *Processor) metric(name string, labels map[string]string, value float64, now time.Time) collector.Envelope {
	var l map[string]string
	if labels != nil {
		l = make(map[string]string, len(labels))
		for k, v := range labels {
			l[k] = v
		}
	}
	return collector.Envelope{
		Kind:      collector.KindMetric,
		AgentID:   p.agentID,
		Source:    name,
		Timestamp: now,
		Labels:    l,
		Value:     value,
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
