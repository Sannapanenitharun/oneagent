package aggregate

import (
	"math"
	"math/rand"
	"sort"
	"time"
)

// Reservoir retains a bounded, uniformly-sampled subset of the values offered
// to it, which is what lets percentiles be computed over unbounded traffic in
// bounded memory.
//
// It uses Algorithm R: once full, observation number n replaces a random
// existing sample with probability max/n. Every observation therefore has the
// same chance of being retained no matter when it arrived — important because
// the alternative (keep the first N, or keep the last N) systematically biases
// percentiles toward whichever part of the window happened to be sampled.
//
// Counts are deliberately NOT tracked here. Callers keep exact counts
// separately, so a capped sample set never makes a request count approximate.
type Reservoir struct {
	max     int
	samples []float64
	seen    int64
	rnd     *rand.Rand
}

func NewReservoir(max int) *Reservoir {
	if max <= 0 {
		max = 1
	}
	return &Reservoir{
		max: max,
		rnd: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (r *Reservoir) Observe(v float64) {
	r.seen++
	if len(r.samples) < r.max {
		r.samples = append(r.samples, v)
		return
	}
	if j := r.rnd.Int63n(r.seen); j < int64(r.max) {
		r.samples[j] = v
	}
}

// Len is the number of retained samples, which is min(observations, max).
func (r *Reservoir) Len() int { return len(r.samples) }

// Sorted returns a sorted copy, so callers can take several percentiles
// without re-sorting and without mutating the reservoir.
func (r *Reservoir) Sorted() []float64 {
	out := append([]float64(nil), r.samples...)
	sort.Float64s(out)
	return out
}

// Percentile reads from an already-sorted slice using nearest-rank, which
// needs no interpolation and so never reports a latency that was not actually
// observed.
func Percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}
