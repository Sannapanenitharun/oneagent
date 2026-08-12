// Package collector defines the common telemetry envelope and the Collector
// interface every source (metrics, logs, traces) implements. Normalizing at
// this boundary means the exporter and any downstream consumer never needs
// to know whether a signal came from /proc, a tailed log file, or a pushed
// span — one shape in, one shape out.
package collector

import (
	"context"
	"time"
)

// Kind identifies the telemetry signal type.
type Kind string

const (
	KindMetric  Kind = "metric"
	KindLog     Kind = "log"
	KindTrace   Kind = "trace"
	KindAPICall Kind = "api_call" // a parsed HTTP request from an access log — structured (method/path/status/duration), unlike a raw KindLog line
)

// Envelope is the single normalized shape for every signal the agent emits.
// Fields are deliberately generic (Value as float64, Payload as map) so new
// source types don't require schema changes here.
type Envelope struct {
	Kind      Kind              `json:"kind"`
	AgentID   string            `json:"agent_id"`
	Source    string            `json:"source"` // e.g. "host.cpu", "app.log", "otlp.span"
	Timestamp time.Time         `json:"timestamp"`
	Labels    map[string]string `json:"labels,omitempty"`
	Value     float64           `json:"value"`             // used by metrics, trace duration, api_call latency — deliberately NOT omitempty: a genuine 0.0 reading (e.g. 0% CPU) must be distinguishable from "no value was collected" in the emitted JSON. Dropping it on zero silently corrupted real data — see the fix rationale below.
	Message   string            `json:"message,omitempty"` // used by logs
	Payload   map[string]any    `json:"payload,omitempty"` // used by traces / structured extras
}

// Collector is implemented by every telemetry source. Collect is called on
// the configured interval and should return quickly — long-running work
// (e.g. tailing) should be started in Start and stream into the channel
// rather than blocking Collect.
type Collector interface {
	// Name identifies the collector in logs/errors.
	Name() string

	// Start begins any background work (e.g. a log tailer, an OTLP
	// listener) and streams envelopes into out until ctx is cancelled.
	// Collectors with nothing to start in the background (e.g. a simple
	// polled metric) can implement this as a no-op.
	Start(ctx context.Context, out chan<- Envelope) error

	// Stop releases any resources (open files, listeners). Called once
	// on shutdown after ctx has been cancelled.
	Stop() error
}
