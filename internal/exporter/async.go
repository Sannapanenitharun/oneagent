package exporter

import (
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agent-i/agent/internal/collector"
)

// asyncExporter decouples collection from delivery.
//
// Before this existed, the daemon's drain loop called Export directly, which
// meant a flush went out on the collector's own goroutine. A degraded backend
// therefore stalled the entire agent: with the default 10s client timeout and
// 3 retries plus backoff, a single flush could block for ~41 seconds, the
// 256-slot envelope channel filled, and every collector blocked on its send —
// host metrics stopped sampling, tailers stopped reading. Backend degradation
// became agent-wide data loss, which is exactly backwards: the exporter is the
// part that is allowed to fail.
//
// Now Export only enqueues, and one sender goroutine owns delivery. When the
// queue is full the OLDEST envelope is dropped rather than the newest, because
// for telemetry the freshest data is the data worth keeping, and drops are
// counted and logged rather than being silent.
type asyncExporter struct {
	inner        Exporter
	queue        chan collector.Envelope
	stopCh       chan struct{}
	stopOnce     sync.Once
	wg           sync.WaitGroup
	dropped      atomic.Uint64
	failed       atomic.Uint64
	drainTimeout time.Duration
}

const (
	defaultQueueSize    = 4096
	defaultDrainTimeout = 5 * time.Second
	dropReportInterval  = 30 * time.Second
)

func newAsyncExporter(inner Exporter, queueSize int, drainTimeout time.Duration) *asyncExporter {
	if queueSize <= 0 {
		queueSize = defaultQueueSize
	}
	if drainTimeout <= 0 {
		drainTimeout = defaultDrainTimeout
	}
	a := &asyncExporter{
		inner:        inner,
		queue:        make(chan collector.Envelope, queueSize),
		stopCh:       make(chan struct{}),
		drainTimeout: drainTimeout,
	}
	a.wg.Add(1)
	go a.run()
	return a
}

// Export enqueues an envelope. It never blocks and never returns a delivery
// error, because by design the caller is no longer the one delivering; send
// failures are reported by the sender goroutine instead.
func (a *asyncExporter) Export(e collector.Envelope) error {
	select {
	case a.queue <- e:
		return nil
	default:
	}

	// Queue is full: make room by discarding the oldest envelope.
	select {
	case <-a.queue:
		a.dropped.Add(1)
	default:
	}
	select {
	case a.queue <- e:
	default:
		// Lost the race with the sender refilling the queue; drop this one.
		a.dropped.Add(1)
	}
	return nil
}

func (a *asyncExporter) run() {
	defer a.wg.Done()

	report := time.NewTicker(dropReportInterval)
	defer report.Stop()

	for {
		select {
		case e := <-a.queue:
			a.deliver(e)
		case <-report.C:
			a.report()
		case <-a.stopCh:
			a.drain()
			return
		}
	}
}

func (a *asyncExporter) deliver(e collector.Envelope) {
	if err := a.inner.Export(e); err != nil {
		a.failed.Add(1)
		log.Printf("export error (source=%s): %v", e.Source, err)
	}
}

// report surfaces queue pressure. Silence here is meaningful: if these lines
// never appear, the exporter is keeping up.
func (a *asyncExporter) report() {
	if n := a.dropped.Swap(0); n > 0 {
		log.Printf("exporter: dropped %d envelopes in the last %s — backend is not keeping up (queue=%d, depth=%d)",
			n, dropReportInterval, cap(a.queue), len(a.queue))
	}
	if n := a.failed.Swap(0); n > 0 {
		log.Printf("exporter: %d failed sends in the last %s", n, dropReportInterval)
	}
}

// drain makes a bounded effort to deliver what is still queued at shutdown.
// Bounded is the operative word: an unbounded drain against an unreachable
// backend is what turns a service restart into a 90-second outage.
func (a *asyncExporter) drain() {
	deadline := time.After(a.drainTimeout)
	for {
		select {
		case e := <-a.queue:
			a.deliver(e)
		case <-deadline:
			if n := len(a.queue); n > 0 {
				log.Printf("exporter: shutdown drain timed out after %s, %d envelopes not sent", a.drainTimeout, n)
			}
			return
		default:
			return // queue empty
		}
	}
}

func (a *asyncExporter) Close() error {
	a.stopOnce.Do(func() { close(a.stopCh) })
	a.wg.Wait()
	a.report()
	return a.inner.Close()
}
