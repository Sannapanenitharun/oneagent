package ingest

import (
	"context"
	"log"
	"sync"
	"time"
)

// Writer buffers rows and flushes them to the store in batches.
//
// ClickHouse is explicit that it wants few large inserts rather than many
// small ones: every insert creates a part, and parts are merged in the
// background, so a thousand single-row inserts a second is a thousand parts a
// second for the merge scheduler to work through. It will accept them and then
// spend the rest of the day catching up. Batching is not an optimisation here,
// it is the documented way to use the database.
//
// The buffer is bounded and sheds oldest-first, the same policy and for the
// same reason as the agent's export queue: under sustained overload the
// freshest telemetry is the useful telemetry, and an unbounded buffer turns a
// slow database into an out-of-memory kill.
type Writer struct {
	store    Store
	maxRows  int
	interval time.Duration

	mu     sync.Mutex
	buf    map[string][]Row
	nRows  int
	closed bool

	flushMu sync.Mutex

	stopCh chan struct{}
	wg     sync.WaitGroup

	mu2     sync.Mutex
	dropped int
	failed  int
}

// Store is what the writer needs from the database. An interface so the
// writer's batching and shedding can be tested without one.
type Store interface {
	Insert(ctx context.Context, table string, rows []map[string]any) error
}

const (
	defaultMaxRows  = 10000
	defaultInterval = 2 * time.Second
	// reportInterval bounds how often shed and failure counts are logged.
	// Silence here is meaningful: if these lines never appear, ingest is
	// keeping up.
	reportInterval = 30 * time.Second
)

func NewWriter(store Store, maxRows int, interval time.Duration) *Writer {
	if maxRows <= 0 {
		maxRows = defaultMaxRows
	}
	if interval <= 0 {
		interval = defaultInterval
	}
	w := &Writer{
		store:    store,
		maxRows:  maxRows,
		interval: interval,
		buf:      map[string][]Row{},
		stopCh:   make(chan struct{}),
	}
	w.wg.Add(1)
	go w.run()
	return w
}

// Add buffers a batch. It never blocks on the database: a request that had to
// wait for ClickHouse would make every agent's export latency a function of
// the backend's merge queue.
func (w *Writer) Add(b Batch) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.append("metrics", b.Metrics)
	w.append("logs", b.Logs)
	w.append("spans", b.Spans)
	w.append("hosts", b.Hosts)
	full := w.nRows >= w.maxRows
	w.mu.Unlock()

	if full {
		// On the caller's goroutine deliberately. It is the one producing the
		// rows, so making it pay for the flush is what stops a fast producer
		// outrunning the writer indefinitely — the backpressure has to land
		// somewhere, and here it lands on whoever caused it.
		w.Flush(context.Background())
	}
}

// append must be called with mu held.
func (w *Writer) append(table string, rows []Row) {
	if len(rows) == 0 {
		return
	}
	w.buf[table] = append(w.buf[table], rows...)
	w.nRows += len(rows)

	// Hard cap, so a database that is down cannot be absorbed indefinitely.
	// Twice the flush threshold: below that the buffer is doing its job, above
	// it the flush is not keeping up and something has to give.
	limit := w.maxRows * 2
	if w.nRows <= limit {
		return
	}
	excess := w.nRows - limit
	for _, t := range []string{"metrics", "logs", "spans", "hosts"} {
		if excess <= 0 {
			break
		}
		have := len(w.buf[t])
		if have == 0 {
			continue
		}
		drop := excess
		if drop > have {
			drop = have
		}
		w.buf[t] = w.buf[t][drop:]
		w.nRows -= drop
		excess -= drop
		w.countDropped(drop)
	}
}

// Flush writes everything buffered.
//
// The flush lock is separate from the buffer lock so a slow insert does not
// block Add for its duration — the buffer is swapped out under the short lock,
// and only the writing is serialised.
func (w *Writer) Flush(ctx context.Context) {
	w.flushMu.Lock()
	defer w.flushMu.Unlock()

	w.mu.Lock()
	if w.nRows == 0 {
		w.mu.Unlock()
		return
	}
	pending := w.buf
	w.buf = map[string][]Row{}
	w.nRows = 0
	w.mu.Unlock()

	for table, rows := range pending {
		if len(rows) == 0 {
			continue
		}
		out := make([]map[string]any, len(rows))
		for i, r := range rows {
			out[i] = r
		}
		if err := w.store.Insert(ctx, table, out); err != nil {
			// Not requeued. A failed insert is usually a schema or type
			// problem, which will fail identically forever and would block
			// every table behind it; and when it is the database being down,
			// the rows would pile up against the same cap that is already
			// shedding. Counted and logged rather than silently lost.
			w.countFailed(len(rows))
			log.Printf("ingest: writing %d rows to %s: %v", len(rows), table, err)
		}
	}
}

func (w *Writer) run() {
	defer w.wg.Done()
	flush := time.NewTicker(w.interval)
	defer flush.Stop()
	report := time.NewTicker(reportInterval)
	defer report.Stop()

	for {
		select {
		case <-flush.C:
			// A time-triggered flush is what bounds how long a low-volume
			// deployment's data sits invisible. Without it a single agent
			// sending a handful of rows would never reach maxRows and its
			// telemetry would not appear until something else did.
			w.Flush(context.Background())
		case <-report.C:
			w.report()
		case <-w.stopCh:
			return
		}
	}
}

func (w *Writer) countDropped(n int) {
	w.mu2.Lock()
	w.dropped += n
	w.mu2.Unlock()
}

func (w *Writer) countFailed(n int) {
	w.mu2.Lock()
	w.failed += n
	w.mu2.Unlock()
}

func (w *Writer) report() {
	w.mu2.Lock()
	dropped, failed := w.dropped, w.failed
	w.dropped, w.failed = 0, 0
	w.mu2.Unlock()

	if dropped > 0 {
		log.Printf("ingest: dropped %d rows in the last %s — the database is not keeping up (buffer cap %d)",
			dropped, reportInterval, w.maxRows*2)
	}
	if failed > 0 {
		log.Printf("ingest: %d rows failed to write in the last %s", failed, reportInterval)
	}
}

// Stats reports what has been shed and what has failed since the last report.
// Exposed for tests and for a health endpoint.
func (w *Writer) Stats() (dropped, failed int) {
	w.mu2.Lock()
	defer w.mu2.Unlock()
	return w.dropped, w.failed
}

// Close flushes what is buffered and stops the background loop.
func (w *Writer) Close(ctx context.Context) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	w.mu.Unlock()

	close(w.stopCh)
	w.wg.Wait()
	w.Flush(ctx)
	w.report()
}
