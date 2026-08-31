// Package dashboard serves a local, loopback-only web view of what this
// agent is collecting right now.
//
// It exists because every question about a running agent previously
// required either reading its stdout or waiting for the data to appear in
// a remote backend — which is a slow loop when the thing you are debugging
// is whether the agent is collecting at all. This answers that on the host,
// with no backend involved and nothing sent anywhere.
//
// The store is a bounded in-memory tap on the export path, NOT a time
// series database: it holds a short recent window, drops the oldest data,
// and refuses to grow past a fixed series count. An agent must never fall
// over because its own debug view retained too much.
package dashboard

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agent-i/agent/internal/collector"
	"github.com/agent-i/agent/internal/exporter"
)

const (
	defaultRetain    = 15 * time.Minute
	defaultMaxSeries = 500
	maxPointsPerSer  = 900 // retain/interval at a 1s interval — the practical ceiling
	maxLogs          = 300
	maxSpans         = 400
)

// Point is one sample. T is unix milliseconds — the wire format the
// browser's Date already speaks, so the UI needs no conversion.
type Point struct {
	T int64   `json:"t"`
	V float64 `json:"v"`
}

// Series is one metric name + label-set over time.
type Series struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
	// Cumulative marks a monotonic counter, which the UI must differentiate
	// into a rate before plotting. Sourced from the exporter so there is one
	// definition of which metrics are counters.
	Cumulative bool    `json:"cumulative"`
	Points     []Point `json:"points"`
}

// LogLine is one tailed log record.
type LogLine struct {
	T       int64             `json:"t"`
	Source  string            `json:"source"`
	Message string            `json:"message"`
	Labels  map[string]string `json:"labels,omitempty"`
}

// Span is one received trace span. ParentID is what lets a consumer rebuild
// the call tree — depth for a waterfall, stacking for a flame graph, and the
// caller→callee edges of a service map. Empty on a root span.
type Span struct {
	T        int64   `json:"t"`
	TraceID  string  `json:"trace_id"`
	SpanID   string  `json:"span_id"`
	ParentID string  `json:"parent_id,omitempty"`
	Service  string  `json:"service"`
	Name     string  `json:"name"`
	DurMs    float64 `json:"dur_ms"`
	Status   string  `json:"status,omitempty"`
	// Kind is OTLP's span kind — client, server, producer, consumer, internal.
	//
	// It is what makes a service graph derivable rather than guessed. An edge
	// between two services is a client span whose child is a server span;
	// parent-child alone cannot distinguish that from an ordinary nested call,
	// and cannot tell an outbound call to an uninstrumented database from one
	// to a service that happens not to be reporting.
	Kind string `json:"kind,omitempty"`
	// Peer carries the attributes naming what an outbound span was talking to,
	// for the case where that something is not instrumented and so has no span
	// of its own anywhere in the trace. Without it a service's databases,
	// queues and third-party APIs are simply absent from the graph.
	//
	// Omitted when the span named no peer, which is every server span and most
	// internal ones.
	Peer map[string]string `json:"peer,omitempty"`
}

// AdapterContract identifies the derivation semantics this payload is built
// for: which fields exist, what they mean, and what a consumer is expected to
// compute from them rather than read.
//
// The agent computes none of that. Percentiles, health thresholds, counter
// rates and severity are all derived client-side in frontend/src/adapters.js,
// deliberately, so changing one costs a page reload instead of a redeploy to
// every host. That leaves a gap for any second consumer of this API: it
// receives raw series and has no way to know which set of conventions the
// numbers follow.
//
// This is the agent's half of that answer, and it is the only half the agent
// can honestly give. It cannot report the version of a browser-side library it
// has never loaded — that value would have to be hardcoded here and would drift
// the first time only one side changed. So this versions the CONTRACT, and
// adapters.js declares which contract it implements. A mismatch is the signal.
//
// Bump this when the payload's shape or the meaning of a field changes, not
// when a threshold is retuned — a retuned threshold is exactly the kind of
// change the client-side split exists to make cheap.
const AdapterContract = "1"

// Snapshot is the whole view, as served to the browser.
type Snapshot struct {
	AgentID string `json:"agent_id"`
	Version string `json:"version"`
	// AdapterContract is informational. Nothing in the agent branches on it.
	AdapterContract string            `json:"adapter_contract"`
	StartedAt       int64             `json:"started_at"`
	Now             int64             `json:"now"`
	RetainSec       int               `json:"retain_sec"`
	Counts          map[string]uint64 `json:"counts"`
	// SeriesDropped counts distinct series refused because the cap was
	// already reached. A non-zero value here means the view is incomplete,
	// and the UI says so rather than quietly showing a subset.
	SeriesDropped uint64    `json:"series_dropped"`
	Series        []Series  `json:"series"`
	Logs          []LogLine `json:"logs"`
	Spans         []Span    `json:"spans"`
	// ReloadPendingRestart names settings that changed in the most recent
	// reload but could not be applied to a running process. Empty means the
	// running agent matches its config file.
	//
	// It was previously only logged, which put it in the one place you are not
	// looking: the operator edits the config, reload reports success, and the
	// setting silently is not in effect. This is the same surface that already
	// answers "is the agent working", so it is where "is the agent running what
	// you think it is" belongs too.
	ReloadPendingRestart []string `json:"reload_pending_restart"`
	// Host carries attributes describing the machine itself, discovered at
	// startup rather than configured — on EC2 that is the instance id, type,
	// region, availability zone and account.
	//
	// It is separate from AgentID because the two answer different questions.
	// AgentID is the name an operator chose in the config file and is all that
	// exists off a cloud host; this is what the machine actually is. Showing
	// only the former means a dashboard cannot tell you which instance you are
	// looking at, which is exactly what you need when a host misbehaves.
	//
	// Omitted entirely when nothing was discovered, so a non-cloud host does
	// not carry an empty object the UI would have to special-case.
	Host map[string]string `json:"host,omitempty"`
}

type seriesBuf struct {
	name       string
	labels     map[string]string
	cumulative bool
	points     []Point
}

// Store accumulates recent telemetry. Every method is safe for concurrent
// use; Record is called from the daemon's drain loop, so it holds the lock
// only long enough to append and never does I/O.
type Store struct {
	agentID   string
	version   string
	startedAt time.Time
	retain    time.Duration
	maxSeries int

	mu      sync.Mutex
	series  map[string]*seriesBuf
	logs    []LogLine
	spans   []Span
	counts  map[string]uint64
	dropped uint64
	// pendingRestart is written by the daemon goroutine on reload and read by
	// HTTP handlers, so it lives behind the store's existing mutex rather than
	// introducing a lock into the daemon, which owns its state without one.
	pendingRestart []string
	// hostAttrs is written once at startup, before the HTTP server is
	// serving, but read by handler goroutines — kept under the same mutex as
	// everything else rather than reasoned about as a special case.
	hostAttrs map[string]string
	nowFn     func() time.Time // injectable for tests
}

func NewStore(agentID, version string, retain time.Duration, maxSeries int) *Store {
	if retain <= 0 {
		retain = defaultRetain
	}
	if maxSeries <= 0 {
		maxSeries = defaultMaxSeries
	}
	return &Store{
		agentID:   agentID,
		version:   version,
		startedAt: time.Now().UTC(),
		retain:    retain,
		maxSeries: maxSeries,
		series:    make(map[string]*seriesBuf),
		counts:    make(map[string]uint64),
		nowFn:     func() time.Time { return time.Now().UTC() },
	}
}

// SetHostAttributes records what the machine is, as discovered at startup.
//
// A setter rather than a NewStore parameter because detection is optional and
// can fail: threading it through the constructor would put a "may be nil" map
// in every caller, including the several tests that have no interest in it.
func (s *Store) SetHostAttributes(attrs map[string]string) {
	if len(attrs) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hostAttrs = make(map[string]string, len(attrs))
	for k, v := range attrs {
		s.hostAttrs[k] = v
	}
}

// SetPendingRestart records which settings the most recent reload could not
// apply live. Called from the daemon goroutine; replaces the previous set
// rather than accumulating, so a later clean reload clears it and the field
// always describes the latest attempt.
//
// The slice is copied because the caller built it and may reuse the backing
// array; sharing it would let the daemon mutate what a handler is encoding.
func (s *Store) SetPendingRestart(names []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingRestart = append(make([]string, 0, len(names)), names...)
}

// Record files one envelope into the view. It never returns an error and
// never blocks: this sits on the hot path between collection and export,
// and a debug surface must not be able to stall real telemetry.
func (s *Store) Record(e collector.Envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.counts[string(e.Kind)]++

	switch e.Kind {
	case collector.KindMetric:
		s.recordMetric(e)
	case collector.KindLog:
		s.logs = appendCapped(s.logs, LogLine{
			T:       e.Timestamp.UnixMilli(),
			Source:  e.Source,
			Message: e.Message,
			Labels:  publicLabels(e.Labels),
		}, maxLogs)
	case collector.KindTrace:
		s.spans = appendCapped(s.spans, Span{
			T:        e.Timestamp.UnixMilli(),
			TraceID:  e.Labels["trace_id"],
			SpanID:   e.Labels["span_id"],
			ParentID: e.Labels["parent_span_id"],
			Service:  e.Labels["service.name"],
			Name:     e.Labels["name"],
			DurMs:    e.Value,
			Status:   e.Labels["status.code"],
			Kind:     e.Labels["span.kind"],
			Peer:     peerAttributes(e.Labels),
		}, maxSpans)
	case collector.KindAPICall:
		// An access-log request is a metric-shaped thing here: its latency
		// over time is the useful view, keyed by method+path+status.
		s.recordMetric(e)
	}
}

// peerAttributeKeys mirrors the receiver's list. Kept as an explicit set
// rather than "every label that is not one of ours" so that a new label added
// elsewhere cannot silently start appearing on every span in the payload.
var peerAttributeKeys = []string{
	"peer.service",
	"db.system", "db.system.name", "db.name", "db.namespace",
	"messaging.system", "messaging.destination.name", "messaging.destination",
	"rpc.system", "rpc.service",
	"server.address", "net.peer.name",
}

// peerAttributes extracts what an outbound span named as its peer, returning
// nil when it named none — which is most spans, and nil keeps the field out of
// the JSON entirely rather than sending an empty object per span.
func peerAttributes(labels map[string]string) map[string]string {
	var out map[string]string
	for _, k := range peerAttributeKeys {
		if v := labels[k]; v != "" {
			if out == nil {
				out = make(map[string]string, 2)
			}
			out[k] = v
		}
	}
	return out
}

func (s *Store) recordMetric(e collector.Envelope) {
	key := seriesKey(e.Source, e.Labels)
	buf, ok := s.series[key]
	if !ok {
		if len(s.series) >= s.maxSeries {
			s.dropped++
			return
		}
		buf = &seriesBuf{
			name:       e.Source,
			labels:     publicLabels(e.Labels),
			cumulative: exporter.IsCumulative(e.Source),
		}
		s.series[key] = buf
	}
	buf.points = append(buf.points, Point{T: e.Timestamp.UnixMilli(), V: e.Value})
	s.trim(buf)
}

// trim drops points that have aged out of the retention window, and hard-caps
// the slice so a collector emitting far faster than expected cannot grow one
// series without bound between windows.
func (s *Store) trim(buf *seriesBuf) {
	cutoff := s.nowFn().Add(-s.retain).UnixMilli()
	i := 0
	for i < len(buf.points) && buf.points[i].T < cutoff {
		i++
	}
	if i > 0 {
		buf.points = append(buf.points[:0], buf.points[i:]...)
	}
	if len(buf.points) > maxPointsPerSer {
		buf.points = append(buf.points[:0], buf.points[len(buf.points)-maxPointsPerSer:]...)
	}
}

// prune enforces the retention window across everything the store holds.
//
// Two reasons it exists rather than trimming only on write. First, logs and
// spans were previously bounded by COUNT alone: on a host with light trace
// traffic a span sat in the view until maxSpans newer ones displaced it, so a
// snapshot advertising retain_sec=900 could serve a span from hours ago and
// the UI would present it as current. Second, per-series trimming only ran
// when that series received a sample — so a collector going quiet froze its
// last points in place forever, which reads as "steady" when the truth is
// "stopped", the single most misleading thing a monitoring view can do.
//
// Dropping series that empty also releases their slot against maxSeries.
// Without that, a host whose containers come and go exhausts the cap with
// series that no longer exist and starts refusing live ones.
//
// Caller must hold s.mu.
func (s *Store) prune(now time.Time) {
	cutoff := now.Add(-s.retain).UnixMilli()

	for key, buf := range s.series {
		s.trim(buf)
		if len(buf.points) == 0 {
			delete(s.series, key)
		}
	}
	s.logs = dropBefore(s.logs, cutoff, func(l LogLine) int64 { return l.T })
	s.spans = dropBefore(s.spans, cutoff, func(sp Span) int64 { return sp.T })
}

// dropBefore removes entries older than cutoff, filtering rather than seeking
// the first survivor: these buffers are appended from batches that can carry
// slightly out-of-order timestamps, and a scan that stops at the first
// in-window entry would keep every older one behind it.
func dropBefore[T any](buf []T, cutoff int64, at func(T) int64) []T {
	keep := buf[:0]
	for _, v := range buf {
		if at(v) >= cutoff {
			keep = append(keep, v)
		}
	}
	// Release the tail so dropped entries are not pinned by the backing array.
	var zero T
	for i := len(keep); i < len(buf); i++ {
		buf[i] = zero
	}
	return keep
}

// Snapshot returns a deep copy of the current view. Copying matters: the
// caller marshals this to JSON without holding the lock, and sharing the
// backing arrays would race with the drain loop appending to them.
//
// It prunes before copying, so the window is enforced on read. That is what
// makes the guarantee hold on an agent that has gone quiet: pruning only on
// write means no writes, no pruning, and a view that keeps serving whatever
// it last saw. The cost is that Snapshot mutates.
func (s *Store) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.nowFn()
	s.prune(now)

	out := Snapshot{
		AgentID:         s.agentID,
		Version:         s.version,
		AdapterContract: AdapterContract,
		StartedAt:       s.startedAt.UnixMilli(),
		Now:             now.UnixMilli(),
		RetainSec:       int(s.retain / time.Second),
		Counts:          make(map[string]uint64, len(s.counts)),
		SeriesDropped:   s.dropped,
		Host:            copyAttrs(s.hostAttrs),
		Series:          make([]Series, 0, len(s.series)),
		// make, not append to a nil slice: appending nothing to nil yields nil,
		// which marshals as JSON null rather than []. Series was already built
		// with make and so always encoded as an array, leaving the payload
		// inconsistent — a consumer reading .spans.length got a value on a busy
		// agent and a TypeError on a quiet one. Empty is a list with no members,
		// not the absence of a list.
		Logs:                 append(make([]LogLine, 0, len(s.logs)), s.logs...),
		Spans:                append(make([]Span, 0, len(s.spans)), s.spans...),
		ReloadPendingRestart: append(make([]string, 0, len(s.pendingRestart)), s.pendingRestart...),
	}
	for k, v := range s.counts {
		out.Counts[k] = v
	}
	for _, buf := range s.series {
		out.Series = append(out.Series, Series{
			Name:       buf.name,
			Labels:     buf.labels,
			Cumulative: buf.cumulative,
			Points:     append([]Point(nil), buf.points...),
		})
	}
	// Stable order so the UI's panels and colors don't reshuffle between
	// polls — a chart whose series swap identity every 5 seconds is unreadable.
	sort.Slice(out.Series, func(i, j int) bool {
		if out.Series[i].Name != out.Series[j].Name {
			return out.Series[i].Name < out.Series[j].Name
		}
		return labelString(out.Series[i].Labels) < labelString(out.Series[j].Labels)
	})
	return out
}

// publicLabels strips the internal underscore-prefixed labels (e.g.
// _boot_time_unix) the exporter consumes and removes — they are plumbing,
// not something to show in a UI.
func publicLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if strings.HasPrefix(k, "_") {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func seriesKey(name string, labels map[string]string) string {
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte(0)
	b.WriteString(labelString(publicLabels(labels)))
	return b.String()
}

// labelString renders a label set deterministically. Map iteration order in
// Go is randomized, so without sorting the same series would produce a
// different key on each sample and fragment into many.
func labelString(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
	}
	return b.String()
}

func appendCapped[T any](buf []T, v T, max int) []T {
	buf = append(buf, v)
	if len(buf) > max {
		buf = append(buf[:0], buf[len(buf)-max:]...)
	}
	return buf
}

// copyAttrs returns a copy so a caller holding a Snapshot cannot mutate store
// state, and nil for an empty map so the field is omitted from the JSON.
func copyAttrs(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
