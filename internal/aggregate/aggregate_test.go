package aggregate

import (
	"math"
	"testing"
	"time"

	"github.com/agent-i/agent/internal/collector"
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
	// Seven scalar series plus the distribution they were derived from.
	if len(out) != 8 {
		t.Fatalf("expected 7 series + 1 histogram for one context, got %d: %+v", len(out), out)
	}

	m := bySource(t, out)
	for _, e := range out {
		if e.Kind != collector.KindMetric && e.Kind != collector.KindHistogram {
			t.Errorf("%s: kind = %q, want metric or histogram", e.Source, e.Kind)
		}
		if e.Labels["path"] != "/api/orders/{id}" {
			t.Errorf("%s: path = %q, want normalized /api/orders/{id}", e.Source, e.Labels["path"])
		}
		if e.Labels["method"] != "GET" || e.Labels["status"] != "200" {
			t.Errorf("%s: unexpected labels %+v", e.Source, e.Labels)
		}
	}

	// Counts, mean and max are tracked exactly.
	exact := map[string]float64{
		"http.server.requests":     10,
		"http.server.errors":       0,
		"http.server.duration.avg": 5.5,
		"http.server.duration.max": 10,
	}
	for name, want := range exact {
		e, ok := m[name]
		if !ok {
			t.Errorf("missing series %s", name)
			continue
		}
		if e.Value != want {
			t.Errorf("%s = %v, want %v", name, e.Value, want)
		}
	}

	// Percentiles come from a bucketed histogram, so they carry a bounded
	// relative error rather than being exact — the deliberate trade that makes
	// them mergeable across hosts and windows.
	const tolerance = 0.011
	approx := map[string]float64{
		"http.server.duration.p50": 5, // nearest-rank over 1..10
		"http.server.duration.p95": 10,
		"http.server.duration.p99": 10,
	}
	for name, want := range approx {
		e, ok := m[name]
		if !ok {
			t.Errorf("missing series %s", name)
			continue
		}
		if rel := math.Abs(e.Value-want) / want; rel > tolerance {
			t.Errorf("%s = %v, want %v within %.1f%%", name, e.Value, want, tolerance*100)
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

	if got := len(a.Flush(time.Now())); got != 8 {
		t.Fatalf("first flush returned %d series, want 8 (7 scalars + the distribution)", got)
	}
	if got := a.Flush(time.Now()); got != nil {
		t.Errorf("second flush returned %d series, want none", len(got))
	}
}

// TestAggregator_HistogramBoundsMemory: far more requests than any sample cap
// must not grow the per-context footprint, while counts stay exact.
//
// This replaced a reservoir. The bound is now the bucket count rather than a
// sample count, and it is a better bound: a reservoir's error grows the further
// into the tail you look, because fewer and fewer retained samples support the
// answer. Bucket error is relative and constant wherever you look.
func TestAggregator_HistogramBoundsMemory(t *testing.T) {
	a := testAggregator(Config{})
	for i := 0; i < 10000; i++ {
		a.Add(apiCall("GET", "/api/x", "200", float64(i%50)))
	}

	for key, b := range a.contexts {
		if got := len(b.lat.counts); got > b.lat.maxBuckets {
			t.Errorf("context %+v grew to %d buckets, cap is %d", key, got, b.lat.maxBuckets)
		}
		if b.count != 10000 {
			t.Errorf("context %+v counted %d requests, want 10000 — the count must stay exact regardless of how the distribution is stored", key, b.count)
		}
		if b.lat.Count() != 10000 {
			t.Errorf("context %+v histogram saw %d observations, want 10000 — bounding memory must never discard one", key, b.lat.Count())
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

// The percentiles keep every existing consumer working; the distribution is
// what makes the numbers useful across more than one host. Both must be
// emitted, and the distribution must actually carry its buckets.
func TestAggregator_EmitsTheDistributionAlongsideThePercentiles(t *testing.T) {
	a := testAggregator(Config{})
	for i := 1; i <= 200; i++ {
		a.Add(apiCall("GET", "/api/x", "200", float64(i)))
	}

	var hist *collector.Envelope
	seenPercentiles := 0
	for _, e := range a.Flush(time.Now()) {
		switch {
		case e.Kind == collector.KindHistogram:
			cp := e
			hist = &cp
		case e.Source == "http.server.duration.p50",
			e.Source == "http.server.duration.p95",
			e.Source == "http.server.duration.p99":
			seenPercentiles++
		}
	}

	if seenPercentiles != 3 {
		t.Errorf("emitted %d percentile series, want 3 — existing consumers must not break", seenPercentiles)
	}
	if hist == nil {
		t.Fatal("no histogram envelope was emitted; percentiles alone cannot be merged across hosts")
	}
	if hist.Source != "http.server.duration" {
		t.Errorf("histogram source = %q, want http.server.duration", hist.Source)
	}
	raw, ok := hist.Payload[collector.HistogramPointKey]
	if !ok {
		t.Fatal("histogram envelope carries no distribution")
	}
	pt, ok := raw.(collector.HistogramPoint)
	if !ok {
		t.Fatalf("payload is %T, want collector.HistogramPoint", raw)
	}
	if pt.Count != 200 {
		t.Errorf("distribution count = %d, want 200", pt.Count)
	}
	var bucketed uint64
	for _, c := range pt.BucketCounts {
		bucketed += c
	}
	if bucketed+pt.ZeroCount != pt.Count {
		t.Errorf("bucket counts total %d + %d zeros != %d observations", bucketed, pt.ZeroCount, pt.Count)
	}
	if hist.Labels["path"] == "" {
		t.Error("the distribution lost the labels that say which endpoint it describes")
	}
}
