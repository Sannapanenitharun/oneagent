package collector

import (
	"testing"

	"github.com/agent-i/agent/internal/otlpwire"
)

// The parent link is what makes a set of spans a trace rather than a bag of
// timings. It was being parsed off the wire and then dropped, so every trace
// the agent re-exported arrived flat — no waterfall, no service graph, no way
// to find the root. These pin both decode paths.

func TestSpanToEnvelopeProto_CarriesParentSpanID(t *testing.T) {
	sp := &otlpwire.Span{
		TraceID:           []byte{0x5b, 0x8a, 0xa5, 0xa2, 0xd2, 0xc8, 0x72, 0xe8, 0x32, 0x1c, 0xf3, 0x73, 0x08, 0xd6, 0x9d, 0xf2},
		SpanID:            []byte{0x05, 0x15, 0x81, 0xbf, 0x3c, 0xb5, 0x5c, 0x13},
		ParentSpanID:      []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00, 0x11},
		Name:              "childOp",
		StartTimeUnixNano: 1735689600000000000,
		EndTimeUnixNano:   1735689600050000000,
		Attributes: []*otlpwire.KeyValue{
			{Key: "k", Value: &otlpwire.AnyValue{Kind: otlpwire.ValueString, Str: "v"}},
		},
	}

	env := spanToEnvelopeProto("agent-1", "checkout", "scope", sp)
	if got := env.Labels["parent_span_id"]; got != "aabbccddeeff0011" {
		t.Errorf("parent_span_id = %q, want aabbccddeeff0011", got)
	}
	if env.Labels["span_id"] != "051581bf3cb55c13" {
		t.Errorf("span_id = %q", env.Labels["span_id"])
	}
}

// A root span has no parent, and OTLP identifies the root by that absence.
// Emitting an empty string would make every root look parented to nothing,
// which is a different claim than "this is the root".
func TestSpanToEnvelopeProto_RootSpanHasNoParentLabel(t *testing.T) {
	sp := &otlpwire.Span{
		TraceID:           []byte{0x01},
		SpanID:            []byte{0x02},
		Name:              "rootOp",
		StartTimeUnixNano: 1735689600000000000,
		EndTimeUnixNano:   1735689600010000000,
	}
	env := spanToEnvelopeProto("agent-1", "checkout", "", sp)
	if v, present := env.Labels["parent_span_id"]; present {
		t.Errorf("root span carries parent_span_id=%q, want the key absent", v)
	}
}

func TestSpanToEnvelopeJSON_CarriesParentSpanID(t *testing.T) {
	sp := otlpSpan{
		TraceID:           "5b8aa5a2d2c872e8321cf37308d69df2",
		SpanID:            "051581bf3cb55c13",
		ParentSpanID:      "aabbccddeeff0011",
		Name:              "childOp",
		StartTimeUnixNano: "1735689600000000000",
		EndTimeUnixNano:   "1735689600050000000",
	}
	env := spanToEnvelopeJSON("agent-1", "checkout", "scope", sp)
	if got := env.Labels["parent_span_id"]; got != "aabbccddeeff0011" {
		t.Errorf("parent_span_id = %q, want aabbccddeeff0011", got)
	}
}

func TestSpanToEnvelopeJSON_RootSpanHasNoParentLabel(t *testing.T) {
	sp := otlpSpan{
		TraceID:           "5b8aa5a2d2c872e8321cf37308d69df2",
		SpanID:            "051581bf3cb55c13",
		Name:              "rootOp",
		StartTimeUnixNano: "1735689600000000000",
		EndTimeUnixNano:   "1735689600010000000",
	}
	env := spanToEnvelopeJSON("agent-1", "checkout", "", sp)
	if v, present := env.Labels["parent_span_id"]; present {
		t.Errorf("root span carries parent_span_id=%q, want the key absent", v)
	}
}
