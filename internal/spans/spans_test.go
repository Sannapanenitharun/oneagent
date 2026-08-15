package spans

import (
	"fmt"
	"testing"
	"time"

	"github.com/agent-i/agent/internal/collector"
)

func span(traceID, service, name, statusCode string, durationMs float64) collector.Envelope {
	return collector.Envelope{
		Kind:    collector.KindTrace,
		AgentID: "test-agent",
		Source:  "otlp.span",
		Labels: map[string]string{
			"trace_id":     traceID,
			"span_id":      traceID + "-01",
			"name":         name,
			"service.name": service,
			"status.code":  statusCode,
		},
		Value: durationMs,
	}
}

func bySource(envs []collector.Envelope) map[string]collector.Envelope {
	m := make(map[string]collector.Envelope, len(envs))
	for _, e := range envs {
		m[e.Source] = e
	}
	return m
}

func TestProcessor_PassesThroughNonTraceEnvelopes(t *testing.T) {
	p := New("test-agent", Config{StatsEnabled: true, SamplingEnabled: true, Rate: 0})
	for _, kind := range []collector.Kind{collector.KindMetric, collector.KindLog, collector.KindAPICall} {
		if !p.Process(collector.Envelope{Kind: kind}) {
			t.Errorf("kind %q was not forwarded; only spans are this type's business", kind)
		}
	}
}

func TestProcessor_DisabledForwardsEverything(t *testing.T) {
	p := New("test-agent", Config{})
	if p.Enabled() {
		t.Error("processor with neither stats nor sampling reports Enabled")
	}
	if !p.Process(span("abc", "svc", "GET /x", "0", 5)) {
		t.Error("with sampling off every span must be forwarded")
	}
}

// TestProcessor_StatsCountEverySpanEvenWhenSampledOut is the central guarantee:
// dropping spans must not make the counts wrong.
func TestProcessor_StatsCountEverySpanEvenWhenSampledOut(t *testing.T) {
	p := New("test-agent", Config{
		StatsEnabled:    true,
		SamplingEnabled: true,
		Rate:            0, // drop everything that is not an error
		KeepErrors:      false,
	})

	const total = 100
	forwarded := 0
	for i := 0; i < total; i++ {
		if p.Process(span(fmt.Sprintf("trace-%d", i), "svc", "GET /x", "0", float64(i))) {
			forwarded++
		}
	}

	if forwarded != 0 {
		t.Fatalf("rate 0 forwarded %d spans, want 0", forwarded)
	}

	m := bySource(p.Flush(time.Now()))
	if got := m["trace.spans"].Value; got != total {
		t.Errorf("trace.spans = %v, want %d — statistics must cover every span, not just the sampled ones", got, total)
	}
	if got := m["trace.spans.dropped"].Value; got != total {
		t.Errorf("trace.spans.dropped = %v, want %d", got, total)
	}
	if got := m["trace.spans.kept"].Value; got != 0 {
		t.Errorf("trace.spans.kept = %v, want 0", got)
	}
}

// TestProcessor_SamplingIsConsistentPerTrace: all spans of one trace must share
// a verdict, or the backend receives traces with holes in them.
func TestProcessor_SamplingIsConsistentPerTrace(t *testing.T) {
	p := New("test-agent", Config{SamplingEnabled: true, Rate: 0.5})

	for i := 0; i < 200; i++ {
		traceID := fmt.Sprintf("trace-%d", i)
		first := p.Process(span(traceID, "svc", "op-a", "0", 1))
		// Same trace, different spans — the verdict must not vary.
		for j := 0; j < 5; j++ {
			if got := p.Process(span(traceID, "svc", fmt.Sprintf("op-%d", j), "0", 1)); got != first {
				t.Fatalf("trace %s: span %d verdict %v differs from first span's %v — this produces partial traces",
					traceID, j, got, first)
			}
		}
	}
}

// TestProcessor_SamplingRateIsRoughlyHonoured checks the rate is actually
// applied, with a wide tolerance so the test is not flaky.
func TestProcessor_SamplingRateIsRoughlyHonoured(t *testing.T) {
	p := New("test-agent", Config{SamplingEnabled: true, Rate: 0.25})

	const traces = 4000
	kept := 0
	for i := 0; i < traces; i++ {
		if p.Process(span(fmt.Sprintf("trace-%d", i), "svc", "op", "0", 1)) {
			kept++
		}
	}
	ratio := float64(kept) / traces
	if ratio < 0.18 || ratio > 0.32 {
		t.Errorf("kept ratio %.3f, want roughly 0.25", ratio)
	}
}

func TestProcessor_KeepErrorsOverridesRate(t *testing.T) {
	p := New("test-agent", Config{SamplingEnabled: true, Rate: 0, KeepErrors: true})

	if !p.Process(span("t1", "svc", "op", statusCodeError, 1)) {
		t.Error("error span was dropped despite KeepErrors")
	}
	if p.Process(span("t2", "svc", "op", "0", 1)) {
		t.Error("non-error span was kept at rate 0")
	}
}

func TestProcessor_SlowThresholdOverridesRate(t *testing.T) {
	p := New("test-agent", Config{SamplingEnabled: true, Rate: 0, SlowThresholdMs: 1000})

	if !p.Process(span("t1", "svc", "op", "0", 1500)) {
		t.Error("slow span was dropped despite SlowThresholdMs")
	}
	if p.Process(span("t2", "svc", "op", "0", 999)) {
		t.Error("fast span was kept at rate 0")
	}
}

func TestProcessor_SeparatesErrorAndOkStatus(t *testing.T) {
	p := New("test-agent", Config{StatsEnabled: true})

	for i := 0; i < 7; i++ {
		p.Process(span(fmt.Sprintf("t%d", i), "svc", "op", "0", 10))
	}
	for i := 0; i < 3; i++ {
		p.Process(span(fmt.Sprintf("e%d", i), "svc", "op", statusCodeError, 20))
	}

	byStatus := map[string]float64{}
	errsByStatus := map[string]float64{}
	for _, e := range p.Flush(time.Now()) {
		switch e.Source {
		case "trace.spans":
			byStatus[e.Labels["status"]] = e.Value
		case "trace.errors":
			errsByStatus[e.Labels["status"]] = e.Value
		}
	}

	if byStatus["ok"] != 7 {
		t.Errorf("ok spans = %v, want 7", byStatus["ok"])
	}
	if byStatus["error"] != 3 {
		t.Errorf("error spans = %v, want 3", byStatus["error"])
	}
	if errsByStatus["error"] != 3 {
		t.Errorf("trace.errors for error status = %v, want 3", errsByStatus["error"])
	}
}

func TestProcessor_LatencyPercentiles(t *testing.T) {
	p := New("test-agent", Config{StatsEnabled: true})
	for i := 1; i <= 10; i++ {
		p.Process(span(fmt.Sprintf("t%d", i), "svc", "op", "0", float64(i)))
	}

	m := bySource(p.Flush(time.Now()))
	checks := map[string]float64{
		"trace.spans":        10,
		"trace.duration.avg": 5.5,
		"trace.duration.p50": 5,
		"trace.duration.p95": 10,
		"trace.duration.max": 10,
	}
	for name, want := range checks {
		if got := m[name].Value; got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
}

// TestProcessor_OverflowKeepsTotals: past MaxContexts nothing is lost, it is
// folded into a visible bucket.
func TestProcessor_OverflowKeepsTotals(t *testing.T) {
	p := New("test-agent", Config{StatsEnabled: true, MaxContexts: 2})

	const distinct = 10
	for i := 0; i < distinct; i++ {
		p.Process(span(fmt.Sprintf("t%d", i), "svc", fmt.Sprintf("op-%d", i), "0", 1))
	}

	var total float64
	sawOverflow := false
	for _, e := range p.Flush(time.Now()) {
		if e.Source != "trace.spans" {
			continue
		}
		total += e.Value
		if e.Labels["operation"] == overflowName {
			sawOverflow = true
		}
	}
	if total != distinct {
		t.Errorf("total spans across summaries = %v, want %d — spans were lost", total, distinct)
	}
	if !sawOverflow {
		t.Error("expected an overflow context past MaxContexts")
	}
}

func TestProcessor_FlushResetsWindow(t *testing.T) {
	p := New("test-agent", Config{StatsEnabled: true, SamplingEnabled: true, Rate: 1})
	p.Process(span("t1", "svc", "op", "0", 1))

	if len(p.Flush(time.Now())) == 0 {
		t.Fatal("first flush returned nothing")
	}
	if got := p.Flush(time.Now()); len(got) != 0 {
		t.Errorf("second flush returned %d envelopes, want none: %+v", len(got), got)
	}
}

// TestProcessor_MissingTraceIDIsKept: a span we cannot make a consistent
// decision about must not be dropped, since a partial trace is the outcome
// being avoided.
func TestProcessor_MissingTraceIDIsKept(t *testing.T) {
	p := New("test-agent", Config{SamplingEnabled: true, Rate: 0.0001})
	e := span("", "svc", "op", "0", 1)
	delete(e.Labels, "trace_id")
	if !p.Process(e) {
		t.Error("span without a trace_id was dropped")
	}
}
