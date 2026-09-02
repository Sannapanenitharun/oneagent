package ingest

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/agent-i/agent/internal/otlpwire"
)

var testNow = time.Date(2026, 8, 26, 10, 30, 0, 0, time.UTC)

// The payload the agent actually sends. Captured from internal/exporter's own
// JSON shape rather than written from the spec, because the thing worth
// testing is that this backend accepts its own agent — a decoder that is
// correct against the specification and wrong against the producer is still
// broken.
const agentMetricsJSON = `{"resourceMetrics":[{"resource":{"attributes":[
 {"key":"service.name","value":{"stringValue":"agent-i"}},
 {"key":"host.id","value":{"stringValue":"i-00aab1097c1a58ac5"}},
 {"key":"host.name","value":{"stringValue":"teleport"}},
 {"key":"os.name","value":{"stringValue":"Ubuntu"}},
 {"key":"cloud.account.id","value":{"stringValue":"123456789012"}}]},
 "scopeMetrics":[{"scope":{"name":"agent-i"},"metrics":[
  {"name":"host.cpu.used_pct","gauge":{"dataPoints":[{"timeUnixNano":"1787740680000000000","asDouble":37.5}]}},
  {"name":"system.cpu.time","sum":{"isMonotonic":true,"aggregationTemporality":2,
    "dataPoints":[{"timeUnixNano":"1787740680000000000","asDouble":1007.2,
      "attributes":[{"key":"state","value":{"stringValue":"user"}}]}]}}
 ]}]}]}`

func TestUnmarshalJSONMetrics_AgentPayload(t *testing.T) {
	req, err := UnmarshalJSONMetrics([]byte(agentMetricsJSON))
	if err != nil {
		t.Fatalf("UnmarshalJSONMetrics: %v", err)
	}
	b := Metrics(req, testNow)

	if len(b.Metrics) != 2 {
		t.Fatalf("got %d metric rows, want 2", len(b.Metrics))
	}
	if len(b.Hosts) != 1 {
		t.Fatalf("got %d host rows, want 1", len(b.Hosts))
	}

	byName := map[string]Row{}
	for _, r := range b.Metrics {
		byName[r["name"].(string)] = r
	}

	gauge := byName["host.cpu.used_pct"]
	if gauge == nil {
		t.Fatal("gauge metric missing")
	}
	if gauge["value"].(float64) != 37.5 {
		t.Errorf("gauge value = %v, want 37.5", gauge["value"])
	}
	// A gauge is not monotonic; treating it as a counter would have consumers
	// computing rates from a level.
	if gauge["is_monotonic"].(uint8) != 0 {
		t.Errorf("gauge marked monotonic")
	}
	if gauge["host_id"] != "i-00aab1097c1a58ac5" {
		t.Errorf("host_id = %v", gauge["host_id"])
	}

	sum := byName["system.cpu.time"]
	if sum["is_monotonic"].(uint8) != 1 {
		t.Errorf("monotonic sum not marked monotonic")
	}
	attrs := sum["attributes"].(map[string]string)
	if attrs["state"] != "user" {
		t.Errorf("point attribute lost: %v", attrs)
	}
	// Resource attributes ride on every row so a query can filter by account
	// or OS without a join.
	if attrs["cloud.account.id"] != "123456789012" || attrs["os.name"] != "Ubuntu" {
		t.Errorf("resource attributes not merged onto the row: %v", attrs)
	}
}

// The protobuf JSON mapping encodes 64-bit integers as strings. Unmarshalling
// one into a uint64 fails silently to zero, which would put every point at the
// epoch — present, unfindable, and outside every query's time range.
func TestParseUnixNano_StringEncoded(t *testing.T) {
	req, err := UnmarshalJSONMetrics([]byte(agentMetricsJSON))
	if err != nil {
		t.Fatal(err)
	}
	got := req.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].NumberPoints[0].TimeUnixNano
	if got != 1787740680000000000 {
		t.Fatalf("timeUnixNano = %d, want 1787740680000000000 — the string form was not parsed", got)
	}
}

func TestMetrics_ZeroTimestampBecomesReceiveTime(t *testing.T) {
	req := &otlpwire.ExportMetricsServiceRequest{ResourceMetrics: []*otlpwire.ResourceMetrics{{
		Resource: &otlpwire.Resource{Attributes: []*otlpwire.KeyValue{
			{Key: "host.id", Value: &otlpwire.AnyValue{Kind: otlpwire.ValueString, Str: "h1"}},
		}},
		ScopeMetrics: []*otlpwire.ScopeMetrics{{Metrics: []*otlpwire.Metric{{
			Name: "m", NumberPoints: []*otlpwire.NumberDataPoint{{TimeUnixNano: 0, Value: 1}},
		}}}},
	}}}
	b := Metrics(req, testNow)
	ts := b.Metrics[0]["timestamp"].(string)
	if !strings.HasPrefix(ts, "2026-08-26") {
		t.Fatalf("timestamp = %q, want the receive time — a zero stamp would land in 1970", ts)
	}
}

// host_id is what every row is attributed by. A row without one is
// unqueryable, so the fallback chain matters more than it looks.
func TestReadResource_HostIDFallsBack(t *testing.T) {
	cases := []struct {
		name  string
		attrs map[string]string
		want  string
	}{
		{"host.id wins", map[string]string{"host.id": "i-abc", "host.name": "web", "service.name": "svc"}, "i-abc"},
		{"host.name next", map[string]string{"host.name": "web", "service.name": "svc"}, "web"},
		{"service.name last", map[string]string{"service.name": "svc"}, "svc"},
		{"nothing", map[string]string{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var kvs []*otlpwire.KeyValue
			for k, v := range tc.attrs {
				kvs = append(kvs, &otlpwire.KeyValue{Key: k, Value: &otlpwire.AnyValue{Kind: otlpwire.ValueString, Str: v}})
			}
			if got := readResource(&otlpwire.Resource{Attributes: kvs}).hostID; got != tc.want {
				t.Errorf("hostID = %q, want %q", got, tc.want)
			}
		})
	}
}

// One host row per resource, not one per data point — otherwise a single
// export of fifty metrics writes fifty identical inventory rows.
func TestMetrics_HostRowIsDeduplicated(t *testing.T) {
	points := make([]*otlpwire.NumberDataPoint, 20)
	for i := range points {
		points[i] = &otlpwire.NumberDataPoint{TimeUnixNano: 1, Value: float64(i)}
	}
	req := &otlpwire.ExportMetricsServiceRequest{ResourceMetrics: []*otlpwire.ResourceMetrics{{
		Resource: &otlpwire.Resource{Attributes: []*otlpwire.KeyValue{
			{Key: "host.id", Value: &otlpwire.AnyValue{Kind: otlpwire.ValueString, Str: "h1"}},
		}},
		ScopeMetrics: []*otlpwire.ScopeMetrics{{Metrics: []*otlpwire.Metric{{Name: "m", NumberPoints: points}}}},
	}}}
	b := Metrics(req, testNow)
	if len(b.Metrics) != 20 {
		t.Errorf("got %d metric rows, want 20", len(b.Metrics))
	}
	if len(b.Hosts) != 1 {
		t.Errorf("got %d host rows for one resource, want 1", len(b.Hosts))
	}
}

const agentTracesJSON = `{"resourceSpans":[{"resource":{"attributes":[
 {"key":"service.name","value":{"stringValue":"checkout"}},
 {"key":"host.id","value":{"stringValue":"i-abc"}}]},
 "scopeSpans":[{"spans":[
  {"traceId":"5b8efff798038103d269b633813fc60c","spanId":"eee19b7ec3c1b174",
   "parentSpanId":"eee19b7ec3c1b100","name":"GET /checkout","kind":2,
   "startTimeUnixNano":"1787740680000000000","endTimeUnixNano":"1787740680250000000",
   "status":{"code":2,"message":"boom"}}
 ]}]}]}`

func TestUnmarshalJSONTraces_AgentPayload(t *testing.T) {
	req, err := UnmarshalJSONTraces([]byte(agentTracesJSON))
	if err != nil {
		t.Fatalf("UnmarshalJSONTraces: %v", err)
	}
	b := Spans(req, testNow)
	if len(b.Spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(b.Spans))
	}
	s := b.Spans[0]

	if s["trace_id"] != "5b8efff798038103d269b633813fc60c" {
		t.Errorf("trace_id = %v — hex did not round trip", s["trace_id"])
	}
	if s["service"] != "checkout" || s["name"] != "GET /checkout" {
		t.Errorf("identity fields wrong: %v", s)
	}
	if s["kind"] != "server" {
		t.Errorf("kind = %v, want server", s["kind"])
	}
	if s["duration_ns"].(uint64) != 250000000 {
		t.Errorf("duration = %v, want 250ms in nanos", s["duration_ns"])
	}
	if s["status_code"] != "error" || s["status_message"] != "boom" {
		t.Errorf("status lost: %v / %v", s["status_code"], s["status_message"])
	}
}

// Clocks step. A span whose end precedes its start would underflow to an
// enormous unsigned duration and poison every percentile it landed in.
func TestSpans_EndBeforeStartIsZeroNotHuge(t *testing.T) {
	req := &otlpwire.ExportTraceServiceRequest{ResourceSpans: []*otlpwire.ResourceSpans{{
		Resource:   &otlpwire.Resource{},
		ScopeSpans: []*otlpwire.ScopeSpans{{Spans: []*otlpwire.Span{{StartTimeUnixNano: 2000, EndTimeUnixNano: 1000}}}},
	}}}
	b := Spans(req, testNow)
	if got := b.Spans[0]["duration_ns"].(uint64); got != 0 {
		t.Fatalf("duration = %d, want 0 — it underflowed", got)
	}
}

// Unset is not Ok. Collapsing them makes an error rate depend on how
// thoroughly an application annotates its spans.
func TestStatusCodeName_UnsetIsDistinctFromOk(t *testing.T) {
	if statusCodeName(nil) != "unset" {
		t.Error("a span with no status should be unset")
	}
	if statusCodeName(&otlpwire.Status{Code: 0}) != "unset" {
		t.Error("code 0 should be unset")
	}
	if statusCodeName(&otlpwire.Status{Code: 1}) != "ok" {
		t.Error("code 1 should be ok")
	}
	if statusCodeName(&otlpwire.Status{Code: 2}) != "error" {
		t.Error("code 2 should be error")
	}
}

const agentLogsJSON = `{"resourceLogs":[{"resource":{"attributes":[
 {"key":"service.name","value":{"stringValue":"checkout"}},
 {"key":"host.id","value":{"stringValue":"i-abc"}}]},
 "scopeLogs":[{"logRecords":[
  {"timeUnixNano":"1787740680000000000","severityNumber":17,
   "body":{"stringValue":"connection refused"},
   "traceId":"5b8efff798038103d269b633813fc60c"}
 ]}]}]}`

func TestUnmarshalJSONLogs_AgentPayload(t *testing.T) {
	req, err := UnmarshalJSONLogs([]byte(agentLogsJSON))
	if err != nil {
		t.Fatalf("UnmarshalJSONLogs: %v", err)
	}
	b := Logs(req, testNow)
	if len(b.Logs) != 1 {
		t.Fatalf("got %d logs, want 1", len(b.Logs))
	}
	l := b.Logs[0]
	if l["body"] != "connection refused" {
		t.Errorf("body = %v", l["body"])
	}
	// severityText was absent, so the name has to come from the number or the
	// column is half empty and useless as a filter.
	if l["severity"] != "ERROR" {
		t.Errorf("severity = %v, want ERROR derived from 17", l["severity"])
	}
	if l["severity_num"].(uint8) != 17 {
		t.Errorf("severity_num = %v", l["severity_num"])
	}
	if l["trace_id"] != "5b8efff798038103d269b633813fc60c" {
		t.Errorf("trace correlation lost: %v", l["trace_id"])
	}
}

func TestSeverityName_Bands(t *testing.T) {
	for _, tc := range []struct {
		n    int32
		want string
	}{{1, "TRACE"}, {5, "DEBUG"}, {9, "INFO"}, {13, "WARN"}, {17, "ERROR"}, {21, "FATAL"}, {0, ""}} {
		if got := severityName(tc.n); got != tc.want {
			t.Errorf("severityName(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// A malformed export must cost that export, not the process.
func TestUnmarshalJSON_RejectsGarbage(t *testing.T) {
	if _, err := UnmarshalJSONMetrics([]byte("not json")); err == nil {
		t.Error("garbage metrics accepted")
	}
	if _, err := UnmarshalJSONTraces([]byte("{")); err == nil {
		t.Error("truncated traces accepted")
	}
	if _, err := UnmarshalJSONLogs([]byte("[]")); err == nil {
		t.Error("wrong-shaped logs accepted")
	}
}

func TestDecodeHex_BadIDYieldsNilNotError(t *testing.T) {
	if got := decodeHex("nothex!!"); got != nil {
		t.Errorf("got %v, want nil for an unreadable id", got)
	}
	want, _ := hex.DecodeString("abcd")
	if got := decodeHex("abcd"); string(got) != string(want) {
		t.Errorf("valid hex did not decode")
	}
}

// A histogram becomes .count and .sum series; the store has one value column.
func TestMetrics_HistogramSplitsIntoCountAndSum(t *testing.T) {
	req := &otlpwire.ExportMetricsServiceRequest{ResourceMetrics: []*otlpwire.ResourceMetrics{{
		Resource: &otlpwire.Resource{Attributes: []*otlpwire.KeyValue{
			{Key: "host.id", Value: &otlpwire.AnyValue{Kind: otlpwire.ValueString, Str: "h1"}},
		}},
		ScopeMetrics: []*otlpwire.ScopeMetrics{{Metrics: []*otlpwire.Metric{{
			Name:            "http.duration",
			HistogramPoints: []*otlpwire.HistogramDataPoint{{TimeUnixNano: 1, Count: 10, Sum: 42.5, HasSum: true}},
		}}}},
	}}}
	b := Metrics(req, testNow)
	names := map[string]float64{}
	for _, r := range b.Metrics {
		names[r["name"].(string)] = r["value"].(float64)
	}
	if names["http.duration.count"] != 10 {
		t.Errorf("count series = %v, want 10", names["http.duration.count"])
	}
	if names["http.duration.sum"] != 42.5 {
		t.Errorf("sum series = %v, want 42.5", names["http.duration.sum"])
	}
}

// HasSum exists so a histogram whose values are all zero is distinguishable
// from one that sent no sum at all.
func TestMetrics_HistogramWithoutSumEmitsOnlyCount(t *testing.T) {
	req := &otlpwire.ExportMetricsServiceRequest{ResourceMetrics: []*otlpwire.ResourceMetrics{{
		Resource: &otlpwire.Resource{},
		ScopeMetrics: []*otlpwire.ScopeMetrics{{Metrics: []*otlpwire.Metric{{
			Name:            "d",
			HistogramPoints: []*otlpwire.HistogramDataPoint{{TimeUnixNano: 1, Count: 3, HasSum: false}},
		}}}},
	}}}
	b := Metrics(req, testNow)
	for _, r := range b.Metrics {
		if strings.HasSuffix(r["name"].(string), ".sum") {
			t.Fatal("emitted a .sum series for a histogram that sent no sum")
		}
	}
}

func TestBatch_EmptyInputsAreSafe(t *testing.T) {
	if !(Metrics(nil, testNow).Empty() && Logs(nil, testNow).Empty() && Spans(nil, testNow).Empty()) {
		t.Fatal("a nil request produced rows")
	}
}

func resourceWith(kv ...string) *otlpwire.Resource {
	r := &otlpwire.Resource{}
	for i := 0; i < len(kv); i += 2 {
		r.Attributes = append(r.Attributes, &otlpwire.KeyValue{
			Key: kv[i], Value: &otlpwire.AnyValue{Kind: otlpwire.ValueString, Str: kv[i+1]},
		})
	}
	return r
}

// One export now carries a resource per application, because the agent labels a
// container's telemetry with the app it runs. They describe one machine and
// differ only in service.name, so the inventory row must describe the machine
// rather than whichever application sorted first.
func TestMetrics_HostRowDescribesTheMachineNotAWorkload(t *testing.T) {
	req := &otlpwire.ExportMetricsServiceRequest{
		ResourceMetrics: []*otlpwire.ResourceMetrics{
			// A container's resource arrives first, as it routinely will.
			{Resource: resourceWith("host.id", "i-0abc", "host.name", "teleport",
				"os.name", "Ubuntu", "service.name", "checkout")},
			// The host's own resource: service.name here IS the host.
			{Resource: resourceWith("host.id", "i-0abc", "host.name", "teleport",
				"os.name", "Ubuntu", "service.name", "teleport")},
		},
	}
	b := Metrics(req, time.Now())

	if len(b.Hosts) != 1 {
		t.Fatalf("got %d host rows, want one per machine", len(b.Hosts))
	}
	attrs, _ := b.Hosts[0]["attributes"].(map[string]string)
	if got := attrs["service.name"]; got != "teleport" {
		t.Errorf("host inventory service.name = %q, want the host's own identity — "+
			"an application's name here makes the fleet row describe a workload", got)
	}
	if attrs["os.name"] != "Ubuntu" {
		t.Errorf("os.name = %q, want the machine description preserved", attrs["os.name"])
	}
	if b.Hosts[0]["host_id"] != "i-0abc" {
		t.Errorf("host_id = %v, want the instance id", b.Hosts[0]["host_id"])
	}
}

// Ordering must not decide the answer: the same two resources reversed produce
// the same row.
func TestMetrics_HostRowIsIndependentOfResourceOrder(t *testing.T) {
	host := resourceWith("host.id", "i-0abc", "host.name", "teleport", "os.name", "Ubuntu", "service.name", "teleport")
	app := resourceWith("host.id", "i-0abc", "host.name", "teleport", "os.name", "Ubuntu", "service.name", "checkout")

	for _, order := range [][]*otlpwire.Resource{{app, host}, {host, app}} {
		req := &otlpwire.ExportMetricsServiceRequest{}
		for _, r := range order {
			req.ResourceMetrics = append(req.ResourceMetrics, &otlpwire.ResourceMetrics{Resource: r})
		}
		b := Metrics(req, time.Now())
		if len(b.Hosts) != 1 {
			t.Fatalf("got %d host rows, want 1", len(b.Hosts))
		}
		attrs, _ := b.Hosts[0]["attributes"].(map[string]string)
		if attrs["service.name"] != "teleport" {
			t.Errorf("service.name = %q regardless of order, want %q", attrs["service.name"], "teleport")
		}
	}
}

// A sender that reports neither host.id nor host.name is identified BY its
// service name. Stripping it there would leave the machine with nothing
// describing it.
func TestMetrics_ServiceNameSurvivesWhenItIsTheIdentity(t *testing.T) {
	req := &otlpwire.ExportMetricsServiceRequest{
		ResourceMetrics: []*otlpwire.ResourceMetrics{
			{Resource: resourceWith("service.name", "standalone-app")},
		},
	}
	b := Metrics(req, time.Now())
	if len(b.Hosts) != 1 {
		t.Fatalf("got %d host rows, want 1", len(b.Hosts))
	}
	attrs, _ := b.Hosts[0]["attributes"].(map[string]string)
	if attrs["service.name"] != "standalone-app" {
		t.Errorf("service.name = %q, want it kept when it is the host's only identity", attrs["service.name"])
	}
	if b.Hosts[0]["host_id"] != "standalone-app" {
		t.Errorf("host_id = %v, want the service-name fallback", b.Hosts[0]["host_id"])
	}
}
