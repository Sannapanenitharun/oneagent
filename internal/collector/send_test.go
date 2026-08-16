package collector

import (
	"context"
	"sync"
	"testing"
	"time"
)

func testEnv(src string) Envelope {
	return Envelope{Kind: KindMetric, AgentID: "h", Source: src, Timestamp: time.Now(), Value: 1}
}

// The healthy path must behave exactly as a bare channel send did: accepted
// immediately, nothing counted, no timer involved.
func TestSendGate_DeliversWhenThereIsRoom(t *testing.T) {
	g := newSendGate("test")
	out := make(chan Envelope, 4)

	for i := 0; i < 4; i++ {
		if !g.send(context.Background(), out, testEnv("m")) {
			t.Fatalf("send %d was refused with room in the buffer", i)
		}
	}
	if len(out) != 4 {
		t.Errorf("buffer holds %d, want 4", len(out))
	}
	if g.Dropped() != 0 {
		t.Errorf("dropped = %d on a healthy pipeline, want 0", g.Dropped())
	}
}

// A consumer that resumes within the window must not lose anything — the whole
// point of waiting rather than dropping immediately.
func TestSendGate_WaitsForASlowConsumerRatherThanDropping(t *testing.T) {
	g := newSendGate("test")
	g.timeout = 2 * time.Second
	out := make(chan Envelope) // unbuffered: every send needs a live receiver

	go func() {
		time.Sleep(150 * time.Millisecond)
		<-out
	}()

	if !g.send(context.Background(), out, testEnv("m")) {
		t.Fatal("send was dropped even though the consumer resumed well inside the timeout")
	}
	if g.Dropped() != 0 {
		t.Errorf("dropped = %d, want 0", g.Dropped())
	}
}

// The failure this exists for: the consumer has genuinely stopped. The send
// must give up, count it, and let the collector keep running.
func TestSendGate_DropsAndCountsWhenTheConsumerIsGone(t *testing.T) {
	g := newSendGate("test")
	g.timeout = 80 * time.Millisecond
	out := make(chan Envelope) // nothing will ever receive

	start := time.Now()
	if g.send(context.Background(), out, testEnv("system.cpu.time")) {
		t.Fatal("send reported success with no receiver")
	}
	elapsed := time.Since(start)

	if elapsed < g.timeout {
		t.Errorf("gave up after %s, before the %s timeout", elapsed, g.timeout)
	}
	if elapsed > time.Second {
		t.Errorf("took %s to give up — the bound is not being applied", elapsed)
	}
	if g.Dropped() != 1 {
		t.Errorf("dropped = %d, want 1", g.Dropped())
	}

	// And it must stay usable: a wedged pipeline must not disable the collector.
	if g.send(context.Background(), out, testEnv("m")) {
		t.Error("second send unexpectedly succeeded")
	}
	if g.Dropped() != 2 {
		t.Errorf("dropped = %d after two failures, want 2", g.Dropped())
	}
}

// Shutdown is not congestion. Counting these would make every clean stop look
// like data loss in the logs.
func TestSendGate_CancelledContextIsNotCountedAsADrop(t *testing.T) {
	g := newSendGate("test")
	g.timeout = 5 * time.Second
	out := make(chan Envelope)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if g.send(ctx, out, testEnv("m")) {
		t.Fatal("send reported success after cancellation")
	}
	if el := time.Since(start); el > time.Second {
		t.Errorf("cancellation took %s to take effect", el)
	}
	if g.Dropped() != 0 {
		t.Errorf("dropped = %d after a cancelled send, want 0", g.Dropped())
	}
}

// The OTLP receiver sends from its HTTP handler goroutines, so several sends
// can be in flight for one collector. Run with -race.
func TestSendGate_ConcurrentSendersCountCorrectly(t *testing.T) {
	g := newSendGate("test")
	g.timeout = 60 * time.Millisecond
	out := make(chan Envelope) // no receiver: every send drops

	const senders = 12
	var wg sync.WaitGroup
	wg.Add(senders)
	for i := 0; i < senders; i++ {
		go func() {
			defer wg.Done()
			g.send(context.Background(), out, testEnv("m"))
		}()
	}
	wg.Wait()

	if got := g.Dropped(); got != senders {
		t.Errorf("dropped = %d, want %d — the counter lost increments under concurrency", got, senders)
	}
}
