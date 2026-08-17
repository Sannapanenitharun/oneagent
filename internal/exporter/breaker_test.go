package exporter

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/agent-i/agent/internal/collector"
)

// noJitter makes the backoff deterministic so tests assert on the ladder
// itself rather than on a random sample of it.
func newTestBreaker() *breaker {
	b := newBreaker()
	b.jitter = func(d time.Duration) time.Duration { return d }
	return b
}

func TestBreaker_ClosedAllowsEverything(t *testing.T) {
	b := newTestBreaker()
	now := time.Now()

	for i := 0; i < 100; i++ {
		ok, wait := b.allow(now)
		if !ok || wait != 0 {
			t.Fatalf("closed breaker must allow with no wait, got ok=%t wait=%v", ok, wait)
		}
		b.success(now)
	}
	if b.tripped() {
		t.Fatal("breaker tripped despite an unbroken run of successes")
	}
}

func TestBreaker_OpensOnFailureAndBacksOffExponentially(t *testing.T) {
	b := newTestBreaker()
	now := time.Now()

	want := []time.Duration{
		defaultBreakerBase,     // 1st failure
		defaultBreakerBase * 2, // 2nd
		defaultBreakerBase * 4, // 3rd
		defaultBreakerBase * 8, // 4th
	}

	for i, w := range want {
		b.failure(now)
		ok, wait := b.allow(now)
		if ok {
			t.Fatalf("failure %d: breaker should be open", i+1)
		}
		if wait != w {
			t.Errorf("failure %d: backoff = %v, want %v", i+1, wait, w)
		}
		// Expire the window so the next failure is recorded from halfOpen
		// rather than being ignored as a late result.
		now = now.Add(w)
		if ok, _ := b.allow(now); !ok {
			t.Fatalf("failure %d: expected a probe to be allowed once the backoff expired", i+1)
		}
	}
}

func TestBreaker_BackoffIsCapped(t *testing.T) {
	b := newTestBreaker()
	now := time.Now()

	for i := 0; i < 40; i++ {
		b.failure(now)
		_, wait := b.allow(now)
		now = now.Add(wait)
		b.allow(now) // move to halfOpen so the next failure counts
	}
	b.failure(now)
	_, wait := b.allow(now)
	if wait > defaultBreakerMax {
		t.Errorf("backoff %v exceeded the cap %v", wait, defaultBreakerMax)
	}
	if wait != defaultBreakerMax {
		t.Errorf("after 40 failures backoff should sit at the cap, got %v", wait)
	}
}

func TestBreaker_HalfOpenAdmitsExactlyOneProbe(t *testing.T) {
	b := newTestBreaker()
	now := time.Now()

	b.failure(now)
	now = now.Add(defaultBreakerBase)

	ok, _ := b.allow(now)
	if !ok {
		t.Fatal("first attempt after the backoff expired should be admitted as a probe")
	}
	if b.state != breakerHalfOpen {
		t.Fatalf("state = %v, want half-open", b.state)
	}
	// The probe's verdict has not arrived, so nothing else may go out.
	if ok, _ := b.allow(now); ok {
		t.Error("a second envelope was admitted while a probe was still in flight")
	}
}

func TestBreaker_RecoveryStepsDownRatherThanSnappingShut(t *testing.T) {
	b := newTestBreaker()
	now := time.Now()

	// Three consecutive failures.
	for i := 0; i < 3; i++ {
		b.failure(now)
		_, wait := b.allow(now)
		now = now.Add(wait)
		b.allow(now)
	}
	if b.errs != 3 {
		t.Fatalf("errs = %d, want 3", b.errs)
	}

	// One good probe forgives one error but must NOT fully close: an endpoint
	// that just came back should not immediately receive full traffic.
	b.success(now)
	if b.state == breakerClosed {
		t.Fatal("breaker closed after a single successful probe following 3 failures")
	}
	if b.errs != 2 {
		t.Errorf("errs = %d after one success, want 2", b.errs)
	}

	// Two more good probes clear it.
	for i := 0; i < 2; i++ {
		_, wait := b.allow(now)
		now = now.Add(wait)
		b.allow(now)
		b.success(now)
	}
	if b.state != breakerClosed {
		t.Errorf("state = %v after recovery, want closed", b.state)
	}
	if ok, _ := b.allow(now); !ok {
		t.Error("recovered breaker is still withholding traffic")
	}
}

func TestBreaker_IgnoresLateResultsWhileOpen(t *testing.T) {
	b := newTestBreaker()
	now := time.Now()

	b.failure(now)
	_, first := b.allow(now)

	// Results from attempts that started before the breaker opened must not
	// extend the backoff, or one outage multiplies into a very long silence.
	for i := 0; i < 10; i++ {
		b.failure(now)
	}
	_, after := b.allow(now)
	if after != first {
		t.Errorf("late failures changed the backoff: %v -> %v", first, after)
	}
	if b.errs != 1 {
		t.Errorf("errs = %d, want 1 — late failures should not be counted", b.errs)
	}

	// A late success is equally uninformative and must not close the breaker.
	b.success(now)
	if b.state != breakerOpen {
		t.Errorf("state = %v after a late success, want open", b.state)
	}
}

func TestFullJitter_StaysInRange(t *testing.T) {
	for i := 0; i < 500; i++ {
		d := 800 * time.Millisecond
		got := fullJitter(d)
		if got < d/2 || got > d {
			t.Fatalf("jitter %v outside [%v, %v]", got, d/2, d)
		}
	}
	if fullJitter(0) != 0 {
		t.Error("jitter of zero should be zero")
	}
}

// failingExporter fails a configurable number of times, then succeeds.
type failingExporter struct {
	mu       sync.Mutex
	attempts int
	failFor  int
}

func (f *failingExporter) Export(collector.Envelope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.attempts <= f.failFor {
		return errors.New("endpoint down")
	}
	return nil
}
func (f *failingExporter) Close() error { return nil }
func (f *failingExporter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

// The behaviour that matters most: during an outage the queue must still be
// holding the freshest envelopes, not have been drained and discarded.
func TestAsyncExporter_OpenBreakerRetainsQueuedEnvelopes(t *testing.T) {
	inner := &failingExporter{failFor: 1 << 30} // never recovers
	a := newAsyncExporter(inner, 8, 50*time.Millisecond, nil)
	defer a.Close()

	// First envelope trips the breaker.
	if err := a.Export(collector.Envelope{Source: "trip"}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	waitFor(t, time.Second, func() bool { return inner.count() >= 1 })

	// Give the sender a chance to (wrongly) drain everything.
	for i := 0; i < 8; i++ {
		if err := a.Export(collector.Envelope{Source: "held"}); err != nil {
			t.Fatalf("Export: %v", err)
		}
	}
	time.Sleep(150 * time.Millisecond)

	if got := len(a.queue); got == 0 {
		t.Fatal("queue was drained to empty while the endpoint was down — envelopes were discarded instead of retained")
	}
	// It must also not be hammering the dead endpoint once per envelope.
	if got := inner.count(); got > 3 {
		t.Errorf("made %d attempts against a down endpoint; the breaker should have paused delivery", got)
	}
}

func TestAsyncExporter_ResumesAfterEndpointRecovers(t *testing.T) {
	inner := &failingExporter{failFor: 1}
	a := newAsyncExporter(inner, 16, time.Second, nil)
	defer a.Close()

	// The first send fails and opens the breaker.
	if err := a.Export(collector.Envelope{Source: "first"}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool { return inner.count() >= 1 })

	// The half-open probe needs something to send, so queue more work. Once the
	// backoff expires this is delivered, the probe succeeds, and the breaker
	// closes.
	for i := 0; i < 3; i++ {
		if err := a.Export(collector.Envelope{Source: "after"}); err != nil {
			t.Fatalf("Export: %v", err)
		}
	}
	waitFor(t, 3*time.Second, func() bool { return inner.count() >= 4 })

	if a.breaker.tripped() {
		t.Error("breaker still tripped after the endpoint recovered")
	}
	if got := len(a.queue); got != 0 {
		t.Errorf("queue still holds %d envelopes after recovery", got)
	}
}

func waitFor(t *testing.T, limit time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", limit)
}
