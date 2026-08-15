package aggregate

import (
	"testing"
	"time"

	"github.com/oneagent/agent/internal/collector"
)

func apiCall(method, path, status string, durationMs float64) collector.Envelope {
	return collector.Envelope{
		Kind:    collector.KindAPICall,
		AgentID: "test-agent",
		Source:  "http.access_log:/var/log/nginx/access.log",
		Labels: map[string]string{
			"method": method,
			"path":   path,
			"status": status,
		},
		Value: durationMs,
	}
}

// bySource indexes flushed envelopes by metric name for one expected context.
func bySource(t *testing.T, envs []collector.Envelope) map[string]collector.Envelope {
	t.Helper()
	m := make(map[string]collector.Envelope, len(envs))
	for _, e := range envs {
		m[e.Source] = e
	}
	return m
}

func testAggregator(cfg Config) *Aggregator {
	cfg.Enabled = true
	return New("test-agent", cfg)
}

func TestNormalizePath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "/"},
		{"/", "/"},
		{"/api/orders", "/api/orders"},
		{"/api/orders/12345", "/api/orders/{id}"},
		{"/api/orders/12345/items/9", "/api/orders/{id}/items/{id}"},
		{"/users/550e8400-e29b-41d4-a716-446655440000", "/users/{uuid}"},
		{"/blob/9f86d081884c7d659a2feaa0c55ad015", "/blob/{hash}"},
		{"/search?q=hello&page=2", "/search"},
		{"/api/orders/12345#frag", "/api/orders/{id}"},
		{"//double//slashes//", "/double/slashes"},
		// Ordinary words made only of hex characters must survive: collapsing
		// these would quietly destroy real endpoint names.
		{"/decade", "/decade"},
		{"/faced/added", "/faced/added"},
	}
	for _, c := range cases {
		if got := NormalizePath(c.in); got != c.want {
			t.Errorf("NormalizePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizePath_TruncatesDeepPaths(t *testing.T) {
	deep := ""
	for i := 0; i < 30; i++ {
		deep += "/seg"
	}
	got := NormalizePath(deep)
	if got[len(got)-4:] != "/..." {
		t.Errorf("expected deep path to be marked truncated, got %q", got)
	}
}

// TestAggregator_PassesThroughNonAPICall: only api_call is absorbed. Metrics,
// logs and traces must reach the exporter untouched.
func TestAggregator_PassesThroughNonAPICall(t *testing.T) {
	a := testAggregator(Config{})
	for _, kind := range []collector.Kind{collector.KindMetric, collector.KindLog, collector.KindTrace} {
		if a.Add(collector.Envelope{Kind: kind}) {
			t.Errorf("kind %q was absorbed, want pass-through", kind)
		}
	}
}

func TestAggregator_DisabledAbsorbsNothing(t *testing.T) {
	a := New("test-agent", Config{Enabled: false})
	if a.Add(apiCall("GET", "/api/x", "200", 5)) {
		t.Error("disabled aggregator absorbed an envelope")
	}
}

// TestAggregator_SummarizesRequests is the core behaviour: many per-request
// events in, one set of series out, with exact counts.
func TestAggregator_SummarizesRequests(t *testing.T) {
	a := testAggregator(Config{})

	// Ten requests to the same logical endpoint, distinct only by object id,
	// with latencies 1..10ms.
	for i := 1; i <= 10; i++ {
		e := apiCall("GET", "/api/orders/"+itoa(i), "200", float64(i))
		if !a.Add(e) {
			t.Fatal("api_call was not absorbed")
		}
	}

	out := a.Flush(time.Now())
	if len(out) != 7 {
		t.Fatalf("expected 7 series for one context, got %d: %+v", len(out), out)
	}

	m := bySource(t, out)
	for _, e := range out {
		if e.Kind != collector.KindMetric {
			t.Errorf("%s: kind = %q, want metric", e.Source, e.Kind)
		}
		if e.Labels["path"] != "/api/orders/{id}" {
			t.Errorf("%s: path = %q, want normalized /api/orders/{id}", e.Source, e.Labels["path"])
		}
		if e.Labels["method"] != "GET" || e.Labels["status"] != "200" {
			t.Errorf("%s: unexpected labels %+v", e.Source, e.Labels)
		}
	}

	checks := map[string]float64{
		"http.server.requests":     10,
		"http.server.errors":       0,
		"http.server.duration.avg": 5.5,
		"http.server.duration.p50": 5, // nearest-rank over 1..10
		"http.server.duration.p95": 10,
		"http.server.duration.p99": 10,
		"http.server.duration.max": 10,
	}
	for name, want := range checks {
		e, ok := m[name]
		if !ok {
			t.Errorf("missing series %s", name)
			continue
		}
		if e.Value != want {
			t.Errorf("%s = %v, want %v", name, e.Value, want)
		}
	}
}

func TestAggregator_CountsServerErrorsOnly(t *testing.T) {
	a := testAggregator(Config{})
	// Same endpoint, different statuses — each status is its own context, so
	// check the 500 bucket specifically.
	a.Add(apiCall("GET", "/api/x", "200", 1))
	a.Add(apiCall("GET", "/api/x", "404", 1))
	a.Add(apiCall("GET", "/api/x", "500", 1))
	a.Add(apiCall("GET", "/api/x", "503", 1))

	byStatus := map[string]float64{}
	for _, e := range a.Flush(time.Now()) {
		if e.Source == "http.server.errors" {
			byStatus[e.Labels["status"]] = e.Value
		}
	}
	for status, want := range map[string]float64{"200": 0, "404": 0, "500": 1, "503": 1} {
		if byStatus[status] != want {
			t.Errorf("status %s: errors = %v, want %v", status, byStatus[status], want)
		}
	}
}

// TestAggregator_OverflowKeepsTotals is the cardinality backstop: past
// MaxContexts nothing is dropped, it is folded into a visible bucket.
func TestAggregator_OverflowKeepsTotals(t *testing.T) {
	a := testAggregator(Config{MaxContexts: 2})

	const distinct = 10
	for i := 0; i < distinct; i++ {
		// Alphabetic segments, so normalization does not collapse them.
		a.Add(apiCall("GET", "/api/"+string(rune('a'+i)), "200", 1))
	}

	var total float64
	sawOverflow := false
	for _, e := range a.Flush(time.Now()) {
		if e.Source != "http.server.requests" {
			continue
		}
		total += e.Value
		if e.Labels["path"] == overflowPath {
			sawOverflow = true
		}
	}

	if total != distinct {
		t.Errorf("total requests across summaries = %v, want %d — requests were lost", total, distinct)
	}
	if !sawOverflow {
		t.Error("expected an overflow context once MaxContexts was exceeded")
	}
}

func TestAggregator_KeepRawEventsForwardsOriginal(t *testing.T) {
	a := testAggregator(Config{KeepRawEvents: true})
	if a.Add(apiCall("GET", "/api/x", "200", 3)) {
		t.Error("with KeepRawEvents the original envelope must still pass through")
	}
	// ...and it must still be counted.
	out := a.Flush(time.Now())
	m := bySource(t, out)
	if m["http.server.requests"].Value != 1 {
		t.Errorf("request was not aggregated: %+v", out)
	}
}

// TestAggregator_FlushResetsWindow guards against a window leaking into the
// next one, which would double-count every request.
func TestAggregator_FlushResetsWindow(t *testing.T) {
	a := testAggregator(Config{})
	a.Add(apiCall("GET", "/api/x", "200", 1))

	if got := len(a.Flush(time.Now())); got != 7 {
		t.Fatalf("first flush returned %d series, want 7", got)
	}
	if got := a.Flush(time.Now()); got != nil {
		t.Errorf("second flush returned %d series, want none", len(got))
	}
}

// TestAggregator_ReservoirBoundsMemory: far more requests than MaxSamples must
// not grow the retained sample set, while counts stay exact.
func TestAggregator_ReservoirBoundsMemory(t *testing.T) {
	a := testAggregator(Config{MaxSamples: 100})
	for i := 0; i < 10000; i++ {
		a.Add(apiCall("GET", "/api/x", "200", float64(i%50)))
	}

	for key, b := range a.contexts {
		if b.lat.Len() != 100 {
			t.Errorf("context %+v retained %d samples, want the 100 cap", key, b.lat.Len())
		}
		if b.count != 10000 {
			t.Errorf("context %+v counted %d requests, want 10000 — the count must stay exact even though samples are capped", key, b.count)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
