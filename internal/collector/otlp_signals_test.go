package collector

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// The golden payloads below are the exact bytes google.golang.org/protobuf and
// go.opentelemetry.io/proto produced for these messages — the same encoding a
// real OTel SDK exporter puts on the wire with the default
// OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf.
//
// Frozen here rather than re-encoded at test time, for the same reason the
// trace golden in traces_protobuf_test.go is frozen: re-encoding with an
// encoder written from the same reading of the spec as the decoder would prove
// only that the two agree with each other. These bytes come from the reference
// implementation, so they test the decoder against the format itself.
//
// goldenLogsRequest: service "checkout", scope "app.logger", one ERROR record
// with body "payment declined", attribute order.id=A-42, and a trace/span id.
const goldenLogsRequest = "0a86010a1c0a1a0a0c736572766963652e6e616d65120a0a08636865636b6f757412660a0c0a0a6170702e6c6f676765721256090000ab1422fdcb1810111a054552524f522a120a107061796d656e74206465636c696e656432120a086f726465722e696412060a04412d34324a100102030405060708090a0b0c0d0e0f1052081112131415161718"

// goldenMetricsRequest: a double gauge, a monotonic int sum, a histogram with
// buckets, and a summary — the last being a type this agent does not decode.
const goldenMetricsRequest = "0ae7020a1c0a1a0a0c736572766963652e6e616d65120a0a08636865636b6f757412c6020a0b0a096170702e6d65746572123e0a0b71756575652e64657074681a067b6974656d7d2a270a25190000ab1422fdcb183a110a05717565756512080a066f7264657273210000000000001e4012540a14687474702e7365727665722e72657175657374731a097b726571756573747d3a310a2d190000ab1422fdcb183a190a0a687474702e726f757465120b0a092f636865636b6f7574312a00000000000000180112700a14687474702e7365727665722e6475726174696f6e1a026d734a540a52190000ab1422fdcb182103000000000000002900000000000029403210010000000000000002000000000000003a0800000000000014404a190a0a687474702e726f757465120b0a092f636865636b6f7574122f0a0e6c65676163792e73756d6d6172795a1d0a1b190000ab1422fdcb18210900000000000000290000000000c05840"

// goldenNegativeSumRequest carries an as_int of -17.
const goldenNegativeSumRequest = "0a25122312210a0971756575652e6c61673a140a12190000ab1422fdcb1831efffffffffffffff"

// goldenTimeNano is the timestamp every golden data point carries.
const goldenTimeNano = int64(1786800000000000000)

// startReceiver brings up a receiver on its own port and returns the envelope
// channel and the base URL.
func startReceiver(t *testing.T, port int, configure func(*OTLPReceiverCollector)) (chan Envelope, string) {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	coll := NewOTLPReceiverCollector("test-agent", addr, 4<<20, "")
	if configure != nil {
		configure(coll)
	}
	out := make(chan Envelope, 32)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := coll.Start(ctx, out); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = coll.Stop() })
	time.Sleep(150 * time.Millisecond) // let the listener bind
	return out, "http://" + addr
}

func postGolden(t *testing.T, url, golden string) *http.Response {
	t.Helper()
	body, err := hex.DecodeString(golden)
	if err != nil {
		t.Fatalf("decoding golden: %v", err)
	}
	resp, err := http.Post(url, "application/x-protobuf", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

// drain collects everything queued without blocking on an empty channel.
func drain(out chan Envelope) []Envelope {
	var got []Envelope
	for {
		select {
		case e := <-out:
			got = append(got, e)
		default:
			return got
		}
	}
}

// bySource indexes envelopes by the metric name they carry.
func bySource(envs []Envelope) map[string]Envelope {
	m := make(map[string]Envelope, len(envs))
	for _, e := range envs {
		m[e.Source] = e
	}
	return m
}

// An application's logs must survive the trip from an SDK's binary export into
// the agent's own envelope with their identity intact — which service emitted
// them, at what level, and inside which trace.
func TestOTLPReceiver_LogsProtobuf(t *testing.T) {
	out, base := startReceiver(t, 14340, nil)

	resp := postGolden(t, base+"/v1/logs", goldenLogsRequest)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	envs := drain(out)
	if len(envs) != 1 {
		t.Fatalf("got %d envelopes, want 1", len(envs))
	}
	e := envs[0]
	if e.Kind != KindLog {
		t.Errorf("kind = %q, want %q", e.Kind, KindLog)
	}
	if e.Message != "payment declined" {
		t.Errorf("message = %q", e.Message)
	}
	if e.Labels["service.name"] != "checkout" {
		t.Errorf("service.name = %q", e.Labels["service.name"])
	}
	if e.Labels["scope.name"] != "app.logger" {
		t.Errorf("scope.name = %q", e.Labels["scope.name"])
	}
	if e.Labels["severity"] != "ERROR" {
		t.Errorf("severity = %q", e.Labels["severity"])
	}
	// 17 is SEVERITY_NUMBER_ERROR. The number is what a filter can order.
	if e.Labels["severity.number"] != "17" {
		t.Errorf("severity.number = %q, want 17", e.Labels["severity.number"])
	}
	// Without these the application's logs and its traces are two unrelated
	// lists, which is the whole reason the ids are on the record.
	if e.Labels["trace_id"] != "0102030405060708090a0b0c0d0e0f10" {
		t.Errorf("trace_id = %q", e.Labels["trace_id"])
	}
	if e.Labels["span_id"] != "1112131415161718" {
		t.Errorf("span_id = %q", e.Labels["span_id"])
	}
	if e.Timestamp.UnixNano() != goldenTimeNano {
		t.Errorf("timestamp = %d, want %d", e.Timestamp.UnixNano(), goldenTimeNano)
	}
	attrs, _ := e.Payload["attributes"].(map[string]any)
	if attrs["order.id"] != "A-42" {
		t.Errorf("attributes = %v", attrs)
	}
}

// The JSON encoding must produce the same envelope as the protobuf one. An SDK
// picks the encoding, not the operator, so a difference here would be
// telemetry that changes shape for reasons nobody chose.
func TestOTLPReceiver_LogsJSONMatchesProtobuf(t *testing.T) {
	out, base := startReceiver(t, 14341, nil)

	const body = `{"resourceLogs":[{"resource":{"attributes":[
	  {"key":"service.name","value":{"stringValue":"checkout"}}]},
	  "scopeLogs":[{"scope":{"name":"app.logger"},"logRecords":[{
	    "timeUnixNano":"1786800000000000000","severityNumber":17,"severityText":"ERROR",
	    "body":{"stringValue":"payment declined"},
	    "attributes":[{"key":"order.id","value":{"stringValue":"A-42"}}],
	    "traceId":"0102030405060708090a0b0c0d0e0f10","spanId":"1112131415161718"}]}]}]}`

	resp, err := http.Post(base+"/v1/logs", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	envs := drain(out)
	if len(envs) != 1 {
		t.Fatalf("got %d envelopes, want 1", len(envs))
	}
	e := envs[0]
	if e.Message != "payment declined" || e.Labels["severity"] != "ERROR" {
		t.Errorf("message/severity = %q / %q", e.Message, e.Labels["severity"])
	}
	if e.Labels["service.name"] != "checkout" || e.Labels["scope.name"] != "app.logger" {
		t.Errorf("labels = %v", e.Labels)
	}
	if e.Labels["trace_id"] != "0102030405060708090a0b0c0d0e0f10" {
		t.Errorf("trace_id = %q", e.Labels["trace_id"])
	}
	if e.Timestamp.UnixNano() != goldenTimeNano {
		t.Errorf("timestamp = %d", e.Timestamp.UnixNano())
	}
}

// A log record with no timestamp is legal on the wire. It must land in the
// retention window rather than in 1970, where no view would ever show it.
func TestOTLPReceiver_LogWithoutTimestampUsesNow(t *testing.T) {
	out, base := startReceiver(t, 14342, nil)

	const body = `{"resourceLogs":[{"scopeLogs":[{"logRecords":[
	  {"body":{"stringValue":"no timestamp here"}}]}]}]}`
	resp, err := http.Post(base+"/v1/logs", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	envs := drain(out)
	if len(envs) != 1 {
		t.Fatalf("got %d envelopes, want 1", len(envs))
	}
	if age := time.Since(envs[0].Timestamp); age < 0 || age > time.Minute {
		t.Errorf("timestamp %v is not close to now", envs[0].Timestamp)
	}
}

// Gauges, sums and histograms are what SDK instrumentation actually emits, and
// each has to arrive as a series the rest of the agent can already carry.
func TestOTLPReceiver_MetricsProtobuf(t *testing.T) {
	out, base := startReceiver(t, 14343, nil)

	resp := postGolden(t, base+"/v1/metrics", goldenMetricsRequest)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	envs := drain(out)
	got := bySource(envs)

	// The summary metric in the payload is a type this agent does not decode
	// and must be dropped, not turned into an empty series.
	if len(envs) != 4 {
		t.Fatalf("got %d envelopes (%v), want 4 — gauge, sum, histogram count and sum", len(envs), sources(envs))
	}
	if _, ok := got["legacy.summary"]; ok {
		t.Error("a summary metric was decoded; it should be dropped")
	}

	gauge, ok := got["queue.depth"]
	if !ok {
		t.Fatalf("no queue.depth envelope in %v", sources(envs))
	}
	if gauge.Kind != KindMetric {
		t.Errorf("kind = %q", gauge.Kind)
	}
	if gauge.Value != 7.5 {
		t.Errorf("gauge value = %v, want 7.5", gauge.Value)
	}
	if gauge.Labels["queue"] != "orders" {
		t.Errorf("point attribute lost: %v", gauge.Labels)
	}
	if gauge.Labels["service.name"] != "checkout" || gauge.Labels["unit"] != "{item}" {
		t.Errorf("labels = %v", gauge.Labels)
	}
	if gauge.Timestamp.UnixNano() != goldenTimeNano {
		t.Errorf("timestamp = %d", gauge.Timestamp.UnixNano())
	}

	sum, ok := got["http.server.requests"]
	if !ok {
		t.Fatalf("no http.server.requests envelope")
	}
	if sum.Value != 42 {
		t.Errorf("sum value = %v, want 42 (as_int)", sum.Value)
	}
	if sum.Labels["http.route"] != "/checkout" {
		t.Errorf("labels = %v", sum.Labels)
	}

	// A histogram cannot fit in one float, so it reduces to the two figures
	// that stay correct under aggregation.
	count, ok := got["http.server.duration.count"]
	if !ok || count.Value != 3 {
		t.Errorf("histogram count = %v (present=%t), want 3", count.Value, ok)
	}
	hsum, ok := got["http.server.duration.sum"]
	if !ok || hsum.Value != 12.5 {
		t.Errorf("histogram sum = %v (present=%t), want 12.5", hsum.Value, ok)
	}
	if hsum.Labels["http.route"] != "/checkout" {
		t.Errorf("histogram labels = %v", hsum.Labels)
	}
}

// sfixed64 is two's complement, not zigzag. The two encodings agree on zero
// and disagree on every negative number, so a wrong reading here would turn an
// up-down counter that went negative into a value near 2^64.
func TestOTLPReceiver_NegativeIntSum(t *testing.T) {
	out, base := startReceiver(t, 14344, nil)

	resp := postGolden(t, base+"/v1/metrics", goldenNegativeSumRequest)
	defer resp.Body.Close()

	envs := drain(out)
	if len(envs) != 1 {
		t.Fatalf("got %d envelopes, want 1", len(envs))
	}
	if envs[0].Value != -17 {
		t.Errorf("value = %v, want -17", envs[0].Value)
	}
}

// The JSON metric encoding must agree with the protobuf one, including asInt
// arriving as a string per the proto3 JSON mapping.
func TestOTLPReceiver_MetricsJSON(t *testing.T) {
	out, base := startReceiver(t, 14345, nil)

	const body = `{"resourceMetrics":[{"resource":{"attributes":[
	  {"key":"service.name","value":{"stringValue":"checkout"}}]},
	  "scopeMetrics":[{"scope":{"name":"app.meter"},"metrics":[
	    {"name":"queue.depth","unit":"{item}","gauge":{"dataPoints":[
	      {"timeUnixNano":"1786800000000000000","asDouble":7.5,
	       "attributes":[{"key":"queue","value":{"stringValue":"orders"}}]}]}},
	    {"name":"http.server.requests","sum":{"isMonotonic":true,"dataPoints":[
	      {"timeUnixNano":"1786800000000000000","asInt":"42"}]}},
	    {"name":"http.server.duration","histogram":{"dataPoints":[
	      {"timeUnixNano":"1786800000000000000","count":"3","sum":12.5}]}}
	  ]}]}]}`

	resp, err := http.Post(base+"/v1/metrics", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	got := bySource(drain(out))
	if got["queue.depth"].Value != 7.5 {
		t.Errorf("gauge = %v", got["queue.depth"].Value)
	}
	if got["http.server.requests"].Value != 42 {
		t.Errorf("asInt string not parsed: %v", got["http.server.requests"].Value)
	}
	if got["http.server.duration.count"].Value != 3 {
		t.Errorf("histogram count = %v", got["http.server.duration.count"].Value)
	}
	if got["http.server.duration.sum"].Value != 12.5 {
		t.Errorf("histogram sum = %v", got["http.server.duration.sum"].Value)
	}
	if got["queue.depth"].Labels["service.name"] != "checkout" {
		t.Errorf("resource attribute lost: %v", got["queue.depth"].Labels)
	}
}

// A histogram that carried no sum must not gain one. Sending 0 would be
// indistinguishable from a real window whose observations were all zero.
func TestOTLPReceiver_HistogramWithoutSumEmitsOnlyCount(t *testing.T) {
	out, base := startReceiver(t, 14346, nil)

	const body = `{"resourceMetrics":[{"scopeMetrics":[{"metrics":[
	  {"name":"h","histogram":{"dataPoints":[
	    {"timeUnixNano":"1786800000000000000","count":"5"}]}}]}]}]}`
	resp, err := http.Post(base+"/v1/metrics", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	envs := drain(out)
	if len(envs) != 1 {
		t.Fatalf("got %v, want only h.count", sources(envs))
	}
	if envs[0].Source != "h.count" || envs[0].Value != 5 {
		t.Errorf("envelope = %s / %v", envs[0].Source, envs[0].Value)
	}
}

// The resource is the authority on which service emitted a metric. A point
// attribute of the same name must not overwrite it, or one mislabelled metric
// could attribute another service's data to itself.
func TestOTLPReceiver_ResourceServiceNameWinsOverPointAttribute(t *testing.T) {
	out, base := startReceiver(t, 14347, nil)

	const body = `{"resourceMetrics":[{"resource":{"attributes":[
	  {"key":"service.name","value":{"stringValue":"checkout"}}]},
	  "scopeMetrics":[{"metrics":[{"name":"m","gauge":{"dataPoints":[
	    {"timeUnixNano":"1786800000000000000","asDouble":1,
	     "attributes":[{"key":"service.name","value":{"stringValue":"impostor"}}]}]}}]}]}]}`
	resp, err := http.Post(base+"/v1/metrics", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	envs := drain(out)
	if len(envs) != 1 {
		t.Fatalf("got %d envelopes", len(envs))
	}
	if envs[0].Labels["service.name"] != "checkout" {
		t.Errorf("service.name = %q, want the resource's", envs[0].Labels["service.name"])
	}
}

// Turning a signal off must remove its route entirely, so the endpoint behaves
// exactly as it did before this receiver learned to serve it.
func TestOTLPReceiver_DisabledSignalsReturn404(t *testing.T) {
	out, base := startReceiver(t, 14348, func(c *OTLPReceiverCollector) {
		c.AcceptSignals(false, false)
	})

	for _, path := range []string{"/v1/logs", "/v1/metrics"} {
		resp, err := http.Post(base+path, "application/json", bytes.NewReader([]byte("{}")))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", path, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Traces are not gated: the listener exists for them.
	resp := postGolden(t, base+"/v1/traces", goldenBasicRequest)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("traces status = %d, want 200 — traces must stay served", resp.StatusCode)
	}
	if len(drain(out)) == 0 {
		t.Error("no trace envelope emitted while logs and metrics were disabled")
	}
}

// The new endpoints must enforce the same bearer token as the trace one. A
// receiver that authenticated traces and not metrics would be a hole in
// exactly the protection the trace path documents.
func TestOTLPReceiver_AuthAppliesToEverySignal(t *testing.T) {
	addr := "127.0.0.1:14349"
	coll := NewOTLPReceiverCollector("test-agent", addr, 4<<20, "s3cret")
	out := make(chan Envelope, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := coll.Start(ctx, out); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer coll.Stop()
	time.Sleep(150 * time.Millisecond)

	for _, path := range []string{"/v1/traces", "/v1/logs", "/v1/metrics"} {
		resp, err := http.Post("http://"+addr+path, "application/json", bytes.NewReader([]byte("{}")))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without a token = %d, want 401", path, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// And the token is accepted where it is correct.
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/logs", bytes.NewReader([]byte("{}")))
	req.Header.Set("Authorization", "Bearer s3cret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("authorised POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("authorised /v1/logs = %d, want 200", resp.StatusCode)
	}
}

// Malformed input is a client error, never a crash and never a 200 that makes
// an SDK believe its data was accepted.
func TestOTLPReceiver_MalformedPayloads(t *testing.T) {
	_, base := startReceiver(t, 14350, nil)

	cases := []struct {
		path, contentType string
		body              string
	}{
		{"/v1/logs", "application/x-protobuf", "not protobuf at all"},
		{"/v1/metrics", "application/x-protobuf", "also not protobuf"},
		{"/v1/logs", "application/json", "{not json"},
		{"/v1/metrics", "application/json", "{not json"},
	}
	for _, c := range cases {
		resp, err := http.Post(base+c.path, c.contentType, bytes.NewReader([]byte(c.body)))
		if err != nil {
			t.Fatalf("POST %s: %v", c.path, err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s (%s) = %d, want 400", c.path, c.contentType, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// GET is not how OTLP exports arrive, and answering one would be a way to make
// the receiver do work without posting anything.
func TestOTLPReceiver_RejectsNonPost(t *testing.T) {
	_, base := startReceiver(t, 14351, nil)

	for _, path := range []string{"/v1/logs", "/v1/metrics"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d, want 405", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func sources(envs []Envelope) []string {
	out := make([]string, 0, len(envs))
	for _, e := range envs {
		out = append(out, e.Source)
	}
	return out
}

// A service graph is a client span paired with a server span in another
// service, and an uninstrumented dependency is a client span with nothing on
// the other end that names its peer. Both facts live on the span, and the
// dashboard cannot derive a topology without them — so they have to survive
// the trip into the envelope.
func TestOTLPReceiver_SpanKindAndPeerAttributes(t *testing.T) {
	out, base := startReceiver(t, 14352, nil)

	const body = `{"resourceSpans":[{"resource":{"attributes":[
	  {"key":"service.name","value":{"stringValue":"orders"}}]},
	  "scopeSpans":[{"scope":{"name":"app"},"spans":[
	    {"traceId":"0102030405060708090a0b0c0d0e0f10","spanId":"1112131415161718",
	     "name":"SELECT orders","kind":3,
	     "startTimeUnixNano":"1786800000000000000","endTimeUnixNano":"1786800000020000000",
	     "attributes":[
	       {"key":"db.system","value":{"stringValue":"postgresql"}},
	       {"key":"db.name","value":{"stringValue":"ordersdb"}},
	       {"key":"net.peer.name","value":{"stringValue":"db-1.internal"}},
	       {"key":"http.method","value":{"stringValue":"GET"}}]}]}]}]}`

	resp, err := http.Post(base+"/v1/traces", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	envs := drain(out)
	if len(envs) != 1 {
		t.Fatalf("got %d envelopes, want 1", len(envs))
	}
	e := envs[0]

	// kind 3 is CLIENT. Without this the dashboard cannot tell an outbound
	// call from an ordinary nested one.
	if e.Labels["span.kind"] != "client" {
		t.Errorf("span.kind = %q, want client", e.Labels["span.kind"])
	}
	for k, want := range map[string]string{
		"db.system":     "postgresql",
		"db.name":       "ordersdb",
		"net.peer.name": "db-1.internal",
	} {
		if e.Labels[k] != want {
			t.Errorf("label %s = %q, want %q", k, e.Labels[k], want)
		}
	}
	// Only the peer-identifying attributes are promoted. Copying every
	// attribute onto labels would put unbounded high-cardinality data into the
	// series key and the dashboard payload.
	if _, present := e.Labels["http.method"]; present {
		t.Error("http.method was promoted to a label; only peer attributes should be")
	}
	// And the full attribute set is still in the payload, untouched.
	attrs, _ := e.Payload["attributes"].(map[string]any)
	if attrs["http.method"] != "GET" {
		t.Errorf("payload attributes lost http.method: %v", attrs)
	}
}

// A server span has no peer to name, and must not acquire empty labels.
func TestOTLPReceiver_ServerSpanCarriesNoPeer(t *testing.T) {
	out, base := startReceiver(t, 14353, nil)

	const body = `{"resourceSpans":[{"scopeSpans":[{"spans":[
	  {"traceId":"0102030405060708090a0b0c0d0e0f10","spanId":"2112131415161718",
	   "name":"GET /orders","kind":2,
	   "startTimeUnixNano":"1786800000000000000","endTimeUnixNano":"1786800000070000000"}]}]}]}`
	resp, err := http.Post(base+"/v1/traces", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	envs := drain(out)
	if len(envs) != 1 {
		t.Fatalf("got %d envelopes", len(envs))
	}
	if envs[0].Labels["span.kind"] != "server" {
		t.Errorf("span.kind = %q, want server", envs[0].Labels["span.kind"])
	}
	for _, k := range []string{"db.system", "peer.service", "net.peer.name"} {
		if _, present := envs[0].Labels[k]; present {
			t.Errorf("server span carries peer label %q", k)
		}
	}
}
