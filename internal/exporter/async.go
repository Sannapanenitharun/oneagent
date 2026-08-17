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

	// breaker is touched only by the sender goroutine (run, deliver, drain),
	// so it needs no lock. Export runs on the daemon goroutine and deliberately
	// does not consult it: enqueueing must stay non-blocking whatever the
	// endpoint is doing, and the queue's own shed-oldest policy is what decides
	// which envelopes survive an outage.
	breaker *breaker

	// retire reports that an envelope's fate is settled. See New.
	retire func(collector.Envelope)
}

const (
	defaultQueueSize    = 4096
	defaultDrainTimeout = 5 * time.Second
	dropReportInterval  = 30 * time.Second
)

func newAsyncExporter(inner Exporter, queueSize int, drainTimeout time.Duration, retire func(collector.Envelope)) *asyncExporter {
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
		breaker:      newBreaker(),
		retire:       retire,
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
	case old := <-a.queue:
		a.dropped.Add(1)
		// A shed envelope is settled, just unhappily. Reporting it keeps a
		// tailed file moving forward: if deliberate drops blocked the offset,
		// a backend outage would leave the agent re-reading the same bytes
		// after every restart and never making progress.
		a.reportRetired(old)
	default:
	}
	select {
	case a.queue <- e:
	default:
		// Lost the race with the sender refilling the queue; drop this one.
		a.dropped.Add(1)
		a.reportRetired(e)
	}
	return nil
}

func (a *asyncExporter) reportRetired(e collector.Envelope) {
	if a.retire != nil {
		a.retire(e)
	}
}

func (a *asyncExporter) run() {
	defer a.wg.Done()

	report := time.NewTicker(dropReportInterval)
	defer report.Stop()

	for {
		// Consulted BEFORE dequeuing, not after. Taking an envelope off the
		// queue and then discarding it because the endpoint is down would empty
		// the queue at full speed during an outage and leave nothing to send on
		// recovery; leaving it queued lets the normal shed-oldest policy keep
		// the freshest data instead.
		if ok, wait := a.breaker.allow(time.Now()); !ok {
			t := time.NewTimer(wait)
			select {
			case <-t.C:
			case <-report.C:
				a.report()
			case <-a.stopCh:
				t.Stop()
				a.drain()
				return
			}
			t.Stop()
			continue
		}

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
	was := a.breaker.tripped()
	now := time.Now()

	err := a.inner.Export(e)
	if err != nil {
		a.failed.Add(1)
		a.breaker.failure(now)
		if !was && a.breaker.tripped() {
			// Logged only on the transition. An endpoint that is down produces
			// one line here, not one per envelope — the per-envelope detail is
			// already on the line below and the periodic drop report.
			log.Printf("exporter: circuit breaker opened after %v — pausing delivery, queued envelopes are retained (newest first)", err)
		}
		log.Printf("export error (source=%s): %v", e.Source, err)
		return
	}

	a.breaker.success(now)
	if was && !a.breaker.tripped() {
		log.Printf("exporter: endpoint recovered, resuming normal delivery")
	}
	a.reportRetired(e)
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
		// An open breaker at shutdown means we already know the endpoint is not
		// answering. Spending the drain budget re-confirming that just delays
		// the process exit — and on a systemd stop, a slow exit is what turns a
		// restart into an outage.
		if ok, _ := a.breaker.allow(time.Now()); !ok {
			if n := len(a.queue); n > 0 {
				log.Printf("exporter: endpoint is down at shutdown, %d envelopes not sent", n)
			}
			return
		}
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
