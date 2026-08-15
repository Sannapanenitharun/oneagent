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

// Span is one received trace span.
type Span struct {
	T       int64   `json:"t"`
	TraceID string  `json:"trace_id"`
	SpanID  string  `json:"span_id"`
	Service string  `json:"service"`
	Name    string  `json:"name"`
	DurMs   float64 `json:"dur_ms"`
	Status  string  `json:"status,omitempty"`
}

// Snapshot is the whole view, as served to the browser.
type Snapshot struct {
	AgentID   string            `json:"agent_id"`
	Version   string            `json:"version"`
	StartedAt int64             `json:"started_at"`
	Now       int64             `json:"now"`
	RetainSec int               `json:"retain_sec"`
	Counts    map[string]uint64 `json:"counts"`
	// SeriesDropped counts distinct series refused because the cap was
	// already reached. A non-zero value here means the view is incomplete,
	// and the UI says so rather than quietly showing a subset.
	SeriesDropped uint64    `json:"series_dropped"`
	Series        []Series  `json:"series"`
	Logs          []LogLine `json:"logs"`
	Spans         []Span    `json:"spans"`
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
	nowFn   func() time.Time // injectable for tests
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
			T:       e.Timestamp.UnixMilli(),
			TraceID: e.Labels["trace_id"],
			SpanID:  e.Labels["span_id"],
			Service: e.Labels["service.name"],
			Name:    e.Labels["name"],
			DurMs:   e.Value,
			Status:  e.Labels["status.code"],
		}, maxSpans)
	case collector.KindAPICall:
		// An access-log request is a metric-shaped thing here: its latency
		// over time is the useful view, keyed by method+path+status.
		s.recordMetric(e)
	}
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

// Snapshot returns a deep copy of the current view. Copying matters: the
// caller marshals this to JSON without holding the lock, and sharing the
// backing arrays would race with the drain loop appending to them.
func (s *Store) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := Snapshot{
		AgentID:       s.agentID,
		Version:       s.version,
		StartedAt:     s.startedAt.UnixMilli(),
		Now:           s.nowFn().UnixMilli(),
		RetainSec:     int(s.retain / time.Second),
		Counts:        make(map[string]uint64, len(s.counts)),
		SeriesDropped: s.dropped,
		Series:        make([]Series, 0, len(s.series)),
		Logs:          append([]LogLine(nil), s.logs...),
		Spans:         append([]Span(nil), s.spans...),
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
