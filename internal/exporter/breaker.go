package exporter

import (
	"math"
	"math/rand"
	"time"
)

// breaker is a per-endpoint circuit breaker sitting in front of delivery.
//
// Without it, a backend that is down costs the full client timeout plus the
// in-send retry ladder on EVERY envelope: ~10s per attempt, three attempts,
// so roughly 40 seconds of blocked sender per envelope, forever, for a host
// that is never going to answer. The retry ladder inside postWithRetry is the
// right response to a blip; it is the wrong response to an outage, because it
// re-learns the same fact on every single send.
//
// The state machine is the standard three-state one:
//
//	closed    — everything is delivered. A failure opens the breaker.
//	open      — nothing is attempted until the backoff expires. The first
//	            attempt after expiry moves to halfOpen.
//	halfOpen  — exactly one probe is in flight. Its result decides: a failure
//	            re-opens with a longer backoff, a success steps the error count
//	            down. The count has to reach zero before the breaker closes, so
//	            an endpoint that recovers is not immediately hit with the full
//	            backlog it just failed to absorb.
//
// One deliberate difference from the usual implementation, and it matters
// enough to spell out: a breaker normally fails requests fast while open.
// Doing that here would be actively harmful. Our queue is bounded and drops
// the OLDEST envelope to make room, so failing fast would let the sender drain
// the queue at full speed and discard everything, converting "the newest 4096
// envelopes survive an outage" into "nothing survives an outage". So the
// breaker does not reject envelopes — it tells the sender to STOP DEQUEUING.
// The queue then fills and sheds oldest-first exactly as it does under any
// other backpressure, and when the endpoint returns, the freshest data is
// still there to send.
type breaker struct {
	base         time.Duration
	max          time.Duration
	factor       float64
	recoveryStep int

	state breakerState
	errs  int
	until time.Time

	// jitter spreads retry attempts so a fleet of agents pointed at the same
	// endpoint does not reconnect in lockstep and knock it over again.
	// Injectable so tests are deterministic.
	jitter func(time.Duration) time.Duration
}

type breakerState int

const (
	breakerClosed breakerState = iota
	breakerHalfOpen
	breakerOpen
)

func (s breakerState) String() string {
	switch s {
	case breakerHalfOpen:
		return "half-open"
	case breakerOpen:
		return "open"
	default:
		return "closed"
	}
}

const (
	defaultBreakerBase = 500 * time.Millisecond
	defaultBreakerMax  = 60 * time.Second
	// factor 2 doubles the wait per consecutive failure. Below 2 the intervals
	// overlap and the breaker spends longer than intended in open.
	defaultBreakerFactor = 2.0
	// recoveryStep is how many errors one successful probe forgives. At 1 the
	// climb down mirrors the climb up, so an endpoint that was down for a long
	// time is not instantly trusted with full traffic.
	defaultRecoveryStep = 1
)

func newBreaker() *breaker {
	return &breaker{
		base:         defaultBreakerBase,
		max:          defaultBreakerMax,
		factor:       defaultBreakerFactor,
		recoveryStep: defaultRecoveryStep,
		jitter:       fullJitter,
	}
}

// fullJitter returns a random duration in [d/2, d]. Halving the floor keeps
// the backoff meaningful while still de-synchronising a fleet.
func fullJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(half)+1))
}

// allow reports whether the sender may attempt a delivery now, and if not, how
// long to wait before asking again. A zero wait with allow=false never happens;
// callers can treat wait as a safe sleep duration.
func (b *breaker) allow(now time.Time) (bool, time.Duration) {
	switch b.state {
	case breakerClosed:
		return true, 0
	case breakerHalfOpen:
		// A probe is already out. Waiting for its verdict is the whole point of
		// this state — a second concurrent probe would tell us nothing new and
		// would double the load on an endpoint we already suspect.
		//
		// There is only one sender goroutine, so in practice this is reached
		// only if a probe result is still being recorded; a short wait is
		// enough and there is no deadlock risk.
		return false, b.base
	default: // open
		if wait := b.until.Sub(now); wait > 0 {
			return false, wait
		}
		// Backoff expired: let exactly one envelope through as a probe.
		b.state = breakerHalfOpen
		return true, 0
	}
}

// failure records a failed delivery.
func (b *breaker) failure(now time.Time) {
	switch b.state {
	case breakerClosed, breakerHalfOpen:
		b.errs++
		b.until = now.Add(b.backoff())
		b.state = breakerOpen
	case breakerOpen:
		// A late result from an attempt that started before we opened. It tells
		// us nothing about the current state and must not extend the backoff,
		// or a burst of in-flight failures would multiply one outage into a very
		// long silence.
	}
}

// success records a successful delivery.
func (b *breaker) success(now time.Time) {
	switch b.state {
	case breakerClosed:
		// Already healthy.
	case breakerHalfOpen:
		b.errs -= b.recoveryStep
		if b.errs <= 0 {
			b.errs = 0
			b.state = breakerClosed
			return
		}
		b.until = now.Add(b.backoff())
		b.state = breakerOpen
	case breakerOpen:
		// Same reasoning as the failure case: we cannot tell whether this
		// success predates the failure that opened us, so it is not evidence.
	}
}

// backoff is base * factor^(errs-1), capped at max, then jittered.
func (b *breaker) backoff() time.Duration {
	if b.errs <= 0 {
		return 0
	}
	d := float64(b.base) * math.Pow(b.factor, float64(b.errs-1))
	if d > float64(b.max) || math.IsInf(d, 1) {
		d = float64(b.max)
	}
	return b.jitter(time.Duration(d))
}

// tripped reports whether the breaker is currently withholding traffic. Used
// only for reporting, never for a control decision.
func (b *breaker) tripped() bool { return b.state != breakerClosed }
