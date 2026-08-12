package exporter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/oneagent/agent/internal/collector"
	"github.com/oneagent/agent/internal/config"
)

// This file exists because SigNoz (and any other OTLP-native backend)
// does NOT understand our internal Envelope JSON — it speaks OTLP, which
// has three distinct wire shapes (metrics/traces/logs), each with its own
// endpoint (/v1/metrics, /v1/traces, /v1/logs) and schema. The plain
// httpExporter in exporter.go sends our own format and would arrive at
// SigNoz as bytes it can't parse. This exporter does the real conversion:
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
	Name  string    `json:"name"`
	Gauge otlpGauge `json:"gauge"`
}
type otlpGauge struct {
	DataPoints []otlpNumberDataPoint `json:"dataPoints"`
}
type otlpNumberDataPoint struct {
	TimeUnixNano string         `json:"timeUnixNano"`
	AsDouble     float64        `json:"asDouble"`
	Attributes   []otlpKeyValue `json:"attributes,omitempty"`
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
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
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
	flushInterval time.Duration
	maxRetries    int

	mu         sync.Mutex
	metricsBuf []collector.Envelope
	tracesBuf  []collector.Envelope
	logsBuf    []collector.Envelope
	stopCh     chan struct{}
	flushWg    sync.WaitGroup
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
		flushInterval: flushInterval,
		maxRetries:    maxRetries,
		stopCh:        make(chan struct{}),
	}
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
	var full bool
	switch e.Kind {
	case collector.KindMetric:
		o.metricsBuf = append(o.metricsBuf, e)
		full = len(o.metricsBuf) >= o.batchSize
	case collector.KindTrace:
		o.tracesBuf = append(o.tracesBuf, e)
		full = len(o.tracesBuf) >= o.batchSize
	default: // KindLog, KindAPICall — both exported as OTLP logs
		o.logsBuf = append(o.logsBuf, e)
		full = len(o.logsBuf) >= o.batchSize
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

func (o *otlpHTTPExporter) resource() otlpResource {
	name := o.agentIDLabel
	if name == "" {
		name = "oneagent-agent"
	}
	return otlpResource{Attributes: []otlpKeyValue{stringAttr("service.name", name)}}
}

func envelopeAttrs(e collector.Envelope) []otlpKeyValue {
	attrs := make([]otlpKeyValue, 0, len(e.Labels))
	for k, v := range e.Labels {
		attrs = append(attrs, stringAttr(k, v))
	}
	return attrs
}

func (o *otlpHTTPExporter) sendMetrics(envs []collector.Envelope) error {
	points := make([]otlpMetric, 0, len(envs))
	for _, e := range envs {
		points = append(points, otlpMetric{
			Name: e.Source,
			Gauge: otlpGauge{DataPoints: []otlpNumberDataPoint{{
				TimeUnixNano: strconv.FormatInt(e.Timestamp.UnixNano(), 10),
				AsDouble:     e.Value,
				Attributes:   envelopeAttrs(e),
			}}},
		})
	}
	req := otlpMetricsRequest{ResourceMetrics: []otlpResourceMetrics{{
		Resource:     o.resource(),
		ScopeMetrics: []otlpScopeMetrics{{Scope: otlpScope{Name: "oneagent-agent"}, Metrics: points}},
	}}}
	return o.postJSON("/v1/metrics", req, len(envs))
}

func (o *otlpHTTPExporter) sendTraces(envs []collector.Envelope) error {
	spans := make([]otlpSpan, 0, len(envs))
	for _, e := range envs {
		startNano := e.Timestamp.UnixNano()
		endNano := startNano + int64(e.Value*1e6) // Value is duration in ms
		spans = append(spans, otlpSpan{
			TraceID:           e.Labels["trace_id"],
			SpanID:            e.Labels["span_id"],
			Name:              e.Labels["name"],
			StartTimeUnixNano: strconv.FormatInt(startNano, 10),
			EndTimeUnixNano:   strconv.FormatInt(endNano, 10),
			Attributes:        envelopeAttrs(e),
		})
	}
	req := otlpTracesRequest{ResourceSpans: []otlpResourceSpans{{
		Resource:   o.resource(),
		ScopeSpans: []otlpScopeSpans{{Scope: otlpScope{Name: "oneagent-agent"}, Spans: spans}},
	}}}
	return o.postJSON("/v1/traces", req, len(envs))
}

func (o *otlpHTTPExporter) sendLogs(envs []collector.Envelope) error {
	records := make([]otlpLogRecord, 0, len(envs))
	for _, e := range envs {
		body := e.Message
		if body == "" && e.Kind == collector.KindAPICall {
			// api_call envelopes carry structured fields in Labels/Value
			// rather than a Message — build a readable summary line so
			// the log still shows something meaningful in SigNoz's log
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
		Resource:  o.resource(),
		ScopeLogs: []otlpScopeLogs{{Scope: otlpScope{Name: "oneagent-agent"}, LogRecords: records}},
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
