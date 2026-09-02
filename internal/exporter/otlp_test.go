package exporter

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agent-i/agent/internal/collector"
	"github.com/agent-i/agent/internal/config"
	"github.com/agent-i/agent/internal/version"
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

// TestOTLPHTTPExporter_GroupsDataPointsUnderOneMetric covers the payload shape
// for the common case: several series sharing a metric name, differing only by
// attributes. Each envelope used to become its own metric object, so a single
// flush repeated "system.cpu.time" once per CPU state; OTLP models this as one
// metric carrying many data points.
func TestOTLPHTTPExporter_GroupsDataPointsUnderOneMetric(t *testing.T) {
	var got otlpMetricsRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeGzipJSON(t, r, &got)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	exp, err := newOTLPHTTPExporter(config.ExporterConfig{
		Endpoint:      server.URL,
		BatchSize:     6,
		FlushInterval: time.Hour,
		MaxRetries:    1,
	})
	if err != nil {
		t.Fatalf("newOTLPHTTPExporter: %v", err)
	}
	defer exp.Close()

	ts := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	// Four points of one cumulative metric, then two of a gauge.
	for _, state := range []string{"user", "system", "idle", "iowait"} {
		if err := exp.Export(collector.Envelope{
			Kind: collector.KindMetric, AgentID: "host-001",
			Source: "system.cpu.time", Timestamp: ts, Value: 1,
			Labels: map[string]string{"state": state, "_boot_time_unix": "1700000000"},
		}); err != nil {
			t.Fatalf("Export: %v", err)
		}
	}
	for _, dev := range []string{"sda", "sdb"} {
		if err := exp.Export(collector.Envelope{
			Kind: collector.KindMetric, AgentID: "host-001",
			Source: "system.disk.pending_operations", Timestamp: ts, Value: 2,
			Labels: map[string]string{"device": dev},
		}); err != nil {
			t.Fatalf("Export: %v", err)
		}
	}

	metrics := got.ResourceMetrics[0].ScopeMetrics[0].Metrics
	if len(metrics) != 2 {
		t.Fatalf("expected 2 metric entries (one per name), got %d: %+v", len(metrics), metrics)
	}

	byName := map[string]otlpMetric{}
	for _, m := range metrics {
		byName[m.Name] = m
	}

	cpu, ok := byName["system.cpu.time"]
	if !ok {
		t.Fatal("system.cpu.time missing")
	}
	if cpu.Sum == nil {
		t.Fatal("system.cpu.time must be a Sum, not a Gauge")
	}
	if len(cpu.Sum.DataPoints) != 4 {
		t.Errorf("system.cpu.time has %d data points, want 4 grouped under one metric", len(cpu.Sum.DataPoints))
	}
	for _, dp := range cpu.Sum.DataPoints {
		if dp.StartTimeUnixNano == "" {
			t.Error("cumulative data point is missing startTimeUnixNano")
		}
	}

	disk, ok := byName["system.disk.pending_operations"]
	if !ok {
		t.Fatal("system.disk.pending_operations missing")
	}
	if disk.Gauge == nil {
		t.Fatal("pending_operations is a point-in-time value and must stay a Gauge")
	}
	if len(disk.Gauge.DataPoints) != 2 {
		t.Errorf("pending_operations has %d data points, want 2", len(disk.Gauge.DataPoints))
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
		t.Errorf("resource host.name not set — a backend's host-inventory view needs this or it falls back to reverse-DNS: %+v", rm.Resource.Attributes)
	}
	// os.type populates a backend's "OS Type" facet on its hosts view; without
	// it hosts appear under a blank, unselectable filter entry. The distro
	// pair identifies which build shipped the data, so a host running a
	// stale binary is visible from the backend rather than only by SSHing
	// in and asking it.
	resAttrs := map[string]string{}
	for _, a := range rm.Resource.Attributes {
		if a.Value.StringValue != nil {
			resAttrs[a.Key] = *a.Value.StringValue
		}
	}
	if resAttrs["os.type"] != runtime.GOOS {
		t.Errorf("resource os.type = %q, want %q — the OS Type filter reads this", resAttrs["os.type"], runtime.GOOS)
	}
	if resAttrs["telemetry.distro.name"] != "agent-i" {
		t.Errorf("resource telemetry.distro.name = %q, want agent-i", resAttrs["telemetry.distro.name"])
	}
	if resAttrs["telemetry.distro.version"] != version.Version {
		t.Errorf("resource telemetry.distro.version = %q, want %q", resAttrs["telemetry.distro.version"], version.Version)
	}
	// service.version must NOT be set here: on forwarded spans service.name
	// is the instrumented app's identity, and pairing it with the agent's
	// version would misreport that app's version.
	if v, ok := resAttrs["service.version"]; ok {
		t.Errorf("resource service.version = %q, want it absent — the agent's version must not be attributed to a forwarded app", v)
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
	// incoming OTLP resource attributes) and one from Agent-I's own
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

func TestOTLPHTTPExporter_SystemCPUTimeUsesSumType(t *testing.T) {
	// Regression/requirement test: a backend's host-inventory view
	// specifically requires system.cpu.time as a Sum (cumulative counter)
	// metric, not a Gauge — per the OTel hostmetrics receiver spec. This
	// verifies the actual JSON shape sent matches that requirement.
	var got otlpMetricsRequest

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

	ts := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	if err := exp.Export(collector.Envelope{
		Kind: collector.KindMetric, AgentID: "host-001", Source: "system.cpu.time",
		Timestamp: ts, Value: 1234.5,
		Labels: map[string]string{"state": "idle", "cpu": "cpu-total", "_boot_time_unix": "1786619611"},
	}); err != nil {
		t.Fatalf("Export: %v", err)
	}

	metric := got.ResourceMetrics[0].ScopeMetrics[0].Metrics[0]
	if metric.Name != "system.cpu.time" {
		t.Errorf("metric name = %q, want system.cpu.time", metric.Name)
	}
	if metric.Gauge != nil {
		t.Error("system.cpu.time was sent as Gauge — must be Sum")
	}
	if metric.Sum == nil {
		t.Fatal("system.cpu.time has no Sum field — a host-inventory view requires this metric as a Sum")
	}
	if !metric.Sum.IsMonotonic {
		t.Error("Sum.IsMonotonic = false, want true (cumulative counters only increase)")
	}
	if metric.Sum.AggregationTemporality != 2 {
		t.Errorf("Sum.AggregationTemporality = %d, want 2 (CUMULATIVE)", metric.Sum.AggregationTemporality)
	}
	dp := metric.Sum.DataPoints[0]
	if dp.AsDouble != 1234.5 {
		t.Errorf("value = %v, want 1234.5", dp.AsDouble)
	}
	wantStartNano := "1786619611000000000"
	if dp.StartTimeUnixNano != wantStartNano {
		t.Errorf("startTimeUnixNano = %q, want %q (boot time converted to nanoseconds)", dp.StartTimeUnixNano, wantStartNano)
	}

	// The internal _boot_time_unix label must NOT leak into the actual
	// OTLP attributes sent — it's bookkeeping for building this exact
	// Sum shape, not a real CPU-state attribute.
	for _, a := range dp.Attributes {
		if strings.HasPrefix(a.Key, "_") {
			t.Errorf("internal label %q leaked into OTLP attributes: %+v", a.Key, dp.Attributes)
		}
	}
	foundState := false
	for _, a := range dp.Attributes {
		if a.Key == "state" && a.Value.StringValue != nil && *a.Value.StringValue == "idle" {
			foundState = true
		}
	}
	if !foundState {
		t.Errorf("expected state=idle attribute, got: %+v", dp.Attributes)
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
		gotAuthHeader = r.Header.Get("x-ingestion-key")
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	exp, err := newOTLPHTTPExporter(config.ExporterConfig{
		Endpoint:      server.URL,
		BatchSize:     1,
		FlushInterval: time.Hour,
		MaxRetries:    1,
		Headers:       map[string]string{"x-ingestion-key": "secret-test-key"},
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

// resourceServiceNames maps each resource's service.name to the attribute set
// it carried, so a test can assert on grouping without walking the shape twice.
func resourceServiceName(t *testing.T, res otlpResource) string {
	t.Helper()
	for _, a := range res.Attributes {
		if a.Key == "service.name" && a.Value.StringValue != nil {
			return *a.Value.StringValue
		}
	}
	return ""
}

// Metrics from a container labelled as an application must report under that
// application's resource, not the host's.
//
// Spans have grouped by service since the certi-backend bug; metrics and logs
// did not, so a container's app name arrived as a point attribute while the
// resource still said "host". For logs that was worse than cosmetic: the
// backend fills the logs table's service column from the RESOURCE, so the name
// was present in an attributes map and invisible to everything that groups.
func TestOTLPHTTPExporter_MetricsGroupByOriginService(t *testing.T) {
	var got otlpMetricsRequest
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

	ts := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	for _, e := range []collector.Envelope{
		{Kind: collector.KindMetric, AgentID: "host-001", Source: "container.cpu.utilization", Timestamp: ts, Value: 12,
			Labels: map[string]string{"container.name": "checkout-1", "service.name": "checkout"}},
		{Kind: collector.KindMetric, AgentID: "host-001", Source: "container.cpu.utilization", Timestamp: ts, Value: 8,
			Labels: map[string]string{"container.name": "auth-1", "service.name": "auth-api"}},
		// No service.name: an agent-generated host metric, which must stay
		// under the host's own identity.
		{Kind: collector.KindMetric, AgentID: "host-001", Source: "host.cpu.used_pct", Timestamp: ts, Value: 40},
	} {
		if err := exp.Export(e); err != nil {
			t.Fatalf("Export: %v", err)
		}
	}

	if len(got.ResourceMetrics) != 3 {
		t.Fatalf("got %d resourceMetrics, want one per distinct service", len(got.ResourceMetrics))
	}
	seen := map[string][]string{}
	for _, rm := range got.ResourceMetrics {
		sn := resourceServiceName(t, rm.Resource)
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				seen[sn] = append(seen[sn], m.Name)
			}
		}
	}
	for _, want := range []struct{ service, metric string }{
		{"checkout", "container.cpu.utilization"},
		{"auth-api", "container.cpu.utilization"},
		{"host-001", "host.cpu.used_pct"},
	} {
		names, ok := seen[want.service]
		if !ok {
			t.Errorf("no resource for service %q; got %v", want.service, seen)
			continue
		}
		if len(names) != 1 || names[0] != want.metric {
			t.Errorf("service %q carried %v, want just %q", want.service, names, want.metric)
		}
	}
}

// The grouping must not lose histograms. They take a separate branch through
// the builder, and an early version of the per-service grouping appended their
// names to a discarded copy of the ordering slice — so every histogram was
// built, stored, and then silently omitted from the payload.
func TestOTLPHTTPExporter_GroupingKeepsHistograms(t *testing.T) {
	var got otlpMetricsRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeGzipJSON(t, r, &got)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	exp, err := newOTLPHTTPExporter(config.ExporterConfig{
		Endpoint: server.URL, BatchSize: 2, FlushInterval: time.Hour, MaxRetries: 1,
	})
	if err != nil {
		t.Fatalf("newOTLPHTTPExporter: %v", err)
	}
	defer exp.Close()

	ts := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	hist := collector.HistogramPoint{Count: 3, Sum: 30, Min: 5, Max: 20, Scale: 1, BucketCounts: []uint64{1, 2}}
	for _, svc := range []string{"checkout", "auth-api"} {
		if err := exp.Export(collector.Envelope{
			Kind: collector.KindHistogram, AgentID: "host-001", Source: "http.server.duration", Timestamp: ts,
			Labels:  map[string]string{"service.name": svc},
			Payload: map[string]any{collector.HistogramPointKey: hist},
		}); err != nil {
			t.Fatalf("Export: %v", err)
		}
	}

	if len(got.ResourceMetrics) != 2 {
		t.Fatalf("got %d resourceMetrics, want 2", len(got.ResourceMetrics))
	}
	for _, rm := range got.ResourceMetrics {
		sn := resourceServiceName(t, rm.Resource)
		found := false
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name == "http.server.duration" && m.ExponentialHistogram != nil &&
					len(m.ExponentialHistogram.DataPoints) == 1 {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("service %q lost its histogram — built and then omitted from the payload", sn)
		}
	}
}

// Logs are the case that actually broke attribution, because the backend reads
// the service column from the resource rather than from a record attribute.
func TestOTLPHTTPExporter_LogsGroupByOriginService(t *testing.T) {
	var got otlpLogsRequest
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

	ts := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	for _, e := range []collector.Envelope{
		{Kind: collector.KindLog, AgentID: "host-001", Source: "container/checkout-1", Timestamp: ts,
			Message: "checkout ok", Labels: map[string]string{"service.name": "checkout"}},
		{Kind: collector.KindLog, AgentID: "host-001", Source: "container/checkout-1", Timestamp: ts,
			Message: "checkout ok again", Labels: map[string]string{"service.name": "checkout"}},
		{Kind: collector.KindLog, AgentID: "host-001", Source: "/var/log/syslog", Timestamp: ts,
			Message: "a host log"},
	} {
		if err := exp.Export(e); err != nil {
			t.Fatalf("Export: %v", err)
		}
	}

	counts := map[string]int{}
	for _, rl := range got.ResourceLogs {
		sn := resourceServiceName(t, rl.Resource)
		for _, sl := range rl.ScopeLogs {
			counts[sn] += len(sl.LogRecords)
		}
	}
	if counts["checkout"] != 2 {
		t.Errorf("checkout resource carried %d records, want 2 — got %v", counts["checkout"], counts)
	}
	if counts["host-001"] != 1 {
		t.Errorf("host resource carried %d records, want the one unlabelled log — got %v", counts["host-001"], counts)
	}
}
