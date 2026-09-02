package collector

import (
	"strings"
	"testing"
)

func excEvent(attrs map[string]string) spanEventView {
	return spanEventView{Name: eventNameException, Attributes: attrs}
}

// The case this exists for. OTel records a thrown error as a span EVENT, not
// as a span attribute, so a receiver reading only attributes sees a failed
// span with nothing saying what failed.
func TestApplyExceptionEvent_CopiesTheFields(t *testing.T) {
	labels := map[string]string{"name": "GET /checkout"}
	applyExceptionEvent(labels, []spanEventView{
		excEvent(map[string]string{
			attrExcType:    "ValueError",
			attrExcMessage: "bad input",
			attrExcStack:   "at handler()\nat main()",
		}),
	})

	if labels[attrExcType] != "ValueError" {
		t.Errorf("%s = %q", attrExcType, labels[attrExcType])
	}
	if labels[attrExcMessage] != "bad input" {
		t.Errorf("%s = %q", attrExcMessage, labels[attrExcMessage])
	}
	if labels[attrExcStack] != "at handler()\nat main()" {
		t.Errorf("%s = %q", attrExcStack, labels[attrExcStack])
	}
	if labels["name"] != "GET /checkout" {
		t.Error("existing labels must survive")
	}
	if _, ok := labels["exception.count"]; ok {
		t.Error("a single exception must not carry a count")
	}
}

// A span with no exception must be untouched — this is nearly every span.
func TestApplyExceptionEvent_LeavesCleanSpansAlone(t *testing.T) {
	for _, events := range [][]spanEventView{
		nil,
		{},
		{{Name: "cache.miss", Attributes: map[string]string{"key": "k1"}}},
	} {
		labels := map[string]string{"name": "GET /ok"}
		applyExceptionEvent(labels, events)
		if len(labels) != 1 {
			t.Errorf("events %v added %v", events, labels)
		}
	}
}

// The first, not the last: a span that records several usually records a cause
// and then the wrappers that re-threw it, and the innermost one is what an
// operator groups by.
func TestApplyExceptionEvent_KeepsTheFirstAndCountsTheRest(t *testing.T) {
	labels := map[string]string{}
	applyExceptionEvent(labels, []spanEventView{
		{Name: "cache.miss"},
		excEvent(map[string]string{attrExcType: "ValueError", attrExcMessage: "root cause"}),
		excEvent(map[string]string{attrExcType: "HandlerError", attrExcMessage: "wrapped"}),
	})

	if labels[attrExcType] != "ValueError" {
		t.Errorf("%s = %q, want the first (innermost) exception", attrExcType, labels[attrExcType])
	}
	if labels[attrExcMessage] != "root cause" {
		t.Errorf("%s = %q", attrExcMessage, labels[attrExcMessage])
	}
	// Visible rather than implied: something was set aside.
	if labels["exception.count"] != "2" {
		t.Errorf("exception.count = %q, want 2", labels["exception.count"])
	}
}

// An unbounded stack trace rides in a label map that is re-exported, batched,
// gzipped and stored. Truncated and marked, not dropped: the first frames are
// the ones that identify a fault.
func TestApplyExceptionEvent_TruncatesAStackTrace(t *testing.T) {
	huge := strings.Repeat("at frame()\n", 5000)
	labels := map[string]string{}
	applyExceptionEvent(labels, []spanEventView{
		excEvent(map[string]string{attrExcType: "Deep", attrExcStack: huge}),
	})

	got := labels[attrExcStack]
	if len(got) > maxStacktraceBytes+32 {
		t.Errorf("stacktrace kept %d bytes, want it capped near %d", len(got), maxStacktraceBytes)
	}
	if !strings.HasSuffix(got, "truncated") {
		t.Errorf("a shortened trace must say so; got tail %q", got[max(0, len(got)-24):])
	}
	if !strings.HasPrefix(got, "at frame()") {
		t.Error("truncation must keep the START of the trace — that is the part that identifies the fault")
	}
}

// A trace exactly at the bound is kept whole; one byte over is cut. Tested
// because an off-by-one silently mangles a legitimate value.
func TestApplyExceptionEvent_StacktraceBoundary(t *testing.T) {
	atBound := strings.Repeat("x", maxStacktraceBytes)
	labels := map[string]string{}
	applyExceptionEvent(labels, []spanEventView{excEvent(map[string]string{attrExcStack: atBound})})
	if labels[attrExcStack] != atBound {
		t.Error("a trace of exactly maxStacktraceBytes was truncated")
	}

	overBound := atBound + "x"
	labels = map[string]string{}
	applyExceptionEvent(labels, []spanEventView{excEvent(map[string]string{attrExcStack: overBound})})
	if labels[attrExcStack] == overBound {
		t.Error("a trace one byte over the bound was kept whole")
	}
}

// Some SDKs record a message and no type. Grouping needs a key, and a bucket
// called "unknown" beats a row that disappears from the view.
func TestApplyExceptionEvent_MessageWithoutTypeStillGroups(t *testing.T) {
	labels := map[string]string{}
	applyExceptionEvent(labels, []spanEventView{
		excEvent(map[string]string{attrExcMessage: "something failed"}),
	})
	if labels[attrExcType] != "unknown" {
		t.Errorf("%s = %q, want a groupable fallback", attrExcType, labels[attrExcType])
	}
}

// An exception event carrying nothing usable must not invent a row.
func TestApplyExceptionEvent_EmptyEventAddsNothing(t *testing.T) {
	labels := map[string]string{}
	applyExceptionEvent(labels, []spanEventView{excEvent(map[string]string{attrExcType: ""})})
	if len(labels) != 0 {
		t.Errorf("got %v, want nothing added for an exception with no usable fields", labels)
	}
}

// End to end through the JSON receiver path, which is what an SDK configured
// for OTLP/JSON sends.
func TestSpanToEnvelopeJSON_CarriesTheException(t *testing.T) {
	str := func(s string) *string { return &s }
	sp := otlpSpan{
		TraceID:           "0102030405060708090a0b0c0d0e0f10",
		SpanID:            "0102030405060708",
		Name:              "POST /orders",
		StartTimeUnixNano: "1700000000000000000",
		EndTimeUnixNano:   "1700000000250000000",
		Events: []otlpSpanEvent{
			{Name: "cache.miss"},
			{Name: "exception", Attributes: []otlpKeyValue{
				{Key: attrExcType, Value: otlpAnyValue{StringValue: str("SQLError")}},
				{Key: attrExcMessage, Value: otlpAnyValue{StringValue: str("deadlock detected")}},
			}},
		},
	}

	env := spanToEnvelopeJSON("agent-1", "checkout", "scope", sp)
	if env.Labels[attrExcType] != "SQLError" {
		t.Errorf("%s = %q", attrExcType, env.Labels[attrExcType])
	}
	if env.Labels[attrExcMessage] != "deadlock detected" {
		t.Errorf("%s = %q", attrExcMessage, env.Labels[attrExcMessage])
	}
	// The rest of the span must be unaffected.
	if env.Labels["name"] != "POST /orders" || env.Value != 250 {
		t.Errorf("span fields disturbed: name=%q value=%v", env.Labels["name"], env.Value)
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
