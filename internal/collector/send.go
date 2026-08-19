package collector

import (
	"context"
	"log"
	"sync/atomic"
	"time"
)

// sendGate bounds how long a collector will wait to hand an envelope to the
// daemon.
//
// The shared channel is buffered, and a bare `out <- env` is fine right up
// until that buffer fills. Once it does, every collector blocks on its send —
// including the ones with nothing wrong with them. One stuck consumer then
// stops CPU, memory, disk, network, log and trace collection together, and
// nothing anywhere says why.
//
// This is not hypothetical. The stdout and file exporters are deliberately
// synchronous, so a host whose disk has filled makes every write slow, the
// buffer fills behind it, and the whole pipeline wedges. That exact disk-full
// case has already happened in production on this agent.
//
// The trade being made: past the timeout an envelope is dropped rather than
// waited on forever. That is a real loss, so it is counted and logged rather
// than swallowed — visible degradation, in the same spirit as the dashboard
// store refusing new series and reporting the count instead of silently
// evicting old ones. Blocking forever is not the safer option; it converts one
// slow writer into total collection failure.
//
// Safe for concurrent use: the OTLP receiver sends from its HTTP handler
// goroutines, so several sends can be in flight for one collector at once.
type sendGate struct {
	name        string
	timeout     time.Duration
	dropped     atomic.Uint64
	lastLogUnix atomic.Int64
}

// sendTimeout is generous on purpose. It must never fire on an ordinary burst —
// a batch of pushed spans, or a tailer catching up on a rotated file — because
// dropping there would lose data the pipeline could easily have absorbed. It
// exists for the case where the consumer has genuinely stopped moving.
const sendTimeout = 5 * time.Second

// dropReportEvery rate-limits the drop log the same way the poll-failure log
// once did: a wedged pipeline would otherwise write a line per envelope
// and bury everything else in the journal.
const dropReportEvery = 30 * time.Second

func newSendGate(name string) *sendGate {
	return &sendGate{name: name, timeout: sendTimeout}
}

// send hands one envelope to the daemon. It reports whether the envelope was
// accepted; callers that emit several envelopes per sample may ignore the
// result, since a full pipeline will fail the rest of the batch too and the
// drop is already counted.
func (g *sendGate) send(ctx context.Context, out chan<- Envelope, env Envelope) bool {
	// Fast path: there is room. No timer is allocated, so the cost of a send
	// while the pipeline is healthy is what it always was — which matters
	// because the log tailer can call this hundreds of times a second.
	select {
	case out <- env:
		return true
	default:
	}

	// Buffer is full. Only now is it worth paying for a timer.
	t := time.NewTimer(g.timeout)
	defer t.Stop()
	select {
	case out <- env:
		return true
	case <-ctx.Done():
		// Shutdown, not congestion. Not counted as a drop: the daemon's own
		// drain handles what is still buffered, and reporting these would
		// make every clean stop look like data loss.
		return false
	case <-t.C:
		g.recordDrop(env)
		return false
	}
}

func (g *sendGate) recordDrop(env Envelope) {
	n := g.dropped.Add(1)
	now := time.Now().Unix()
	last := g.lastLogUnix.Load()
	// The first drop logs immediately (last is zero), then at most one line
	// per window. CompareAndSwap so concurrent senders produce one line, not
	// one per goroutine.
	if now-last >= int64(dropReportEvery/time.Second) && g.lastLogUnix.CompareAndSwap(last, now) {
		log.Printf("%s: pipeline full, dropped %d envelope(s) after waiting %s (most recent source=%s) — the exporter or daemon loop is not keeping up",
			g.name, n, g.timeout, env.Source)
	}
}

// Dropped reports the lifetime drop count for this collector.
func (g *sendGate) Dropped() uint64 { return g.dropped.Load() }
