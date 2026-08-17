package aggregate

import (
	"math"
	"math/rand"
	"sort"
	"testing"
)

// relErr is the accuracy the DEFAULT scale promises, used where the data is
// narrow enough that no downscaling happens. Where it might, tests read
// h.RelativeError() instead — a wide distribution trades accuracy for memory on
// purpose, and asserting a constant there would be asserting that the trade
// never happens.
const relErr = 0.011

func TestHistogram_EmptyIsZero(t *testing.T) {
	h := NewHistogram()
	if h.Count() != 0 || h.Sum() != 0 {
		t.Errorf("empty histogram: count=%d sum=%v, want 0/0", h.Count(), h.Sum())
	}
	for _, q := range []float64{0, 0.5, 0.99, 1} {
		if got := h.Quantile(q); got != 0 {
			t.Errorf("Quantile(%v) on empty = %v, want 0", q, got)
		}
	}
}

func TestHistogram_QuantilesAreWithinTheAccuracyBound(t *testing.T) {
	h := NewHistogram()
	var raw []float64
	// A realistic latency spread: most requests fast, a long tail.
	for i := 1; i <= 10000; i++ {
		v := float64(i%500) + 1 // 1..500 ms
		raw = append(raw, v)
		h.Observe(v)
	}
	sort.Float64s(raw)

	// 1..500 spans more buckets than the cap allows at the default scale, so
	// the histogram will have downscaled. The bound to hold it to is the one
	// actually in force, not the one it started with.
	tol := h.RelativeError()
	if h.scale == defaultHistogramScale {
		tol = relErr
	}
	for _, q := range []float64{0.5, 0.9, 0.95, 0.99} {
		want := exactQuantile(raw, q)
		got := h.Quantile(q)
		if rel := math.Abs(got-want) / want; rel > tol {
			t.Errorf("Quantile(%v) = %v, exact %v, relative error %.4f > %.4f (scale %d)",
				q, got, want, rel, tol, h.scale)
		}
	}
}

// A narrow distribution must NOT downscale, and must hold the default bound.
func TestHistogram_NarrowDistributionKeepsFullAccuracy(t *testing.T) {
	h := NewHistogram()
	var raw []float64
	for i := 1; i <= 2000; i++ {
		v := float64(i%100) + 1 // 1..100 ms
		raw = append(raw, v)
		h.Observe(v)
	}
	sort.Float64s(raw)

	if h.scale != defaultHistogramScale {
		t.Fatalf("scale dropped to %d on a 1..100 range; that should fit at the default", h.scale)
	}
	for _, q := range []float64{0.5, 0.9, 0.99} {
		want := exactQuantile(raw, q)
		got := h.Quantile(q)
		if rel := math.Abs(got-want) / want; rel > relErr {
			t.Errorf("Quantile(%v) = %v, exact %v, relative error %.4f > %.4f", q, got, want, rel, relErr)
		}
	}
}

// The terminal case in add: a value too extreme to represent even at the
// coarsest scale must still be counted, not lost and not hang the process.
func TestHistogram_ExtremeValuesTerminate(t *testing.T) {
	h := NewHistogram()
	h.Observe(1)
	for _, v := range []float64{1e-300, 1e300, math.SmallestNonzeroFloat64, math.MaxFloat64} {
		h.Observe(v)
	}
	if h.Count() != 5 {
		t.Errorf("count = %d, want 5 — no observation may be dropped", h.Count())
	}
	var bucketed uint64
	for _, c := range h.counts {
		bucketed += c
	}
	if bucketed+h.zeroCount != h.Count() {
		t.Errorf("buckets hold %d + %d zeros, want %d", bucketed, h.zeroCount, h.Count())
	}
	if len(h.counts) > h.maxBuckets {
		t.Errorf("bucket slice grew to %d, cap %d", len(h.counts), h.maxBuckets)
	}
}

func TestHistogram_HandlesFourOrdersOfMagnitude(t *testing.T) {
	h := NewHistogram()
	var raw []float64
	for _, v := range []float64{0.1, 0.5, 1, 5, 25, 100, 900, 5000, 60000} {
		for i := 0; i < 100; i++ {
			raw = append(raw, v)
			h.Observe(v)
		}
	}
	sort.Float64s(raw)

	if h.Count() != 900 {
		t.Fatalf("count = %d, want 900", h.Count())
	}
	// Downscaling may have kicked in; accuracy degrades but must stay sane and
	// nothing may be lost.
	for _, q := range []float64{0.1, 0.5, 0.9, 0.99} {
		want := exactQuantile(raw, q)
		got := h.Quantile(q)
		if rel := math.Abs(got-want) / want; rel > 0.30 {
			t.Errorf("Quantile(%v) = %v, exact %v, relative error %.3f is too large even after downscaling", q, got, want, rel)
		}
	}
	if h.Max() != 60000 || h.Min() != 0.1 {
		t.Errorf("min/max = %v/%v, want 0.1/60000", h.Min(), h.Max())
	}
}

func TestHistogram_NeverExceedsTheBucketCap(t *testing.T) {
	h := NewHistogram()
	// Values spanning a huge range force repeated downscaling.
	for e := -20; e <= 20; e++ {
		h.Observe(math.Pow(2, float64(e)))
	}
	if len(h.counts) > h.maxBuckets {
		t.Errorf("bucket slice grew to %d, cap is %d", len(h.counts), h.maxBuckets)
	}
	if h.Count() != 41 {
		t.Errorf("count = %d after downscaling, want 41 — downscaling must never lose observations", h.Count())
	}
}

func TestHistogram_ObservationsSurviveDownscaling(t *testing.T) {
	h := NewHistogram()
	var total float64
	for i := 0; i < 5000; i++ {
		v := math.Pow(10, rand.Float64()*6-3) // 0.001 .. 1000
		h.Observe(v)
		total += v
	}
	if h.Count() != 5000 {
		t.Errorf("count = %d, want 5000", h.Count())
	}
	if math.Abs(h.Sum()-total) > 1e-6*total {
		t.Errorf("sum drifted: got %v want %v", h.Sum(), total)
	}
	var bucketed uint64
	for _, c := range h.counts {
		bucketed += c
	}
	if bucketed+h.zeroCount != h.Count() {
		t.Errorf("bucket counts total %d + %d zeros != count %d — observations were lost",
			bucketed, h.zeroCount, h.Count())
	}
}

// The property the histogram exists for: two of them combine exactly.
func TestHistogram_MergeIsExactForCountAndSum(t *testing.T) {
	a, b := NewHistogram(), NewHistogram()
	all := NewHistogram()

	for i := 1; i <= 500; i++ {
		v := float64(i)
		a.Observe(v)
		all.Observe(v)
	}
	for i := 501; i <= 1500; i++ {
		v := float64(i)
		b.Observe(v)
		all.Observe(v)
	}

	a.Merge(b)

	if a.Count() != all.Count() {
		t.Errorf("merged count = %d, want %d", a.Count(), all.Count())
	}
	if math.Abs(a.Sum()-all.Sum()) > 1e-9*all.Sum() {
		t.Errorf("merged sum = %v, want %v", a.Sum(), all.Sum())
	}
	if a.Min() != all.Min() || a.Max() != all.Max() {
		t.Errorf("merged min/max = %v/%v, want %v/%v", a.Min(), a.Max(), all.Min(), all.Max())
	}
	// And the merged quantiles must match the histogram that saw everything.
	for _, q := range []float64{0.5, 0.9, 0.99} {
		m, w := a.Quantile(q), all.Quantile(q)
		if rel := math.Abs(m-w) / w; rel > relErr {
			t.Errorf("merged Quantile(%v) = %v, single-pass %v, relative error %.4f", q, m, w, rel)
		}
	}
}

func TestHistogram_MergeAcrossDifferentScales(t *testing.T) {
	fine := NewHistogram()
	coarse := NewHistogram()
	for i := 0; i < 50; i++ {
		fine.Observe(100)
	}
	// Force coarse to downscale.
	for e := -18; e <= 18; e++ {
		coarse.Observe(math.Pow(2, float64(e)))
	}
	if coarse.scale >= fine.scale {
		t.Fatalf("expected the wide histogram to have downscaled: %d vs %d", coarse.scale, fine.scale)
	}

	wantCount := fine.Count() + coarse.Count()
	fine.Merge(coarse)
	if fine.Count() != wantCount {
		t.Errorf("count after cross-scale merge = %d, want %d", fine.Count(), wantCount)
	}
	var bucketed uint64
	for _, c := range fine.counts {
		bucketed += c
	}
	if bucketed+fine.zeroCount != fine.Count() {
		t.Error("cross-scale merge lost observations")
	}
}

func TestHistogram_ZeroAndInvalidValues(t *testing.T) {
	h := NewHistogram()
	h.Observe(0)
	h.Observe(0)
	h.Observe(5)
	// Rejected outright: these are bugs in the source, not data.
	h.Observe(-1)
	h.Observe(math.NaN())
	h.Observe(math.Inf(1))

	if h.Count() != 3 {
		t.Errorf("count = %d, want 3 — negatives, NaN and Inf must not be recorded", h.Count())
	}
	if h.zeroCount != 2 {
		t.Errorf("zeroCount = %d, want 2", h.zeroCount)
	}
	if got := h.Quantile(0.5); got != 0 {
		t.Errorf("Quantile(0.5) = %v, want 0 — two of three values are zero", got)
	}
}

func TestHistogram_QuantilesAreMonotonic(t *testing.T) {
	h := NewHistogram()
	for i := 1; i <= 2000; i++ {
		h.Observe(float64(i))
	}
	prev := -1.0
	for q := 0.0; q <= 1.0; q += 0.01 {
		got := h.Quantile(q)
		if got < prev {
			t.Fatalf("Quantile went backwards at q=%.2f: %v after %v", q, got, prev)
		}
		prev = got
	}
}

func TestHistogram_QuantileStaysWithinObservedRange(t *testing.T) {
	h := NewHistogram()
	for _, v := range []float64{7, 7, 7, 7} {
		h.Observe(v)
	}
	// Bucket bounds do not land on 7 exactly; the reported value must still not
	// claim something outside what was actually seen.
	for _, q := range []float64{0, 0.25, 0.5, 0.99, 1} {
		if got := h.Quantile(q); got != 7 {
			t.Errorf("Quantile(%v) = %v, want exactly 7 — every observation was 7", q, got)
		}
	}
}

func TestHistogram_PointRoundTripsTheBuckets(t *testing.T) {
	h := NewHistogram()
	for i := 1; i <= 300; i++ {
		h.Observe(float64(i))
	}
	p := h.Point()

	if p.Count != h.Count() || p.Sum != h.Sum() || p.Scale != h.scale || p.Offset != h.offset {
		t.Error("Point does not reflect the histogram it came from")
	}
	var total uint64
	for _, c := range p.BucketCounts {
		total += c
	}
	if total+p.ZeroCount != p.Count {
		t.Errorf("point buckets total %d + %d zeros != count %d", total, p.ZeroCount, p.Count)
	}
	// The slice must be a copy: the exporter marshals it after the window has
	// been reset and reused.
	p.BucketCounts[0] = 999999
	if h.counts[0] == 999999 {
		t.Error("Point shares its bucket slice with the live histogram")
	}
}

func TestHalfIndexCeil(t *testing.T) {
	// ceil(i/2). Note -1 maps to 0, not -1: the index one scale coarser for
	// idx -1 covers (-1, -0.5], whose ceiling is 0.
	cases := map[int32]int32{0: 0, 1: 1, 2: 1, 3: 2, 4: 2, 5: 3, -1: 0, -2: -1, -3: -1, -4: -2, -5: -2}
	for in, want := range cases {
		if got := halfIndexCeil(in); got != want {
			t.Errorf("halfIndexCeil(%d) = %d, want %d", in, got, want)
		}
	}
}

func exactQuantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(q*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}
