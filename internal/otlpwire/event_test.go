package otlpwire

import (
	"encoding/binary"
	"testing"
)

// The golden fixtures in trace_test.go are the reference protobuf runtime's
// real output, captured before it was removed, and they are the strongest
// evidence this package has. They predate span events, and the encoder that
// produced them is gone, so a fixture covering events cannot be made the same
// way.
//
// What follows instead is an explicit encoder written from the wire format:
// every tag names its field number and wire type, so the bytes it produces can
// be checked against the OTLP proto by reading rather than by trust. It is
// weaker evidence than a golden fixture — it tests the decoder against this
// file's understanding of the format — and the field numbers it asserts
// (Span.events = 11, Event.name = 2, Event.attributes = 3) are the part worth
// checking against the spec if this ever disagrees with a real SDK.

const (
	wireVarintT  = 0
	wireFixed64T = 1
	wireBytesT   = 2
)

func tagOf(field, wire int) []byte {
	return appendUvarint(nil, uint64(field)<<3|uint64(wire))
}

func appendUvarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// lenField encodes one length-delimited field.
func lenField(field int, payload []byte) []byte {
	out := tagOf(field, wireBytesT)
	out = appendUvarint(out, uint64(len(payload)))
	return append(out, payload...)
}

func strField(field int, s string) []byte { return lenField(field, []byte(s)) }

func fixed64Field(field int, v uint64) []byte {
	out := tagOf(field, wireFixed64T)
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], v)
	return append(out, buf[:]...)
}

// keyValue is KeyValue{ key = 1, value = 2 { string_value = 1 } }.
func keyValue(k, v string) []byte {
	anyVal := strField(1, v)
	kv := append(strField(1, k), lenField(2, anyVal)...)
	return kv
}

func TestDecodeSpan_ReadsExceptionEvent(t *testing.T) {
	// Event{ name = 2, attributes = 3 }, with the timestamp as fixed64 —
	// the field whose wire type, if guessed as varint, desynchronises
	// everything after it.
	event := fixed64Field(1, 1700000000000000000)
	event = append(event, strField(2, "exception")...)
	event = append(event, lenField(3, keyValue("exception.type", "ValueError"))...)
	event = append(event, lenField(3, keyValue("exception.message", "bad input"))...)

	span := strField(5, "GET /checkout")
	span = append(span, lenField(11, event)...) // Span.events = 11
	// A field after events, so a mis-sized events read shows up as corruption
	// here rather than passing silently.
	span = append(span, lenField(15, appendUvarint(tagOf(3, wireVarintT), 2))...) // Status{code = 3} = ERROR

	scope := lenField(2, span)     // ScopeSpans.spans = 2
	resource := lenField(2, scope) // ResourceSpans.scope_spans = 2
	req := lenField(1, resource)   // request.resource_spans = 1

	got, err := UnmarshalExportTraceServiceRequest(req)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.ResourceSpans) != 1 || len(got.ResourceSpans[0].ScopeSpans) != 1 ||
		len(got.ResourceSpans[0].ScopeSpans[0].Spans) != 1 {
		t.Fatalf("unexpected shape: %+v", got)
	}
	sp := got.ResourceSpans[0].ScopeSpans[0].Spans[0]

	if sp.Name != "GET /checkout" {
		t.Errorf("name = %q", sp.Name)
	}
	// The field after events must still be readable, which is what proves the
	// events sub-message consumed exactly its own bytes.
	if sp.Status == nil || sp.Status.Code != 2 {
		t.Errorf("status = %+v, want code 2 — a mis-sized events read would corrupt this", sp.Status)
	}
	if len(sp.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(sp.Events))
	}
	ev := sp.Events[0]
	if ev.Name != "exception" {
		t.Errorf("event name = %q, want %q", ev.Name, "exception")
	}
	if ev.TimeUnixNano != 1700000000000000000 {
		t.Errorf("event time = %d, want the fixed64 value intact", ev.TimeUnixNano)
	}
	attrs := map[string]string{}
	for _, a := range ev.Attributes {
		attrs[a.Key] = a.Value.String()
	}
	if attrs["exception.type"] != "ValueError" || attrs["exception.message"] != "bad input" {
		t.Errorf("event attributes = %v", attrs)
	}
}

// A span with no events must decode exactly as it always did — this is the
// path every currently-instrumented application takes.
func TestDecodeSpan_NoEventsIsUnchanged(t *testing.T) {
	req := mustDecode(t, goldenBasic)
	sp := req.ResourceSpans[0].ScopeSpans[0].Spans[0]
	if len(sp.Events) != 0 {
		t.Errorf("events = %d on a fixture that has none", len(sp.Events))
	}
	if sp.Name == "" {
		t.Error("adding the events branch broke an existing golden fixture")
	}
}
