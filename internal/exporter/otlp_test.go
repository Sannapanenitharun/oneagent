package exporter

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/oneagent/agent/internal/collector"
	"github.com/oneagent/agent/internal/config"
)

func decodeGzipJSON(t *testing.T, r *http.Request, v any) {
	t.Helper()
	if r.Header.Get("Content-Encoding") != "gzip" {
		t.Errorf("missing Content-Encoding: gzip")
	}
	gr, err := gzip.NewReader(r.Body)
	if err != nil {
		t.Fatalf("gzip decode: %v", err)
	}
	defer gr.Close()
	body, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("unmarshaling: %v (body: %s)", err, body)
	}
}

func TestOTLPHTTPExporter_MetricsShapeAndEndpoint(t *testing.T) {
	var hitPath string
	var got otlpMetricsRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		decodeGzipJSON(t, r, &got)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	exp, err := newOTLPHTTPExporter(config.ExporterConfig{
		Endpoint:      server.URL,
		BatchSize:     1,
		FlushInterval: time.Hour,
		MaxRetries:    1,
	})
	if err != nil {
		t.Fatalf("newOTLPHTTPExporter: %v", err)
	}
	defer exp.Close()

	ts := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	if err := exp.Export(collector.Envelope{
		Kind:      collector.KindMetric,
		AgentID:   "host-001",
		Source:    "host.cpu.used_pct",
		Timestamp: ts,
		Value:     42.5,
		Labels:    map[string]string{"region": "us-east-1"},
	}); err != nil {
		t.Fatalf("Export: %v", err)
	}

	if hitPath != "/v1/metrics" {
		t.Errorf("expected POST to /v1/metrics, got %q", hitPath)
	}
	if len(got.ResourceMetrics) != 1 {
		t.Fatalf("expected 1 resourceMetrics entry, got %d", len(got.ResourceMetrics))
	}
	rm := got.ResourceMetrics[0]
	if rm.Resource.Attributes[0].Value.StringValue == nil || *rm.Resource.Attributes[0].Value.StringValue != "host-001" {
		t.Errorf("resource service.name not set to agent ID: %+v", rm.Resource.Attributes)
	}
	if len(rm.Resource.Attributes) < 2 || rm.Resource.Attributes[1].Key != "host.name" ||
		rm.Resource.Attributes[1].Value.StringValue == nil || *rm.Resource.Attributes[1].Value.StringValue != "host-001" {
		t.Errorf("resource host.name not set — SigNoz's Infrastructure/Hosts page needs this or it falls back to reverse-DNS: %+v", rm.Resource.Attributes)
	}
	metrics := rm.ScopeMetrics[0].Metrics
	if len(metrics) != 1 || metrics[0].Name != "host.cpu.used_pct" {
		t.Fatalf("unexpected metrics: %+v", metrics)
	}
	dp := metrics[0].Gauge.DataPoints[0]
	if dp.AsDouble != 42.5 {
		t.Errorf("asDouble = %v, want 42.5", dp.AsDouble)
	}
	wantNano := ts.UnixNano()
	if dp.TimeUnixNano != itoa64(wantNano) {
		t.Errorf("timeUnixNano = %s, want %d", dp.TimeUnixNano, wantNano)
	}
}

func TestOTLPHTTPExporter_TracesShapeAndEndpoint(t *testing.T) {
	var hitPath string
	var got otlpTracesRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		decodeGzipJSON(t, r, &got)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	exp, err := newOTLPHTTPExporter(config.ExporterConfig{
		Endpoint: server.URL, BatchSize: 1, FlushInterval: time.Hour, MaxRetries: 1,
	})
	if err != nil {
		t.Fatalf("newOTLPHTTPExporter: %v", err)
	}
	defer exp.Close()

	ts := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	if err := exp.Export(collector.Envelope{
		Kind:      collector.KindTrace,
		AgentID:   "host-001",
		Source:    "otlp.span",
		Timestamp: ts,
		Value:     150, // 150ms duration
		Labels:    map[string]string{"trace_id": "abc123", "span_id": "def456", "name": "handleRequest"},
	}); err != nil {
		t.Fatalf("Export: %v", err)
	}

	if hitPath != "/v1/traces" {
		t.Errorf("expected POST to /v1/traces, got %q", hitPath)
	}
	span := got.ResourceSpans[0].ScopeSpans[0].Spans[0]
	if span.TraceID != "abc123" || span.SpanID != "def456" || span.Name != "handleRequest" {
		t.Errorf("unexpected span: %+v", span)
	}
	startNano := ts.UnixNano()
	wantEndNano := startNano + int64(150*1e6)
	if span.StartTimeUnixNano != itoa64(startNano) {
		t.Errorf("startTimeUnixNano = %s, want %d", span.StartTimeUnixNano, startNano)
	}
	if span.EndTimeUnixNano != itoa64(wantEndNano) {
		t.Errorf("endTimeUnixNano = %s, want %d (150ms after start)", span.EndTimeUnixNano, wantEndNano)
	}
}

func TestOTLPHTTPExporter_TracesPreserveOriginServiceName(t *testing.T) {
	// Regression test for a real bug found on a live host: spans received
	// from an externally instrumented app (e.g. a Node.js backend called
	// "certi-backend") were being re-exported with service.name
	// overwritten to the AGENT's own identity, making it impossible to
	// tell which app actually produced a given trace in the backend.
	var got otlpTracesRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeGzipJSON(t, r, &got)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	exp, err := newOTLPHTTPExporter(config.ExporterConfig{
		Endpoint: server.URL, BatchSize: 3, FlushInterval: time.Hour, MaxRetries: 1,
	})
	if err != nil {
		t.Fatalf("newOTLPHTTPExporter: %v", err)
	}
	defer exp.Close()

	ts := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)

	// Two spans from a real instrumented app (carries its own
	// service.name label, as our trace receiver attaches from the
	// incoming OTLP resource attributes) and one from OneAgent's own
	// host-level activity (no service.name label at all).
	if err := exp.Export(collector.Envelope{
		Kind: collector.KindTrace, AgentID: "host-001", Timestamp: ts, Value: 10,
		Labels: map[string]string{"trace_id": "t1", "span_id": "s1", "name": "route1", "service.name": "certi-backend.service"},
	}); err != nil {
		t.Fatalf("Export 1: %v", err)
	}
	if err := exp.Export(collector.Envelope{
		Kind: collector.KindTrace, AgentID: "host-001", Timestamp: ts, Value: 20,
		Labels: map[string]string{"trace_id": "t1", "span_id": "s2", "name": "route2", "service.name": "certi-backend.service"},
	}); err != nil {
		t.Fatalf("Export 2: %v", err)
	}
	if err := exp.Export(collector.Envelope{
		Kind: collector.KindTrace, AgentID: "host-001", Timestamp: ts, Value: 5,
		Labels: map[string]string{"trace_id": "t2", "span_id": "s3", "name": "agentOwnSpan"},
	}); err != nil {
		t.Fatalf("Export 3: %v", err)
	}

	if len(got.ResourceSpans) != 2 {
		t.Fatalf("expected 2 resourceSpans groups (certi-backend.service + host-001 fallback), got %d: %+v", len(got.ResourceSpans), got.ResourceSpans)
	}

	foundCerti, foundHost := false, false
	for _, rs := range got.ResourceSpans {
		var serviceName string
		var hostName string
		for _, a := range rs.Resource.Attributes {
			if a.Key == "service.name" && a.Value.StringValue != nil {
				serviceName = *a.Value.StringValue
			}
			if a.Key == "host.name" && a.Value.StringValue != nil {
				hostName = *a.Value.StringValue
			}
		}
		// host.name must ALWAYS be the agent's own identity, regardless
		// of which group this is — it identifies the physical machine,
		// not the originating app.
		if hostName != "host-001" {
			t.Errorf("host.name = %q, want host-001 (should never vary per-service)", hostName)
		}

		switch serviceName {
		case "certi-backend.service":
			foundCerti = true
			if len(rs.ScopeSpans[0].Spans) != 2 {
				t.Errorf("certi-backend.service group: expected 2 spans, got %d", len(rs.ScopeSpans[0].Spans))
			}
		case "host-001":
			foundHost = true
			if len(rs.ScopeSpans[0].Spans) != 1 {
				t.Errorf("host-001 fallback group: expected 1 span, got %d", len(rs.ScopeSpans[0].Spans))
			}
		default:
			t.Errorf("unexpected service.name group: %q", serviceName)
		}
	}
	if !foundCerti {
		t.Error("no resourceSpans group found with service.name=certi-backend.service — the origin app's identity was lost")
	}
	if !foundHost {
		t.Error("no resourceSpans group found with the agent's own fallback identity")
	}
}

func TestOTLPHTTPExporter_LogsShapeAndEndpoint_PlainLog(t *testing.T) {
	var hitPath string
	var got otlpLogsRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		decodeGzipJSON(t, r, &got)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	exp, err := newOTLPHTTPExporter(config.ExporterConfig{
		Endpoint: server.URL, BatchSize: 1, FlushInterval: time.Hour, MaxRetries: 1,
	})
	if err != nil {
		t.Fatalf("newOTLPHTTPExporter: %v", err)
	}
	defer exp.Close()

	if err := exp.Export(collector.Envelope{
		Kind: collector.KindLog, AgentID: "host-001", Source: "/var/log/app.log",
		Timestamp: time.Now().UTC(), Message: "connection refused",
	}); err != nil {
		t.Fatalf("Export: %v", err)
	}

	if hitPath != "/v1/logs" {
		t.Errorf("expected POST to /v1/logs, got %q", hitPath)
	}
	rec := got.ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	if rec.Body.StringValue == nil || *rec.Body.StringValue != "connection refused" {
		t.Errorf("log body = %+v, want 'connection refused'", rec.Body)
	}
}

func TestOTLPHTTPExporter_LogsShapeAndEndpoint_APICallBuildsSummaryBody(t *testing.T) {
	var got otlpLogsRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeGzipJSON(t, r, &got)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	exp, err := newOTLPHTTPExporter(config.ExporterConfig{
		Endpoint: server.URL, BatchSize: 1, FlushInterval: time.Hour, MaxRetries: 1,
	})
	if err != nil {
		t.Fatalf("newOTLPHTTPExporter: %v", err)
	}
	defer exp.Close()

	// api_call envelopes have no Message field — the exporter must
	// synthesize a readable body from Labels/Value rather than sending
	// an empty log line.
	if err := exp.Export(collector.Envelope{
		Kind: collector.KindAPICall, AgentID: "host-001", Source: "http.access_log:/var/log/nginx/access.log",
		Timestamp: time.Now().UTC(), Value: 87,
		Labels: map[string]string{"method": "GET", "path": "/api/orders", "status": "200"},
	}); err != nil {
		t.Fatalf("Export: %v", err)
	}

	rec := got.ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	if rec.Body.StringValue == nil || *rec.Body.StringValue == "" {
		t.Fatal("api_call envelope produced an empty log body")
	}
	want := "GET /api/orders -> 200 (87.0ms)"
	if *rec.Body.StringValue != want {
		t.Errorf("body = %q, want %q", *rec.Body.StringValue, want)
	}
}

func TestOTLPHTTPExporter_MixedKindsRouteToCorrectEndpoints(t *testing.T) {
	hits := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path]++
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	exp, err := newOTLPHTTPExporter(config.ExporterConfig{
		Endpoint: server.URL, BatchSize: 100, FlushInterval: time.Hour, MaxRetries: 1,
	})
	if err != nil {
		t.Fatalf("newOTLPHTTPExporter: %v", err)
	}

	_ = exp.Export(collector.Envelope{Kind: collector.KindMetric, AgentID: "a", Timestamp: time.Now()})
	_ = exp.Export(collector.Envelope{Kind: collector.KindTrace, AgentID: "a", Timestamp: time.Now()})
	_ = exp.Export(collector.Envelope{Kind: collector.KindLog, AgentID: "a", Timestamp: time.Now()})
	_ = exp.Export(collector.Envelope{Kind: collector.KindAPICall, AgentID: "a", Timestamp: time.Now()})

	if err := exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if hits["/v1/metrics"] != 1 {
		t.Errorf("expected exactly 1 hit on /v1/metrics, got %d", hits["/v1/metrics"])
	}
	if hits["/v1/traces"] != 1 {
		t.Errorf("expected exactly 1 hit on /v1/traces, got %d", hits["/v1/traces"])
	}
	// KindLog and KindAPICall are batched into the SAME logs buffer, so
	// one flush should produce one /v1/logs request carrying both.
	if hits["/v1/logs"] != 1 {
		t.Errorf("expected exactly 1 hit on /v1/logs (log+api_call batched together), got %d", hits["/v1/logs"])
	}
}

func TestOTLPHTTPExporter_SendsCustomHeaders(t *testing.T) {
	var gotAuthHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeader = r.Header.Get("signoz-ingestion-key")
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	exp, err := newOTLPHTTPExporter(config.ExporterConfig{
		Endpoint:      server.URL,
		BatchSize:     1,
		FlushInterval: time.Hour,
		MaxRetries:    1,
		Headers:       map[string]string{"signoz-ingestion-key": "secret-test-key"},
	})
	if err != nil {
		t.Fatalf("newOTLPHTTPExporter: %v", err)
	}
	defer exp.Close()

	_ = exp.Export(collector.Envelope{Kind: collector.KindMetric, AgentID: "a", Timestamp: time.Now()})

	if gotAuthHeader != "secret-test-key" {
		t.Errorf("ingestion key header = %q, want secret-test-key", gotAuthHeader)
	}
}

func itoa64(n int64) string {
	return jsonNumberString(n)
}

func jsonNumberString(n int64) string {
	b, _ := json.Marshal(n)
	// strconv.FormatInt and json.Marshal of an int64 produce identical
	// decimal text for any value in range — reusing json.Marshal here
	// just avoids importing strconv into the test file for one helper.
	return string(b)
}
