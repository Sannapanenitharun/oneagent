package exporter

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/agent-i/agent/internal/collector"
)

// retireRecorder collects the envelopes an exporter reports as settled.
type retireRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *retireRecorder) fn() func(collector.Envelope) {
	return func(e collector.Envelope) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.seen = append(r.seen, e.Source)
	}
}

func (r *retireRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen)
}

func (r *retireRecorder) sources() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

type okExporter struct {
	mu sync.Mutex
	n  int
}

func (o *okExporter) Export(collector.Envelope) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.n++
	return nil
}
func (o *okExporter) Close() error { return nil }

type alwaysFailExporter struct{}

func (alwaysFailExporter) Export(collector.Envelope) error { return errors.New("nope") }
func (alwaysFailExporter) Close() error                    { return nil }

func TestAsyncExporter_RetiresOnSuccessfulDelivery(t *testing.T) {
	rec := &retireRecorder{}
	a := newAsyncExporter(&okExporter{}, 16, time.Second, rec.fn())
	defer a.Close()

	for i := 0; i < 5; i++ {
		if err := a.Export(collector.Envelope{Source: "line"}); err != nil {
			t.Fatalf("Export: %v", err)
		}
	}
	waitFor(t, 2*time.Second, func() bool { return rec.count() == 5 })
}

// The property that makes restart-safety work: a send that failed must NOT be
// reported as settled, so the line behind it is read again next time.
func TestAsyncExporter_DoesNotRetireFailedSends(t *testing.T) {
	rec := &retireRecorder{}
	a := newAsyncExporter(alwaysFailExporter{}, 16, 50*time.Millisecond, rec.fn())
	defer a.Close()

	if err := a.Export(collector.Envelope{Source: "doomed"}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	if got := rec.count(); got != 0 {
		t.Errorf("retired %d envelopes that were never delivered (%v) — those lines would be lost on restart",
			got, rec.sources())
	}
}

// A deliberately shed envelope IS settled, unhappily. If it were not, a backend
// outage would freeze the file offset and the agent would re-read the same
// bytes after every restart without ever making progress.
func TestAsyncExporter_RetiresShedEnvelopes(t *testing.T) {
	rec := &retireRecorder{}
	// A blocking inner exporter keeps the sender busy so the queue really fills.
	block := make(chan struct{})
	a := newAsyncExporter(blockingExporter{release: block}, 2, 50*time.Millisecond, rec.fn())

	for i := 0; i < 20; i++ {
		if err := a.Export(collector.Envelope{Source: "flood"}); err != nil {
			t.Fatalf("Export: %v", err)
		}
	}

	dropped := a.dropped.Load()
	retired := rec.count()

	// Released before Close, not in a defer: Close drains, drain delivers, and
	// delivery would block forever on an exporter still waiting on this channel.
	close(block)
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if dropped == 0 {
		t.Fatal("expected the queue to shed envelopes under this load")
	}
	if retired == 0 {
		t.Error("shed envelopes were not reported as settled — the file offset would never advance during an outage")
	}
}

type blockingExporter struct{ release chan struct{} }

func (b blockingExporter) Export(collector.Envelope) error {
	<-b.release
	return nil
}
func (b blockingExporter) Close() error { return nil }

// The synchronous sinks report through a thin wrapper instead, because their
// Export return value genuinely means "delivered".
func TestRetiringExporter_ReportsOnlyOnSuccess(t *testing.T) {
	rec := &retireRecorder{}
	ok := withRetire(&okExporter{}, rec.fn())
	if err := ok.Export(collector.Envelope{Source: "good"}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if rec.count() != 1 {
		t.Errorf("successful sync export retired %d, want 1", rec.count())
	}

	rec2 := &retireRecorder{}
	bad := withRetire(alwaysFailExporter{}, rec2.fn())
	if err := bad.Export(collector.Envelope{Source: "bad"}); err == nil {
		t.Fatal("expected an error from the failing exporter")
	}
	if rec2.count() != 0 {
		t.Errorf("failed sync export retired %d, want 0", rec2.count())
	}
}

func TestWithRetire_IsAPassThroughWhenUnset(t *testing.T) {
	inner := &okExporter{}
	if got := withRetire(inner, nil); got != Exporter(inner) {
		t.Error("with no callback the exporter should be returned unwrapped, not hidden behind a layer that does nothing")
	}
}
