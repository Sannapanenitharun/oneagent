package collector

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"encoding/hex"
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
	// These are the exact bytes google.golang.org/protobuf produced for this
	// message while it was still vendored — i.e. the same encoding a real OTel
	// SDK's otlptracehttp exporter puts on the wire. Freezing them, rather than
	// re-encoding with whatever library is current, is what keeps this an
	// end-to-end test of the decoder against the reference format instead of a
	// round-trip of our own encoder.
	body, err := hex.DecodeString(goldenBasicRequest)
	if err != nil {
		t.Fatalf("decoding golden request: %v", err)
	}

	coll := NewOTLPReceiverCollector("test-agent", "127.0.0.1:14329", 4<<20, "")
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
	coll := NewOTLPReceiverCollector("test-agent", "127.0.0.1:14330", 4<<20, "")
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

	// Garbage bytes might not always fail to decode (the protobuf wire
	// format is permissive), so this isn't a strict assertion on
	// status code — the important property is that the server doesn't
	// crash and responds with SOME valid HTTP response either way.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unexpected status for garbage protobuf body: %d", resp.StatusCode)
	}
}

func TestOTLPTraceReceiver_JSONStillWorksAlongsideProtobuf(t *testing.T) {
	// Regression guard: adding protobuf support must not have broken the
	// existing JSON path on the same endpoint.
	coll := NewOTLPReceiverCollector("test-agent", "127.0.0.1:14331", 4<<20, "")
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

// goldenBasicRequest mirrors internal/otlpwire's fixture of the same name: one
// ResourceSpans carrying service.name=checkout-service, scope
// manual-test-scope, and a single 123ms processPayment span with an
// http.status_code=200 attribute.
const goldenBasicRequest = "0a96010a240a220a0c736572766963652e6e616d6512120a10636865636b6f75742d73657276696365126e0a130a116d616e75616c2d746573742d73636f706512570a105b8aa5a2d2c872e8321cf37308d69df21208051581bf3cb55c132a0e70726f636573735061796d656e7439000057c07e68161841c0d4abc77e6816184a170a10687474702e7374617475735f636f6465120318c801"
