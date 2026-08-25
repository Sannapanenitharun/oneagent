package exporter

import (
	"fmt"
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
//
// A queue in memory can only ever be a shock absorber, though: it is bounded,
// so a long enough outage still sheds data, and it is volatile, so a restart
// loses whatever it held. When a spool is configured, overflow goes to disk
// instead of being shed and the shutdown drain writes the remainder there
// rather than reporting a loss — see spool.go.
type asyncExporter struct {
	inner        Exporter
	queue        chan collector.Envelope
	spool        *spool
	spoolErrs    atomic.Uint64
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

func newAsyncExporter(inner Exporter, queueSize int, drainTimeout time.Duration, retire func(collector.Envelope), sp *spool) *asyncExporter {
	if queueSize <= 0 {
		queueSize = defaultQueueSize
	}
	if drainTimeout <= 0 {
		drainTimeout = defaultDrainTimeout
	}
	a := &asyncExporter{
		inner:        inner,
		queue:        make(chan collector.Envelope, queueSize),
		spool:        sp,
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
	// Once anything is on disk, everything goes to disk until the spool is
	// empty again. Splitting the stream would deliver fresh envelopes ahead of
	// the backlog for as long as the outage lasted, and then interleave the
	// backlog behind them — out-of-order delivery for the entire recovery,
	// rather than just a delay.
	if a.spool != nil && !a.spool.empty() && a.trySpool(e) {
		return nil
	}

	select {
	case a.queue <- e:
		return nil
	default:
	}

	// Queue is full. Disk is the next stop, and only if that fails do we fall
	// back to shedding.
	//
	// Whatever is already queued moves across first. Those envelopes are older
	// than this one, and the sender drains disk before memory on the
	// assumption that disk always holds the older data — leaving them behind
	// would make that assumption false and deliver the two streams inverted
	// for the whole recovery. This runs once per outage: afterwards Export
	// bypasses the queue entirely until the spool is empty again.
	if a.spool != nil {
		a.migrateQueue()
		if a.trySpool(e) {
			return nil
		}
	}

	// Make room by discarding the oldest envelope.
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

// trySpool writes to disk, reporting whether it succeeded. A spool that cannot
// be written to — a full disk, a read-only mount — must not stop the agent
// collecting, so a failure here degrades to the in-memory shed policy rather
// than propagating. The failures are counted so the degradation is visible
// instead of silent.
func (a *asyncExporter) trySpool(e collector.Envelope) bool {
	if a.spool == nil {
		return false
	}
	if err := a.spool.Append(e); err != nil {
		if a.spoolErrs.Add(1) == 1 {
			log.Printf("exporter: spool write failed, falling back to dropping oldest: %v", err)
		}
		return false
	}
	return true
}

// migrateQueue empties the in-memory queue onto disk so the spool holds one
// unbroken run of oldest-first data.
func (a *asyncExporter) migrateQueue() {
	for {
		select {
		case old := <-a.queue:
			if a.trySpool(old) {
				continue
			}
			// Disk refused it. Put it back rather than dropping it — the
			// caller is about to fall through to the shed-oldest path, which
			// will make its own decision about what to give up.
			select {
			case a.queue <- old:
			default:
				a.dropped.Add(1)
				a.reportRetired(old)
			}
			return
		default:
			return
		}
	}
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

		// Shutdown and reporting are checked before the backlog, because
		// draining a full spool can take far longer than the shutdown budget
		// and the loop below never blocks — without this, a restart carrying
		// 128 MiB of backlog would hang until the whole thing had been shipped.
		// The spool exists precisely so that it does NOT have to be drained
		// before exit.
		select {
		case <-a.stopCh:
			a.drain()
			return
		case <-report.C:
			a.report()
		default:
		}

		// Disk before memory. Envelopes only reach the spool once the queue
		// has already overflowed, so everything on disk is older than
		// everything in the queue; draining memory first would deliver the
		// stream out of order for the whole length of the recovery.
		if a.deliverSpooled() {
			continue
		}

		select {
		case e := <-a.queue:
			if a.deliver(e) != nil {
				// The envelope that was in flight when the endpoint went down
				// is the one an outage would otherwise cost us: it has already
				// left the queue, so nothing else is holding it. Write it down
				// instead of dropping it on the floor.
				a.trySpool(e)
			}
		case <-report.C:
			a.report()
		case <-a.stopCh:
			a.drain()
			return
		}
	}
}

// deliverSpooled sends at most one spooled envelope, reporting whether it had
// anything to send. One at a time, because the caller re-checks the breaker
// between envelopes: a backlog that starts failing again should pause after
// the first failure, not grind through the whole spool re-learning it.
func (a *asyncExporter) deliverSpooled() bool {
	if a.spool == nil {
		return false
	}
	e, ok := a.spool.Peek()
	if !ok {
		return false
	}
	if a.deliver(e) == nil {
		// Only a delivered envelope advances the position. A failure leaves it
		// exactly where it was, which is the point of having written it down.
		a.spool.Ack()
	}
	return true
}

func (a *asyncExporter) deliver(e collector.Envelope) error {
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
		return err
	}

	a.breaker.success(now)
	if was && !a.breaker.tripped() {
		log.Printf("exporter: endpoint recovered, resuming normal delivery")
	}
	a.reportRetired(e)
	return nil
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
	if a.spool != nil {
		if b := a.spool.bytes(); b > 0 {
			log.Printf("exporter: %d bytes spooled to disk awaiting delivery", b)
		}
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
			a.spill("endpoint is down at shutdown")
			return
		}
		select {
		case e := <-a.queue:
			if a.deliver(e) != nil {
				a.trySpool(e)
			}
		case <-deadline:
			a.spill(fmt.Sprintf("shutdown drain timed out after %s", a.drainTimeout))
			return
		default:
			a.spill("") // queue empty; still flush the spool
			return
		}
	}
}

// spill writes whatever is still in memory to disk so the next run picks it
// up. Without a spool this can only report the loss, which is what it did
// before: a restart during an outage discarded the queue outright.
//
// The spool itself is deliberately NOT drained to the network here. It may
// hold far more than the shutdown budget could ship, and an agent that
// lingered trying would turn a systemd restart into the outage the bounded
// drain exists to prevent. Persisting it and exiting promptly is the point.
func (a *asyncExporter) spill(reason string) {
	if a.spool == nil {
		if n := len(a.queue); n > 0 && reason != "" {
			log.Printf("exporter: %s, %d envelopes not sent", reason, n)
		}
		return
	}

	spilled, lost := 0, 0
	for {
		select {
		case e := <-a.queue:
			if a.trySpool(e) {
				spilled++
				continue
			}
			lost++
			a.dropped.Add(1)
			a.reportRetired(e)
			continue
		default:
		}
		break
	}

	a.spool.Sync()
	a.spool.Commit()
	if spilled > 0 {
		if reason == "" {
			reason = "shutting down"
		}
		log.Printf("exporter: %s, %d envelopes written to the spool and will be sent after restart", reason, spilled)
	}
	if lost > 0 {
		log.Printf("exporter: %d envelopes could not be spooled at shutdown and were dropped", lost)
	}
}

func (a *asyncExporter) Close() error {
	a.stopOnce.Do(func() { close(a.stopCh) })
	a.wg.Wait()
	a.report()
	if a.spool != nil {
		// After wg.Wait, so the sender goroutine is finished with it.
		if err := a.spool.Close(); err != nil {
			log.Printf("exporter: closing spool: %v", err)
		}
	}
	return a.inner.Close()
}
