package exporter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agent-i/agent/internal/collector"
	"github.com/agent-i/agent/internal/config"
	"github.com/agent-i/agent/internal/version"
)

// This file exists because an OTLP-native backend does NOT understand our
// internal Envelope JSON — it speaks OTLP, which
// has three distinct wire shapes (metrics/traces/logs), each with its own
// endpoint (/v1/metrics, /v1/traces, /v1/logs) and schema. The plain
// httpExporter in exporter.go sends our own format and would arrive at
// the backend as bytes it can't parse. This exporter does the real conversion:
// one Envelope in, the correct OTLP shape out, routed to the matching
// endpoint. Uses OTLP's JSON encoding (see traces.go's receiver-side
// notes on why JSON rather than binary protobuf — same dependency
// constraint applies here).

// --- OTLP JSON wire types (kept local to this file; the receiver in
// collector/traces.go has its own copies for the inbound direction —
// small duplication across an inbound/outbound boundary is clearer than
// a shared package neither side fully owns). ---

type otlpKeyValue struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

type otlpAnyValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
}

func stringAttr(k, v string) otlpKeyValue {
	return otlpKeyValue{Key: k, Value: otlpAnyValue{StringValue: &v}}
}

type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes"`
}

type otlpScope struct {
	Name string `json:"name"`
}

// --- metrics ---

type otlpMetricsRequest struct {
	ResourceMetrics []otlpResourceMetrics `json:"resourceMetrics"`
}
type otlpResourceMetrics struct {
	Resource     otlpResource       `json:"resource"`
	ScopeMetrics []otlpScopeMetrics `json:"scopeMetrics"`
}
type otlpScopeMetrics struct {
	Scope   otlpScope    `json:"scope"`
	Metrics []otlpMetric `json:"metrics"`
}
type otlpMetric struct {
	Name                 string                    `json:"name"`
	Gauge                *otlpGauge                `json:"gauge,omitempty"`
	Sum                  *otlpSum                  `json:"sum,omitempty"`
	ExponentialHistogram *otlpExponentialHistogram `json:"exponentialHistogram,omitempty"`
}
type otlpGauge struct {
	DataPoints []otlpNumberDataPoint `json:"dataPoints"`
}

// otlpSum is required for system.cpu.time specifically — OTel defines it
// as a monotonic cumulative counter (seconds spent in each CPU state
// since boot), not a point-in-time value, and a backend's host-inventory
// view looks for this metric in Sum form. Sending it as a
// Gauge would be structurally wrong, not just a labeling difference.
type otlpSum struct {
	DataPoints             []otlpNumberDataPoint `json:"dataPoints"`
	AggregationTemporality int                   `json:"aggregationTemporality"` // 2 = AGGREGATION_TEMPORALITY_CUMULATIVE, per OTLP proto
	IsMonotonic            bool                  `json:"isMonotonic"`
}
type otlpNumberDataPoint struct {
	StartTimeUnixNano string         `json:"startTimeUnixNano,omitempty"` // required for Sum points — omitted for Gauge points
	TimeUnixNano      string         `json:"timeUnixNano"`
	AsDouble          float64        `json:"asDouble"`
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
}

// otlpExponentialHistogram carries whole distributions rather than
// pre-computed percentiles.
//
// Temporality is DELTA (1), not CUMULATIVE: each point describes exactly one
// flush window and the agent keeps no running total. That is what lets a
// backend add windows together to answer for an hour, or add hosts together to
// answer for a fleet — neither of which is possible from a p99, because
// percentiles do not compose.
type otlpExponentialHistogram struct {
	DataPoints             []otlpExpHistogramDataPoint `json:"dataPoints"`
	AggregationTemporality int                         `json:"aggregationTemporality"` // 1 = DELTA, per OTLP proto
}

type otlpExpHistogramDataPoint struct {
	StartTimeUnixNano string         `json:"startTimeUnixNano,omitempty"`
	TimeUnixNano      string         `json:"timeUnixNano"`
	Count             uint64         `json:"count"`
	Sum               float64        `json:"sum"`
	Scale             int32          `json:"scale"`
	ZeroCount         uint64         `json:"zeroCount"`
	Positive          otlpExpBuckets `json:"positive"`
	Min               float64        `json:"min"`
	Max               float64        `json:"max"`
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
}

// otlpExpBuckets is one side of the histogram. Only the positive side is ever
// populated: everything measured here is a duration or a size.
type otlpExpBuckets struct {
	Offset       int32    `json:"offset"`
	BucketCounts []uint64 `json:"bucketCounts"`
}

// --- traces ---

type otlpTracesRequest struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}
type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}
type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}
type otlpSpan struct {
	TraceID string `json:"traceId"`
	SpanID  string `json:"spanId"`
	// omitempty matters: OTLP identifies the root span by the ABSENCE of this
	// field, so emitting an empty string would make every span look parented.
	ParentSpanID      string         `json:"parentSpanId,omitempty"`
	Name              string         `json:"name"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	EndTimeUnixNano   string         `json:"endTimeUnixNano"`
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
}

// --- logs ---

type otlpLogsRequest struct {
	ResourceLogs []otlpResourceLogs `json:"resourceLogs"`
}
type otlpResourceLogs struct {
	Resource  otlpResource    `json:"resource"`
	ScopeLogs []otlpScopeLogs `json:"scopeLogs"`
}
type otlpScopeLogs struct {
	Scope      otlpScope       `json:"scope"`
	LogRecords []otlpLogRecord `json:"logRecords"`
}
type otlpLogRecord struct {
	TimeUnixNano string         `json:"timeUnixNano"`
	Body         otlpAnyValue   `json:"body"`
	Attributes   []otlpKeyValue `json:"attributes,omitempty"`
}

// otlpHTTPExporter buffers envelopes by kind (metrics/traces need
// different OTLP shapes than logs, and api_call envelopes are exported
// as logs with structured attributes since OTLP has no dedicated
// "HTTP request" signal type) and flushes each buffer to its matching
// OTLP endpoint. Batching/gzip/retry logic mirrors httpExporter in
// exporter.go — same rationale, kept as a separate implementation here
// because the payload-building step is fundamentally different per kind.
type otlpHTTPExporter struct {
	baseURL       string
	agentIDLabel  string // set from the first envelope's AgentID; OTLP resource attributes need a value, and Envelope doesn't carry one until Export is called
	headers       map[string]string
	client        *http.Client
	batchSize     int
	maxBatchBytes int
	flushInterval time.Duration
	maxRetries    int

	mu         sync.Mutex
	metricsBuf []collector.Envelope
	tracesBuf  []collector.Envelope
	logsBuf    []collector.Envelope
	// Running size estimates for the buffers above, maintained on append so a
	// flush decision costs nothing. Reset together with the buffers.
	metricsBytes int
	tracesBytes  int
	logsBytes    int
	stopCh       chan struct{}
	flushWg      sync.WaitGroup

	hostIDOnce sync.Once
	hostIDVal  string

	// resourceAttrs are runtime-discovered attributes describing the host: the
	// OS description, and on EC2 the instance identity.
	//
	// Held behind an atomic pointer because they are not necessarily final at
	// startup. IMDS can be unreachable for the first seconds of a boot — the
	// agent may well start before the network stack is ready — and a host that
	// lost that race used to report no instance identity for the entire life
	// of the process, until somebody noticed and restarted it. The daemon
	// re-probes in the background and publishes the result here.
	//
	// The map itself is never mutated after being stored; a refresh swaps in a
	// new one, so readers need no lock.
	resourceAttrs atomic.Pointer[map[string]string]
}

// SetResourceAttributes publishes a refined set of host attributes. It is safe
// to call while exports are in flight.
func (o *otlpHTTPExporter) SetResourceAttributes(attrs map[string]string) {
	copied := make(map[string]string, len(attrs))
	for k, v := range attrs {
		copied[k] = v
	}
	o.resourceAttrs.Store(&copied)
}

func newOTLPHTTPExporter(cfg config.ExporterConfig) (*otlpHTTPExporter, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("exporter type 'otlp_http' requires 'endpoint' to be set (the OTLP receiver base URL, e.g. http://host:4318)")
	}
	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = 100
	}
	flushInterval := cfg.FlushInterval
	if flushInterval <= 0 {
		flushInterval = 5 * time.Second
	}
	maxRetries := cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}

	o := &otlpHTTPExporter{
		baseURL:       cfg.Endpoint,
		headers:       cfg.Headers,
		client:        &http.Client{Timeout: 10 * time.Second},
		batchSize:     batchSize,
		maxBatchBytes: resolveMaxBatchBytes(cfg.MaxBatchBytes),
		flushInterval: flushInterval,
		maxRetries:    maxRetries,
		stopCh:        make(chan struct{}),
	}
	o.SetResourceAttributes(cfg.ResourceAttributes)
	o.flushWg.Add(1)
	go o.flushLoop()
	return o, nil
}

func (o *otlpHTTPExporter) flushLoop() {
	defer o.flushWg.Done()
	ticker := time.NewTicker(o.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-o.stopCh:
			return
		case <-ticker.C:
			_ = o.flushAll()
		}
	}
}

func (o *otlpHTTPExporter) Export(e collector.Envelope) error {
	o.mu.Lock()
	if o.agentIDLabel == "" {
		o.agentIDLabel = e.AgentID
	}
	// A batch is full on EITHER limit. Count bounds how many records a backend
	// has to parse; bytes bound how large the request is. Neither implies the
	// other, and only the second one is fatal when exceeded — an oversized
	// request is rejected outright and retrying cannot help.
	//
	// A single envelope larger than the cap still flushes here, alone, rather
	// than being dropped: one record cannot be split, and sending it is the
	// only way it can possibly arrive.
	size := envelopeBytes(e)
	var full bool
	switch e.Kind {
	case collector.KindMetric, collector.KindHistogram:
		// A distribution is a metric — it travels in the same OTLP request and
		// differs only in which data-point shape it produces.
		o.metricsBuf = append(o.metricsBuf, e)
		o.metricsBytes += size
		full = len(o.metricsBuf) >= o.batchSize || o.metricsBytes >= o.maxBatchBytes
	case collector.KindTrace:
		o.tracesBuf = append(o.tracesBuf, e)
		o.tracesBytes += size
		full = len(o.tracesBuf) >= o.batchSize || o.tracesBytes >= o.maxBatchBytes
	default: // KindLog, KindAPICall — both exported as OTLP logs
		o.logsBuf = append(o.logsBuf, e)
		o.logsBytes += size
		full = len(o.logsBuf) >= o.batchSize || o.logsBytes >= o.maxBatchBytes
	}
	o.mu.Unlock()

	if full {
		return o.flushAll()
	}
	return nil
}

func (o *otlpHTTPExporter) flushAll() error {
	o.mu.Lock()
	metrics, traces, logs := o.metricsBuf, o.tracesBuf, o.logsBuf
	o.metricsBuf, o.tracesBuf, o.logsBuf = nil, nil, nil
	o.metricsBytes, o.tracesBytes, o.logsBytes = 0, 0, 0
	o.mu.Unlock()

	var errs []error
	if len(metrics) > 0 {
		if err := o.sendMetrics(metrics); err != nil {
			errs = append(errs, err)
		}
	}
	if len(traces) > 0 {
		if err := o.sendTraces(traces); err != nil {
			errs = append(errs, err)
		}
	}
	if len(logs) > 0 {
		if err := o.sendLogs(logs); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("otlp export: %d of %d signal batches failed: %v", len(errs), boolCount(len(metrics) > 0)+boolCount(len(traces) > 0)+boolCount(len(logs) > 0), errs)
	}
	return nil
}

func boolCount(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (o *otlpHTTPExporter) hostName() string {
	name := o.agentIDLabel
	if name == "" {
		name = "agent-i"
	}
	return name
}

// serviceNameFor returns the service.name to report for one envelope.
// Envelopes received from an externally instrumented app (via our OTLP
// trace receiver) carry their own real service.name as a label — e.g. a
// span from "certi-backend" arrives already tagged with that identity.
// That must be preserved rather than overwritten with the agent's own
// identity when re-exporting, or every distinct app on a host collapses
// into one indistinguishable stream in the backend (this was a real bug,
// caught from a live host: production spans from an app called
// certi-backend were showing up in the backend labeled service.name=host-001,
// the agent's own ID, instead of the app that actually produced them).
// Envelopes Agent-I generates itself (host metrics, tailed logs) carry
// no such label, so those correctly fall back to the agent's identity.
func (o *otlpHTTPExporter) serviceNameFor(e collector.Envelope) string {
	if v, ok := e.Labels["service.name"]; ok && v != "" {
		return v
	}
	return o.hostName()
}

func (o *otlpHTTPExporter) resourceFor(serviceName string) otlpResource {
	// host.name always identifies the physical machine this agent runs
	// on, regardless of which service produced a given signal — unlike
	// service.name, it should never vary per-envelope.
	attrs := []otlpKeyValue{
		stringAttr("service.name", serviceName),
	}
	// Loaded once per resource so every attribute below sees the same
	// snapshot. Reading the pointer per lookup would let a refresh land
	// mid-build and produce a resource that is half old and half new.
	ra := *o.resourceAttrs.Load()

	// host.name is emitted here, immediately after service.name, only when
	// discovery did not supply one. For a while it was emitted unconditionally
	// while discovery also supplied it from the EC2 Name tag, so the resource
	// carried the attribute twice with two different values — and OTLP does
	// not define which of a duplicated pair a backend keeps, so the host's
	// name depended on the reader. The discovered one wins, for the same
	// reason it does for host.id below: the Name tag is what the instance is
	// actually called, while this is the agent's own configured id.
	if _, discovered := ra["host.name"]; !discovered {
		attrs = append(attrs, stringAttr("host.name", o.hostName()))
	}

	// os.type is what a backend's host-inventory view reads to populate its
	// "OS Type" filter. Without it every host this agent reports lands in an
	// unnamed, unselectable bucket in that facet.
	//
	// Emitted here only when detection did not supply it. runtime.GOOS is a
	// build constant, so it can say which kernel family the binary was
	// compiled for and nothing more — it cannot distinguish two hosts, which
	// is what a fleet view exists to do. The detected value carries the
	// distribution and version alongside it; this remains as the floor for a
	// host that could not describe itself. Same precedence as host.name above,
	// and for the same reason: a duplicated attribute has no defined winner in
	// OTLP, so the value would depend on the reader.
	if _, discovered := ra["os.type"]; !discovered {
		attrs = append(attrs, stringAttr("os.type", runtime.GOOS))
	}

	attrs = append(attrs,
		// Which build produced this telemetry. Deliberately NOT
		// service.version: on spans forwarded from an externally
		// instrumented app, service.name is that app's identity, and
		// pairing it with the agent's version would claim the app is
		// running a version it isn't. telemetry.distro.* describes the
		// thing doing the collecting, which is exactly what this is, and
		// so stays correct on every resource regardless of whose signal
		// it carries.
		stringAttr("telemetry.distro.name", "agent-i"),
		stringAttr("telemetry.distro.version", version.Version),
	)
	// host.id is recommended (not required) by the OTel host resource
	// Monitoring as a fallback identifier when hostnames collide (cloned
	// VMs, ephemeral instances reusing a name) — included when available,
	// omitted rather than faked when it isn't.
	//
	// A discovered host.id takes precedence: on EC2 the semantic conventions
	// define host.id as the instance id, and that is what links this telemetry
	// to the instance in a backend. machine-id remains the fallback everywhere
	// else.
	if _, discovered := ra["host.id"]; !discovered {
		if id := o.hostID(); id != "" {
			attrs = append(attrs, stringAttr("host.id", id))
		}
	}

	// Emitted in sorted order so a resource is byte-identical between flushes
	// and between processes, which keeps diffs and tests stable.
	if len(ra) > 0 {
		keys := make([]string, 0, len(ra))
		for k := range ra {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			attrs = append(attrs, stringAttr(k, ra[k]))
		}
	}
	return otlpResource{Attributes: attrs}
}

// hostID reads /etc/machine-id once and caches it — a stable, unique
// identifier for this specific machine (distinct from host.name, which
// is just a label and could theoretically collide). Returns "" (and the
// attribute is simply omitted) if unavailable rather than fabricating a
// value — a missing host.id degrades gracefully since it's recommended,
// not required.
func (o *otlpHTTPExporter) hostID() string {
	o.hostIDOnce.Do(func() {
		b, err := os.ReadFile("/etc/machine-id")
		if err != nil {
			return
		}
		o.hostIDVal = strings.TrimSpace(string(b))
	})
	return o.hostIDVal
}

func envelopeAttrs(e collector.Envelope) []otlpKeyValue {
	attrs := make([]otlpKeyValue, 0, len(e.Labels))
	for k, v := range e.Labels {
		if strings.HasPrefix(k, "_") {
			continue // internal-use label (e.g. _boot_time_unix) — not a real OTLP attribute, consumed elsewhere
		}
		attrs = append(attrs, stringAttr(k, v))
	}
	return attrs
}

// cumulativeMetrics are the OTel-defined monotonic cumulative counters
// (seconds/bytes/count since boot), which must be sent as Sum rather than
// Gauge. system.disk.pending_operations and system.network.connections are
// deliberately absent: both are point-in-time gauges — a queue depth and a
// connection count can go down as well as up — not cumulative counters.
// IsCumulative reports whether a metric name is a monotonic cumulative
// counter rather than a point-in-time gauge. Exported because consumers
// outside this package need the same answer — the local dashboard has to
// know whether to plot a series raw or differentiate it into a rate, and
// a second hand-maintained copy of this list would drift the moment a
// metric is added on one side only.
func IsCumulative(metricName string) bool { return cumulativeMetrics[metricName] }

var cumulativeMetrics = map[string]bool{
	"system.cpu.time":            true,
	"system.network.io":          true,
	"system.network.packets":     true,
	"system.network.errors":      true,
	"system.network.dropped":     true,
	"system.disk.io":             true,
	"system.disk.operations":     true,
	"system.disk.operation_time": true,
}

// startTimeFor returns the cumulative counter's start time, which OTel requires
// a Sum to declare. The collector smuggles boot time through an internal label
// (see infra_hostmetrics.go); absent it, the field is omitted rather than
// guessed.
func startTimeFor(e collector.Envelope) string {
	bootUnix, ok := e.Labels["_boot_time_unix"]
	if !ok {
		return ""
	}
	secs, err := strconv.ParseInt(bootUnix, 10, 64)
	if err != nil {
		return ""
	}
	return strconv.FormatInt(secs*1e9, 10)
}

// expHistogramPoint converts an envelope's carried distribution into an OTLP
// data point. An envelope tagged as a histogram but carrying nothing usable is
// skipped rather than emitted as an empty distribution, which would read as
// "zero requests" instead of "no data".
func expHistogramPoint(e collector.Envelope) (otlpExpHistogramDataPoint, bool) {
	raw, ok := e.Payload[collector.HistogramPointKey]
	if !ok {
		return otlpExpHistogramDataPoint{}, false
	}
	h, ok := raw.(collector.HistogramPoint)
	if !ok || h.Count == 0 {
		return otlpExpHistogramDataPoint{}, false
	}
	return otlpExpHistogramDataPoint{
		TimeUnixNano: strconv.FormatInt(e.Timestamp.UnixNano(), 10),
		Count:        h.Count,
		Sum:          h.Sum,
		Scale:        h.Scale,
		ZeroCount:    h.ZeroCount,
		Min:          h.Min,
		Max:          h.Max,
		Positive: otlpExpBuckets{
			Offset: h.Offset,
			// make, not a nil slice: an absent bucket list and an empty one
			// marshal differently, and null is not a valid bucketCounts.
			BucketCounts: append(make([]uint64, 0, len(h.BucketCounts)), h.BucketCounts...),
		},
		Attributes: envelopeAttrs(e),
	}, true
}

func (o *otlpHTTPExporter) sendMetrics(envs []collector.Envelope) error {
	// Group data points under one metric entry per name instead of emitting a
	// separate metric object per envelope. A host sampling ~70 series repeated
	// each metric name once per point in every payload; OTLP's model is one
	// metric carrying many points, and consumers are entitled to expect that
	// shape. Insertion order is preserved so payloads stay diffable.
	order := make([]string, 0, 16)
	byName := make(map[string]*otlpMetric, 16)

	for _, e := range envs {
		if e.Kind == collector.KindHistogram {
			m, ok := byName[e.Source]
			if !ok {
				m = &otlpMetric{
					Name:                 e.Source,
					ExponentialHistogram: &otlpExponentialHistogram{AggregationTemporality: 1}, // DELTA
				}
				byName[e.Source] = m
				order = append(order, e.Source)
			}
			if m.ExponentialHistogram == nil {
				// A name used for both a scalar and a distribution would produce
				// a metric that is two things at once, which no backend accepts.
				// Skipping the point and saying so beats emitting a payload that
				// is rejected wholesale, taking every other metric with it.
				log.Printf("otlp: metric %q was emitted as both a scalar and a histogram — dropping the histogram point", e.Source)
				continue
			}
			if dp, ok := expHistogramPoint(e); ok {
				m.ExponentialHistogram.DataPoints = append(m.ExponentialHistogram.DataPoints, dp)
			}
			continue
		}

		cumulative := cumulativeMetrics[e.Source]

		m, ok := byName[e.Source]
		if !ok {
			m = &otlpMetric{Name: e.Source}
			if cumulative {
				m.Sum = &otlpSum{
					AggregationTemporality: 2, // CUMULATIVE
					IsMonotonic:            true,
				}
			} else {
				m.Gauge = &otlpGauge{}
			}
			byName[e.Source] = m
			order = append(order, e.Source)
		}

		dp := otlpNumberDataPoint{
			TimeUnixNano: strconv.FormatInt(e.Timestamp.UnixNano(), 10),
			AsDouble:     e.Value,
			Attributes:   envelopeAttrs(e),
		}
		if cumulative {
			dp.StartTimeUnixNano = startTimeFor(e)
			m.Sum.DataPoints = append(m.Sum.DataPoints, dp)
		} else {
			m.Gauge.DataPoints = append(m.Gauge.DataPoints, dp)
		}
	}

	points := make([]otlpMetric, 0, len(order))
	for _, name := range order {
		points = append(points, *byName[name])
	}

	req := otlpMetricsRequest{ResourceMetrics: []otlpResourceMetrics{{
		Resource:     o.resourceFor(o.hostName()),
		ScopeMetrics: []otlpScopeMetrics{{Scope: otlpScope{Name: "agent-i"}, Metrics: points}},
	}}}
	return o.postJSON("/v1/metrics", req, len(envs))
}

func (o *otlpHTTPExporter) sendTraces(envs []collector.Envelope) error {
	// Group spans by their real origin service before building the OTLP
	// request — each distinct service gets its own resourceSpans entry
	// with the correct service.name, rather than one shared resource
	// block that would silently attribute every app's spans to the
	// agent itself. See serviceNameFor's comment for why this matters.
	order := make([]string, 0, 4)
	groups := make(map[string][]collector.Envelope, 4)
	for _, e := range envs {
		sn := o.serviceNameFor(e)
		if _, seen := groups[sn]; !seen {
			order = append(order, sn)
		}
		groups[sn] = append(groups[sn], e)
	}

	resourceSpansList := make([]otlpResourceSpans, 0, len(order))
	for _, sn := range order {
		group := groups[sn]
		spans := make([]otlpSpan, 0, len(group))
		for _, e := range group {
			startNano := e.Timestamp.UnixNano()
			endNano := startNano + int64(e.Value*1e6) // Value is duration in ms
			spans = append(spans, otlpSpan{
				TraceID: e.Labels["trace_id"],
				SpanID:  e.Labels["span_id"],
				// Set as a first-class OTLP field, not just an attribute: this
				// is what lets a backend reconstruct the call tree. Omitted on
				// a root span, which is how OTLP marks the root.
				ParentSpanID:      e.Labels["parent_span_id"],
				Name:              e.Labels["name"],
				StartTimeUnixNano: strconv.FormatInt(startNano, 10),
				EndTimeUnixNano:   strconv.FormatInt(endNano, 10),
				Attributes:        envelopeAttrs(e),
			})
		}
		resourceSpansList = append(resourceSpansList, otlpResourceSpans{
			Resource:   o.resourceFor(sn),
			ScopeSpans: []otlpScopeSpans{{Scope: otlpScope{Name: "agent-i"}, Spans: spans}},
		})
	}

	req := otlpTracesRequest{ResourceSpans: resourceSpansList}
	return o.postJSON("/v1/traces", req, len(envs))
}

func (o *otlpHTTPExporter) sendLogs(envs []collector.Envelope) error {
	records := make([]otlpLogRecord, 0, len(envs))
	for _, e := range envs {
		body := e.Message
		if body == "" && e.Kind == collector.KindAPICall {
			// api_call envelopes carry structured fields in Labels/Value
			// rather than a Message — build a readable summary line so
			// the log still shows something meaningful in the backend's log
			// view, with the structured data available as attributes.
			body = fmt.Sprintf("%s %s -> %s (%.1fms)", e.Labels["method"], e.Labels["path"], e.Labels["status"], e.Value)
		}
		records = append(records, otlpLogRecord{
			TimeUnixNano: strconv.FormatInt(e.Timestamp.UnixNano(), 10),
			Body:         otlpAnyValue{StringValue: &body},
			Attributes:   envelopeAttrs(e),
		})
	}
	req := otlpLogsRequest{ResourceLogs: []otlpResourceLogs{{
		Resource:  o.resourceFor(o.hostName()),
		ScopeLogs: []otlpScopeLogs{{Scope: otlpScope{Name: "agent-i"}, LogRecords: records}},
	}}}
	return o.postJSON("/v1/logs", req, len(envs))
}

func (o *otlpHTTPExporter) postJSON(path string, payload any, count int) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling OTLP payload for %s: %w", path, err)
	}
	compressed, err := gzipCompress(body)
	if err != nil {
		return fmt.Errorf("compressing OTLP payload for %s: %w", path, err)
	}

	var lastErr error
	for attempt := 0; attempt <= o.maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * 200 * time.Millisecond
			jitter := time.Duration(rand.Int63n(int64(backoff)/2 + 1))
			time.Sleep(backoff + jitter)
		}

		req, err := http.NewRequest(http.MethodPost, o.baseURL+path, bytes.NewReader(compressed))
		if err != nil {
			return fmt.Errorf("building request for %s: %w", path, err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Content-Encoding", "gzip")
		for k, v := range o.headers {
			req.Header.Set(k, v)
		}

		resp, err := o.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("posting to %s (attempt %d/%d, %d signals): %w", path, attempt+1, o.maxRetries+1, count, err)
			continue
		}
		func() {
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
		}()

		if resp.StatusCode < 300 {
			return nil
		}
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return fmt.Errorf("otlp http %d from %s (%d signals, not retrying — client error)", resp.StatusCode, path, count)
		}
		lastErr = fmt.Errorf("otlp http %d from %s (attempt %d/%d)", resp.StatusCode, path, attempt+1, o.maxRetries+1)
	}
	return lastErr
}

func (o *otlpHTTPExporter) Close() error {
	close(o.stopCh)
	o.flushWg.Wait()
	return o.flushAll()
}
