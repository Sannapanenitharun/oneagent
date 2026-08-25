package exporter

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/agent-i/agent/internal/collector"
)

// flakyExporter fails every send until it is switched on, then records what it
// receives. It stands in for a backend that is down and later comes back.
type flakyExporter struct {
	mu   sync.Mutex
	up   bool
	seen []string
}

func (f *flakyExporter) Export(e collector.Envelope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.up {
		return errors.New("backend is down")
	}
	f.seen = append(f.seen, e.Source)
	return nil
}

func (f *flakyExporter) Close() error { return nil }

func (f *flakyExporter) recover() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.up = true
}

func (f *flakyExporter) delivered() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

// waitUntil names the condition it is waiting on, so a timeout says which
// step of the recovery never happened rather than just that one did not.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The gap this closes: before the spool, an outage deeper than the queue lost
// the oldest envelopes for good. They must now be on disk and delivered when
// the backend returns.
func TestAsyncExporter_OutageOverflowsToDiskAndRecovers(t *testing.T) {
	dir := t.TempDir()
	inner := &flakyExporter{}
	sp := testSpool(t, spoolOptions{Dir: dir, SyncInterval: time.Hour})

	// A queue of 4 against 200 envelopes: without a spool all but a handful
	// would be shed.
	a := newAsyncExporter(inner, 4, time.Second, nil, sp)

	const n = 200
	for i := 0; i < n; i++ {
		if err := a.Export(env(fmt.Sprintf("evt-%03d", i))); err != nil {
			t.Fatalf("Export: %v", err)
		}
	}

	if a.dropped.Load() != 0 {
		t.Fatalf("%d envelopes were dropped despite a spool being configured", a.dropped.Load())
	}

	inner.recover()
	waitUntil(t, "the backlog to drain", func() bool { return len(inner.delivered()) >= n })

	// Every envelope, exactly once. This is the guarantee — not ordering,
	// which the spool preserves for the bulk but cannot promise across the
	// handful in flight when the endpoint failed.
	got := inner.delivered()
	seen := make(map[string]int, n)
	for _, src := range got {
		seen[src]++
	}
	for i := 0; i < n; i++ {
		src := fmt.Sprintf("evt-%03d", i)
		switch seen[src] {
		case 1:
		case 0:
			t.Fatalf("%s was never delivered — the outage still lost data", src)
		default:
			t.Fatalf("%s was delivered %d times", src, seen[src])
		}
	}

	// The reordering window is bounded by the queue: anything beyond that
	// would mean the sender is not draining disk before memory.
	misordered := 0
	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			misordered++
		}
	}
	if misordered > 1 {
		t.Fatalf("%d order inversions in the delivered stream — the backlog was interleaved with fresh data", misordered)
	}

	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// A restart during an outage used to log a count and drop the queue. Now the
// remainder goes to disk and the next run picks it up.
func TestAsyncExporter_ShutdownSpillsToDiskAndNextRunDelivers(t *testing.T) {
	dir := t.TempDir()

	down := &flakyExporter{}
	sp1, err := openSpool(spoolOptions{Dir: dir, SyncInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	first := newAsyncExporter(down, 64, 50*time.Millisecond, nil, sp1)

	const n = 40
	for i := 0; i < n; i++ {
		if err := first.Export(env(fmt.Sprintf("evt-%02d", i))); err != nil {
			t.Fatal(err)
		}
	}
	// Let the sender trip the breaker so the shutdown path takes the
	// "endpoint is down" branch rather than trying to deliver.
	waitUntil(t, "the breaker to open", func() bool { return first.failed.Load() > 0 })

	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := len(down.delivered()); n > 0 {
		t.Fatalf("a down backend somehow received %d envelopes", n)
	}

	// Second run, same directory, backend healthy again.
	up := &flakyExporter{up: true}
	sp2, err := openSpool(spoolOptions{Dir: dir, SyncInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	second := newAsyncExporter(up, 64, time.Second, nil, sp2)
	defer second.Close()

	waitUntil(t, "the spilled envelopes to be delivered after restart", func() bool {
		return len(up.delivered()) >= n
	})
	got := up.delivered()
	if len(got) < n {
		t.Fatalf("recovered %d envelopes across the restart, want %d", len(got), n)
	}
	if got[0] != "evt-00" {
		t.Fatalf("first recovered envelope is %q, want evt-00", got[0])
	}
}

// The spool must never become a reason the agent stops collecting. A broken
// spool degrades to the old shed-oldest behaviour.
func TestAsyncExporter_BrokenSpoolFallsBackToShedding(t *testing.T) {
	inner := &flakyExporter{}
	sp := testSpool(t, spoolOptions{SyncInterval: time.Hour})
	// Close it out from under the exporter: every Append now fails.
	if err := sp.Close(); err != nil {
		t.Fatal(err)
	}

	a := newAsyncExporter(inner, 4, 50*time.Millisecond, nil, sp)
	defer a.Close()

	for i := 0; i < 100; i++ {
		if err := a.Export(env(fmt.Sprintf("evt-%03d", i))); err != nil {
			t.Fatalf("Export returned an error instead of degrading: %v", err)
		}
	}
	if a.spoolErrs.Load() == 0 {
		t.Fatal("spool write failures were not counted")
	}
	if a.dropped.Load() == 0 {
		t.Fatal("nothing was shed, so the fallback path never ran")
	}
}

// Delivered-from-spool envelopes must settle their sources, exactly as
// queue-delivered ones do — otherwise a tailed file would never advance past
// anything that took the disk path.
func TestAsyncExporter_RetiresEnvelopesDeliveredFromSpool(t *testing.T) {
	rec := &retireRecorder{}
	inner := &flakyExporter{}
	sp := testSpool(t, spoolOptions{SyncInterval: time.Hour})
	a := newAsyncExporter(inner, 2, time.Second, rec.fn(), sp)
	defer a.Close()

	const n = 50
	for i := 0; i < n; i++ {
		if err := a.Export(env(fmt.Sprintf("evt-%02d", i))); err != nil {
			t.Fatal(err)
		}
	}
	inner.recover()
	waitUntil(t, "delivery", func() bool { return len(inner.delivered()) >= n })
	waitUntil(t, "retirement", func() bool { return rec.count() >= n })
}

// A restart must not wait for the backlog. The spool exists so the data
// survives without being shipped first; an agent that drained 128 MiB before
// exiting would turn every systemd restart into the outage the bounded drain
// was added to prevent.
func TestAsyncExporter_ShutdownDoesNotWaitForTheBacklog(t *testing.T) {
	dir := t.TempDir()

	// Fill a spool directly, so the exporter starts life with a backlog.
	loader, err := openSpool(spoolOptions{Dir: dir, SyncInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20000; i++ {
		if err := loader.Append(env(fmt.Sprintf("evt-%05d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := loader.Close(); err != nil {
		t.Fatal(err)
	}

	sp, err := openSpool(spoolOptions{Dir: dir, SyncInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	// A backend that is up but slow: without the shutdown check, the sender
	// would grind through all 20000 before looking at stopCh.
	slow := &slowExporter{delay: time.Millisecond}
	a := newAsyncExporter(slow, 16, 100*time.Millisecond, nil, sp)

	// Let it get properly started on the backlog first.
	waitUntil(t, "the backlog to start draining", func() bool { return slow.count() > 0 })

	start := time.Now()
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("shutdown took %v — the sender drained the backlog instead of leaving it on disk", took)
	}
	if slow.count() >= 20000 {
		t.Fatal("the whole backlog was shipped at shutdown; nothing was left for the next run")
	}
}

type slowExporter struct {
	delay time.Duration
	mu    sync.Mutex
	n     int
}

func (s *slowExporter) Export(collector.Envelope) error {
	time.Sleep(s.delay)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return nil
}

func (s *slowExporter) Close() error { return nil }

func (s *slowExporter) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}
