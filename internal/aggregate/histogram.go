package aggregate

import (
	"math"

	"github.com/agent-i/agent/internal/collector"
)

// Histogram is a base-2 exponential histogram: the representation OpenTelemetry
// defines for distributions, and the reason it is here rather than a reservoir.
//
// A reservoir keeps a bounded random sample and computes percentiles from it.
// That works on one host for one window and stops working the moment you have
// more than one of either, because percentiles do not compose: you cannot
// average the p99 of ten hosts into a fleet p99, and once the window is flushed
// the samples are gone, so "p90 over the last hour" can never be answered after
// the fact. Its error is also unbounded in the tail — the further out the
// quantile, the fewer retained samples support it.
//
// A bucketed histogram fixes both. Buckets are exponentially spaced, so the
// error is RELATIVE (a fixed percentage of the value) rather than absolute,
// which is what you want for latency spanning microseconds to minutes. And two
// histograms merge by adding their bucket counts, so a backend can combine
// hosts and windows and compute any quantile it likes, later.
//
// Bucket k covers (base^(k-1), base^k] where base = 2^(2^-scale). Reporting a
// bucket's upper bound therefore overstates the true value by at most
// (base - 1) — about 1.1% at the default scale. A distribution too wide to fit
// in maxBuckets is stored one scale coarser, which doubles that bound each
// time; RelativeError reports what is actually in force.
//
// Not safe for concurrent use. Like everything else in this package it lives on
// the daemon's single goroutine.
type Histogram struct {
	scale     int32
	offset    int32 // index of counts[0]
	counts    []uint64
	zeroCount uint64

	count uint64
	sum   float64
	min   float64
	max   float64

	maxBuckets int
}

const (
	// defaultHistogramScale gives base = 2^(1/64) ≈ 1.0109, so a reported value
	// is within ~1.1% of the true one. Finer than any latency SLO is stated to.
	defaultHistogramScale = 6
	// defaultMaxBuckets bounds memory per context. The number is chosen from
	// the dynamic range it buys: at the default scale, N buckets span a ratio of
	// 2^(N/64), so 640 covers 1024:1 — 1ms to 1s, or 100µs to 100ms. That is the
	// range real request latency actually occupies, which matters because the
	// first version used 320 and covered only 32:1, so essentially every
	// distribution downscaled on contact and the documented accuracy was never
	// the accuracy in force.
	//
	// Worst case is 640 * 8 bytes = 5 KiB per context, and the slice only grows
	// to the span actually observed. Wider distributions still downscale rather
	// than being clipped — no observation is ever discarded.
	defaultMaxBuckets = 640
	// minHistogramScale stops downscaling somewhere still useful. At scale -4,
	// base = 2^16, which is coarse enough that anything wider is pathological.
	minHistogramScale = -4
)

// NewHistogram returns an empty histogram at the default scale.
func NewHistogram() *Histogram {
	return &Histogram{
		scale:      defaultHistogramScale,
		maxBuckets: defaultMaxBuckets,
	}
}

// Observe records one value. Negative values are ignored: every distribution
// this agent measures is a duration or a size, and a negative one is a bug in
// the source rather than something to represent faithfully.
func (h *Histogram) Observe(v float64) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return
	}

	if h.count == 0 || v < h.min {
		h.min = v
	}
	if h.count == 0 || v > h.max {
		h.max = v
	}
	h.count++
	h.sum += v

	if v == 0 {
		h.zeroCount++
		return
	}
	h.add(h.index(v), 1)
}

// index returns the bucket index for v at the current scale.
func (h *Histogram) index(v float64) int32 {
	return int32(math.Ceil(math.Ldexp(math.Log2(v), int(h.scale))))
}

// add places count into the bucket at idx, growing or rescaling as needed.
//
// Iterative rather than recursive, and with an explicit terminal case. An
// earlier version recursed after downscaling and looped forever once the scale
// had bottomed out: downscaling could no longer shrink the index range, so the
// same index was retried indefinitely. Anything that cannot be represented even
// at the coarsest scale is folded into the nearest edge bucket instead —
// bounded distortion for a pathological value, rather than losing it or
// hanging.
func (h *Histogram) add(idx int32, count uint64) {
	if len(h.counts) == 0 {
		h.offset = idx
		h.counts = []uint64{count}
		return
	}

	for {
		lo := h.offset
		hi := h.offset + int32(len(h.counts)) - 1

		if idx >= lo && idx <= hi {
			h.counts[idx-lo] += count
			return
		}

		newLo, newHi := lo, hi
		if idx < lo {
			newLo = idx
		} else {
			newHi = idx
		}

		if int(newHi-newLo+1) <= h.maxBuckets {
			grow := make([]uint64, newHi-newLo+1)
			copy(grow[lo-newLo:], h.counts)
			h.counts = grow
			h.offset = newLo
			h.counts[idx-newLo] += count
			return
		}

		if h.scale <= minHistogramScale {
			// Coarsest representable scale and it still does not fit. Fold into
			// whichever end it fell outside, so the observation is still counted
			// and the totals stay exact.
			if idx < lo {
				h.counts[0] += count
			} else {
				h.counts[len(h.counts)-1] += count
			}
			return
		}

		h.downscaleOnce()
		idx = halfIndexCeil(idx)
	}
}

// downscaleOnce merges adjacent bucket pairs and drops the scale by one.
func (h *Histogram) downscaleOnce() {
	if len(h.counts) == 0 {
		h.scale--
		return
	}
	oldOffset := h.offset
	newLo := halfIndexCeil(oldOffset)
	newHi := halfIndexCeil(oldOffset + int32(len(h.counts)) - 1)
	merged := make([]uint64, newHi-newLo+1)
	for i, c := range h.counts {
		if c == 0 {
			continue
		}
		merged[halfIndexCeil(oldOffset+int32(i))-newLo] += c
	}
	h.counts = merged
	h.offset = newLo
	h.scale--
}

// halfIndexCeil maps a bucket index to its index one scale coarser. It is
// ceil(i/2), which is the correct mapping for ceil-based indexing — Go's
// truncating division gets the negative half right but not the positive one, so
// the two cases are spelled out.
func halfIndexCeil(i int32) int32 {
	if i >= 0 {
		return (i + 1) / 2
	}
	return -((-i) / 2)
}

// RelativeError is the largest fraction by which a reported quantile can
// overstate the true value at the current scale. It is not a constant: a
// distribution wide enough to force downscaling trades accuracy for memory, and
// this is how a caller finds out that happened.
func (h *Histogram) RelativeError() float64 {
	return math.Exp2(math.Exp2(-float64(h.scale))) - 1
}

// Count returns how many values were observed.
func (h *Histogram) Count() uint64 { return h.count }

// Sum returns the total of all observed values.
func (h *Histogram) Sum() float64 { return h.sum }

// Min returns the smallest observed value, or 0 if none.
func (h *Histogram) Min() float64 { return h.min }

// Max returns the largest observed value, or 0 if none.
func (h *Histogram) Max() float64 { return h.max }

// Quantile returns the value at q in [0,1], estimated from the buckets.
//
// The answer is the upper bound of the bucket the rank falls in, so it is never
// an underestimate — a p99 that reads slightly high is a far less dangerous
// error than one that reads low.
func (h *Histogram) Quantile(q float64) float64 {
	if h.count == 0 {
		return 0
	}
	if q <= 0 {
		return h.min
	}
	if q >= 1 {
		return h.max
	}

	// Rank of the value we want, 1-based.
	rank := uint64(math.Ceil(q * float64(h.count)))
	if rank == 0 {
		rank = 1
	}

	var seen uint64
	if h.zeroCount > 0 {
		seen = h.zeroCount
		if seen >= rank {
			return 0
		}
	}
	for i, c := range h.counts {
		if c == 0 {
			continue
		}
		seen += c
		if seen >= rank {
			v := h.bucketUpperBound(h.offset + int32(i))
			// Never report beyond what was actually seen: the top bucket's
			// upper bound can exceed the largest observation.
			if v > h.max {
				return h.max
			}
			if v < h.min {
				return h.min
			}
			return v
		}
	}
	return h.max
}

// bucketUpperBound is base^k = 2^(k / 2^scale).
func (h *Histogram) bucketUpperBound(k int32) float64 {
	return math.Exp2(math.Ldexp(float64(k), -int(h.scale)))
}

// Merge folds other into h. This is the property the whole type exists for:
// two windows, or two hosts, combine exactly by adding bucket counts.
//
// Both sides are brought to a common scale FIRST, and the scale is then chosen
// so the union of their ranges fits before any bucket is copied. An earlier
// version added src's buckets one at a time and let h downscale part-way
// through the loop — every index after that point was interpreted at a scale it
// was not computed for, and the counts landed in the wrong buckets. The symptom
// was a merged p50 that read as the maximum.
func (h *Histogram) Merge(other *Histogram) {
	if other == nil || other.count == 0 {
		return
	}

	if h.count == 0 || other.min < h.min {
		h.min = other.min
	}
	if h.count == 0 || other.max > h.max {
		h.max = other.max
	}
	h.count += other.count
	h.sum += other.sum
	h.zeroCount += other.zeroCount

	src := other.clone()
	for src.scale > h.scale {
		src.downscaleOnce()
	}
	for h.scale > src.scale {
		h.downscaleOnce()
	}

	if len(src.counts) == 0 {
		return
	}
	if len(h.counts) == 0 {
		h.counts = append([]uint64(nil), src.counts...)
		h.offset = src.offset
		return
	}

	// Shrink both, in step, until the combined range fits.
	for h.scale > minHistogramScale {
		lo := min(h.offset, src.offset)
		hi := max(h.offset+int32(len(h.counts))-1, src.offset+int32(len(src.counts))-1)
		if int(hi-lo+1) <= h.maxBuckets {
			break
		}
		h.downscaleOnce()
		src.downscaleOnce()
	}

	lo := min(h.offset, src.offset)
	hi := max(h.offset+int32(len(h.counts))-1, src.offset+int32(len(src.counts))-1)
	if int(hi-lo+1) > h.maxBuckets {
		// Still too wide at the coarsest scale. Fall back to per-bucket add,
		// which folds anything unrepresentable into the nearest edge. Safe to
		// loop here precisely because h can no longer downscale underneath us.
		for i, c := range src.counts {
			if c != 0 {
				h.add(src.offset+int32(i), c)
			}
		}
		return
	}

	merged := make([]uint64, hi-lo+1)
	copy(merged[h.offset-lo:], h.counts)
	for i, c := range src.counts {
		if c != 0 {
			merged[src.offset+int32(i)-lo] += c
		}
	}
	h.counts = merged
	h.offset = lo
}

func (h *Histogram) clone() *Histogram {
	c := *h
	c.counts = append([]uint64(nil), h.counts...)
	return &c
}

// Point renders the histogram in the shape the OTLP exporter needs. Returning a
// collector type keeps the exporter free of any knowledge of this package.
func (h *Histogram) Point() collector.HistogramPoint {
	return collector.HistogramPoint{
		Count:        h.count,
		Sum:          h.sum,
		Min:          h.min,
		Max:          h.max,
		Scale:        h.scale,
		ZeroCount:    h.zeroCount,
		Offset:       h.offset,
		BucketCounts: append([]uint64(nil), h.counts...),
	}
}
