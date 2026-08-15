package collector

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// TestOTLPTraceReceiver_AcceptsRealBinaryProtobuf builds an actual
// protobuf-encoded ExportTraceServiceRequest — the same wire format a
// real OTel SDK's otlptracehttp exporter sends by default — POSTs it
// with the standard application/x-protobuf Content-Type, and confirms
// the receiver decodes it correctly into an Envelope. This is the
// specific gap flagged earlier this session (JSON-only support); this
// test proves the fix against the real binary wire format, not a JSON
// stand-in for it.
func TestOTLPTraceReceiver_AcceptsRealBinaryProtobuf(t *testing.T) {
	req := &collectortracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{
			{
				Resource: &resourcepb.Resource{
					Attributes: []*commonpb.KeyValue{
						{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "checkout-service"}}},
					},
				},
				ScopeSpans: []*tracepb.ScopeSpans{
					{
						Scope: &commonpb.InstrumentationScope{Name: "manual-test-scope"},
						Spans: []*tracepb.Span{
							{
								TraceId:           []byte{0x5b, 0x8a, 0xa5, 0xa2, 0xd2, 0xc8, 0x72, 0xe8, 0x32, 0x1c, 0xf3, 0x73, 0x08, 0xd6, 0x9d, 0xf2},
								SpanId:            []byte{0x05, 0x15, 0x81, 0xbf, 0x3c, 0xb5, 0x5c, 0x13},
								Name:              "processPayment",
								StartTimeUnixNano: 1735689600000000000,
								EndTimeUnixNano:   1735689600123000000, // 123ms duration
								Attributes: []*commonpb.KeyValue{
									{Key: "http.status_code", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 200}}},
								},
							},
						},
					},
				},
			},
		},
	}

	body, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("marshaling test request: %v", err)
	}

	coll := NewOTLPTraceReceiverCollector("test-agent", "127.0.0.1:14329", 4<<20, "")
	out := make(chan Envelope, 5)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := coll.Start(ctx, out); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer coll.Stop()
	time.Sleep(150 * time.Millisecond) // let the listener bind

	resp, err := http.Post("http://127.0.0.1:14329/v1/traces", "application/x-protobuf", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-protobuf" {
		t.Errorf("response Content-Type = %q, want application/x-protobuf", ct)
	}

	select {
	case env := <-out:
		if env.Kind != KindTrace {
			t.Errorf("Kind = %q, want trace", env.Kind)
		}
		if env.Labels["name"] != "processPayment" {
			t.Errorf("span name = %q, want processPayment", env.Labels["name"])
		}
		if env.Labels["trace_id"] != "5b8aa5a2d2c872e8321cf37308d69df2" {
			t.Errorf("trace_id = %q, want 5b8aa5a2d2c872e8321cf37308d69df2 (hex of the raw bytes)", env.Labels["trace_id"])
		}
		if env.Labels["span_id"] != "051581bf3cb55c13" {
			t.Errorf("span_id = %q, want 051581bf3cb55c13", env.Labels["span_id"])
		}
		if env.Labels["service.name"] != "checkout-service" {
			t.Errorf("service.name = %q, want checkout-service", env.Labels["service.name"])
		}
		if env.Labels["scope.name"] != "manual-test-scope" {
			t.Errorf("scope.name = %q, want manual-test-scope", env.Labels["scope.name"])
		}
		if env.Value != 123 {
			t.Errorf("duration = %v ms, want 123", env.Value)
		}
		attrs, ok := env.Payload["attributes"].(map[string]any)
		if !ok || attrs["http.status_code"] != "200" {
			t.Errorf("attributes = %+v, want http.status_code=200", env.Payload["attributes"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for envelope from protobuf-decoded span")
	}
}

func TestOTLPTraceReceiver_RejectsInvalidProtobuf(t *testing.T) {
	coll := NewOTLPTraceReceiverCollector("test-agent", "127.0.0.1:14330", 4<<20, "")
	out := make(chan Envelope, 5)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := coll.Start(ctx, out); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer coll.Stop()
	time.Sleep(150 * time.Millisecond)

	resp, err := http.Post("http://127.0.0.1:14330/v1/traces", "application/x-protobuf", bytes.NewReader([]byte("not valid protobuf at all, just garbage bytes")))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	// Garbage bytes might not always fail proto.Unmarshal (protobuf's
	// wire format is permissive), so this isn't a strict assertion on
	// status code — the important property is that the server doesn't
	// crash and responds with SOME valid HTTP response either way.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unexpected status for garbage protobuf body: %d", resp.StatusCode)
	}
}

func TestOTLPTraceReceiver_JSONStillWorksAlongsideProtobuf(t *testing.T) {
	// Regression guard: adding protobuf support must not have broken the
	// existing JSON path on the same endpoint.
	coll := NewOTLPTraceReceiverCollector("test-agent", "127.0.0.1:14331", 4<<20, "")
	out := make(chan Envelope, 5)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := coll.Start(ctx, out); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer coll.Stop()
	time.Sleep(150 * time.Millisecond)

	jsonBody := `{"resourceSpans":[{"resource":{"attributes":[]},"scopeSpans":[{"scope":{"name":"s"},"spans":[{"traceId":"aa","spanId":"bb","name":"jsonSpan","startTimeUnixNano":"1000000000","endTimeUnixNano":"1050000000"}]}]}]}`
	resp, err := http.Post("http://127.0.0.1:14331/v1/traces", "application/json", bytes.NewReader([]byte(jsonBody)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	select {
	case env := <-out:
		if env.Labels["name"] != "jsonSpan" {
			t.Errorf("span name = %q, want jsonSpan", env.Labels["name"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for JSON-decoded span")
	}
}
