package otlpwire

import (
	"encoding/hex"
	"strconv"
	"testing"
	"time"
)

// The two payloads below are not hand-written: they are the exact bytes
// google.golang.org/protobuf produced for these messages while it was still
// vendored, captured before it was removed. Decoding them here is therefore a
// comparison against the reference implementation's real output rather than
// against this package's own idea of the format — which is the only version of
// this test worth having.
const (
	goldenBasic = "0a96010a240a220a0c736572766963652e6e616d6512120a10636865636b6f75742d73657276696365126e0a130a116d616e75616c2d746573742d73636f706512570a105b8aa5a2d2c872e8321cf37308d69df21208051581bf3cb55c132a0e70726f636573735061796d656e7439000057c07e68161841c0d4abc77e6816184a170a10687474702e7374617475735f636f6465120318c801"

	goldenRich = "0ab7010a190a170a0c736572766963652e6e616d6512070a057376632d621299010a100a0773636f70652d621205312e322e331284010a100102030405060708090a0b0c0d0e0f1012080909090909090909220807070707070707072a076368696c644f7030023900002a36fe9c9717410065f753fe9c97174a0a0a017312050a037374724a100a0169120b18d6ffffffffffffffff014a0e0a01641209210000000000000c404a070a0162120210017a081204626f6f6d1802"
)

func mustDecode(t *testing.T, h string) *ExportTraceServiceRequest {
	t.Helper()
	raw, err := hex.DecodeString(h)
	if err != nil {
		t.Fatalf("bad fixture hex: %v", err)
	}
	req, err := UnmarshalExportTraceServiceRequest(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return req
}

func TestDecode_MatchesReferenceEncoderOutput_Basic(t *testing.T) {
	req := mustDecode(t, goldenBasic)

	if len(req.ResourceSpans) != 1 {
		t.Fatalf("resource spans = %d, want 1", len(req.ResourceSpans))
	}
	rs := req.ResourceSpans[0]

	if len(rs.Resource.Attributes) != 1 {
		t.Fatalf("resource attrs = %d, want 1", len(rs.Resource.Attributes))
	}
	if got := rs.Resource.Attributes[0].Key; got != "service.name" {
		t.Errorf("resource attr key = %q, want service.name", got)
	}
	if got := rs.Resource.Attributes[0].Value.String(); got != "checkout-service" {
		t.Errorf("service.name = %q, want checkout-service", got)
	}

	if len(rs.ScopeSpans) != 1 {
		t.Fatalf("scope spans = %d, want 1", len(rs.ScopeSpans))
	}
	ss := rs.ScopeSpans[0]
	if got := ss.Scope.Name; got != "manual-test-scope" {
		t.Errorf("scope name = %q, want manual-test-scope", got)
	}

	if len(ss.Spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(ss.Spans))
	}
	sp := ss.Spans[0]

	if got := hex.EncodeToString(sp.TraceID); got != "5b8aa5a2d2c872e8321cf37308d69df2" {
		t.Errorf("trace id = %s", got)
	}
	if got := hex.EncodeToString(sp.SpanID); got != "051581bf3cb55c13" {
		t.Errorf("span id = %s", got)
	}
	if len(sp.ParentSpanID) != 0 {
		t.Errorf("parent span id = %x, want empty on a root span", sp.ParentSpanID)
	}
	if sp.Name != "processPayment" {
		t.Errorf("name = %q", sp.Name)
	}
	if sp.StartTimeUnixNano != 1735689600000000000 {
		t.Errorf("start = %d", sp.StartTimeUnixNano)
	}
	if sp.EndTimeUnixNano != 1735689600123000000 {
		t.Errorf("end = %d", sp.EndTimeUnixNano)
	}
	// The duration the receiver derives from those two timestamps.
	if ms := float64(sp.EndTimeUnixNano-sp.StartTimeUnixNano) / 1e6; ms != 123 {
		t.Errorf("duration = %v ms, want 123", ms)
	}

	if len(sp.Attributes) != 1 {
		t.Fatalf("span attrs = %d, want 1", len(sp.Attributes))
	}
	if sp.Attributes[0].Key != "http.status_code" {
		t.Errorf("attr key = %q", sp.Attributes[0].Key)
	}
	if got := sp.Attributes[0].Value.String(); got != "200" {
		t.Errorf("http.status_code = %q, want 200", got)
	}
}

func TestDecode_MatchesReferenceEncoderOutput_Rich(t *testing.T) {
	req := mustDecode(t, goldenRich)
	rs := req.ResourceSpans[0]
	ss := rs.ScopeSpans[0]
	sp := ss.Spans[0]

	if got := ss.Scope.Name; got != "scope-b" {
		t.Errorf("scope name = %q", got)
	}
	if got := ss.Scope.Version; got != "1.2.3" {
		t.Errorf("scope version = %q", got)
	}
	if got := hex.EncodeToString(sp.ParentSpanID); got != "0707070707070707" {
		t.Errorf("parent span id = %s — the trace's shape depends on this", got)
	}
	if sp.Kind != 2 { // SPAN_KIND_SERVER
		t.Errorf("kind = %d, want 2", sp.Kind)
	}
	if sp.Status == nil {
		t.Fatal("status missing")
	}
	if sp.Status.Code != 2 { // STATUS_CODE_ERROR
		t.Errorf("status code = %d, want 2", sp.Status.Code)
	}
	if sp.Status.Message != "boom" {
		t.Errorf("status message = %q", sp.Status.Message)
	}

	// One attribute per AnyValue kind the agent renders.
	want := map[string]string{
		"s": "str",
		"i": "-42", // negative int64: a ten-byte two's-complement varint
		"d": "3.5",
		"b": "true",
	}
	if len(sp.Attributes) != len(want) {
		t.Fatalf("attrs = %d, want %d", len(sp.Attributes), len(want))
	}
	for _, a := range sp.Attributes {
		if got := a.Value.String(); got != want[a.Key] {
			t.Errorf("attr %q = %q, want %q", a.Key, got, want[a.Key])
		}
	}
}

// The success response carries no fields, so it encodes to nothing. Verified
// against the reference encoder, which also produced zero bytes.
func TestEmptyResponseIsZeroBytes(t *testing.T) {
	if b := MarshalEmptyExportTraceServiceResponse(); len(b) != 0 {
		t.Errorf("response = %x, want zero bytes", b)
	}
}

// Unknown fields must be walked past, not rejected: an OTLP producer newer than
// this agent will send fields it has never heard of, and refusing them would
// turn a routine upgrade on the sending side into silent trace loss here.
func TestDecode_SkipsUnknownFields(t *testing.T) {
	raw, _ := hex.DecodeString(goldenBasic)
	// Field 99, wire type 0 (varint), value 1 — appended at the top level.
	extended := append(append([]byte{}, raw...), 0xd8, 0x06, 0x01)

	req, err := UnmarshalExportTraceServiceRequest(extended)
	if err != nil {
		t.Fatalf("unknown field rejected: %v", err)
	}
	if len(req.ResourceSpans) != 1 {
		t.Fatalf("resource spans = %d, want the known fields still decoded", len(req.ResourceSpans))
	}
	if req.ResourceSpans[0].ScopeSpans[0].Spans[0].Name != "processPayment" {
		t.Error("known fields were disturbed by an unknown one")
	}
}

// Malformed input arrives from the network, so every one of these must return
// an error rather than panic.
func TestDecode_MalformedInputDoesNotPanic(t *testing.T) {
	full, _ := hex.DecodeString(goldenBasic)

	cases := map[string][]byte{
		"empty":                 {},
		"lone tag":              {0x0a},
		"length exceeds buffer": {0x0a, 0x7f, 0x01, 0x02},
		"varint never ends":     {0x08, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		"field number zero":     {0x00, 0x01},
		"group wire type":       {0x0b, 0x01},
		"garbage":               {0xff, 0xfe, 0xfd, 0xfc},
	}
	// Every truncation of a valid message is also fair game.
	for i := 1; i < len(full); i += 7 {
		cases["truncated at "+strconv.Itoa(i)] = full[:i]
	}

	for name, in := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: panicked: %v", name, r)
				}
			}()
			// Errors are fine and expected; only a panic or a hang is a bug.
			_, _ = UnmarshalExportTraceServiceRequest(in)
		}()
	}
}

// A payload can describe nesting far deeper than it is long, which is the
// standard way to blow a recursive-descent decoder's stack.
//
// This decoder is not vulnerable, and the reason is worth stating: it does not
// recurse generically. Each decode function only descends into the specific
// nested messages the OTLP trace schema allows, and that schema is finite —
// request > resource_spans > {resource | scope_spans > spans} > attributes >
// value bottoms out at about five levels. AnyValue's two self-referential
// cases (array_value, kvlist_value) are skipped rather than decoded, so the
// one place recursion could become unbounded is closed off.
//
// So this asserts the property that actually matters — a hostile payload
// terminates without panicking — rather than that the depth guard fires, which
// with today's schema it never will.
func TestDecode_DeepNestingTerminatesSafely(t *testing.T) {
	// Each wrapper is "field 1, wire type 2, length N" around the previous.
	payload := []byte{}
	for len(payload) < 0x7f {
		payload = append([]byte{0x0a, byte(len(payload))}, payload...)
	}

	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("deeply nested payload panicked: %v", r)
			}
			close(done)
		}()
		// Accepting or rejecting are both fine. Hanging or crashing are not.
		_, _ = UnmarshalExportTraceServiceRequest(payload)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("decoding a deeply nested payload did not terminate")
	}
}
