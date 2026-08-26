package ingest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeStore records what was written without a database, so the batching and
// shedding policy can be tested for what it decides rather than for what
// ClickHouse does with the result.
type fakeStore struct {
	mu     sync.Mutex
	writes map[string]int // table -> rows written
	calls  int
	fail   bool
	block  chan struct{}
}

func newFakeStore() *fakeStore {
	return &fakeStore{writes: map[string]int{}}
}

func (f *fakeStore) Insert(ctx context.Context, table string, rows []map[string]any) error {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail {
		return errors.New("insert failed")
	}
	f.writes[table] += len(rows)
	return nil
}

func (f *fakeStore) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, v := range f.writes {
		n += v
	}
	return n
}

func (f *fakeStore) rows(table string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes[table]
}

func (f *fakeStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func rows(n int) []Row {
	out := make([]Row, n)
	for i := range out {
		out[i] = Row{"i": i}
	}
	return out
}

// The whole reason the writer exists: ClickHouse wants few large inserts, and
// a round trip per export would be one part per export for the merge scheduler
// to work through.
func TestWriter_BatchesRatherThanWritingPerCall(t *testing.T) {
	fs := newFakeStore()
	w := NewWriter(fs, 1000, time.Hour) // never flushes on the timer
	defer w.Close(context.Background())

	for i := 0; i < 50; i++ {
		w.Add(Batch{Metrics: rows(1)})
	}
	if got := fs.callCount(); got != 0 {
		t.Fatalf("%d inserts after 50 small adds — the buffer is not batching", got)
	}

	w.Flush(context.Background())
	if got := fs.rows("metrics"); got != 50 {
		t.Errorf("wrote %d rows, want 50", got)
	}
	// One insert for the one table, not one per Add.
	if got := fs.callCount(); got != 1 {
		t.Errorf("%d insert calls for one flush of one table, want 1", got)
	}
}

func TestWriter_FlushesWhenFull(t *testing.T) {
	fs := newFakeStore()
	w := NewWriter(fs, 100, time.Hour)
	defer w.Close(context.Background())

	w.Add(Batch{Metrics: rows(100)})

	// The threshold flush happens on the caller's goroutine, so it has already
	// completed by the time Add returns — no polling needed, and that
	// synchronousness is the backpressure.
	if got := fs.rows("metrics"); got != 100 {
		t.Fatalf("wrote %d rows, want 100 flushed at the threshold", got)
	}
}

// Without a time-triggered flush, a low-volume deployment's data would sit
// invisible until enough of it accumulated to hit the row threshold.
func TestWriter_FlushesOnTheTimer(t *testing.T) {
	fs := newFakeStore()
	w := NewWriter(fs, 1_000_000, 50*time.Millisecond)
	defer w.Close(context.Background())

	w.Add(Batch{Metrics: rows(3)})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fs.rows("metrics") == 3 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timer never flushed: wrote %d rows, want 3", fs.rows("metrics"))
}

func TestWriter_SeparatesTables(t *testing.T) {
	fs := newFakeStore()
	w := NewWriter(fs, 1000, time.Hour)
	defer w.Close(context.Background())

	w.Add(Batch{Metrics: rows(3), Logs: rows(5), Spans: rows(7), Hosts: rows(1)})
	w.Flush(context.Background())

	for table, want := range map[string]int{"metrics": 3, "logs": 5, "spans": 7, "hosts": 1} {
		if got := fs.rows(table); got != want {
			t.Errorf("%s: wrote %d rows, want %d", table, got, want)
		}
	}
}

// A database that is down must not be absorbed indefinitely. The cap is what
// stops a slow store turning into an out-of-memory kill, and shedding is
// counted rather than silent.
//
// Concurrent producers, because that is both the real shape — one goroutine
// per in-flight request — and the only shape where this can happen. With a
// single producer, the threshold flush runs on that same goroutine, so it
// blocks inside the hung insert and never adds anything more; the buffer can
// only outgrow its cap while other goroutines keep filling it.
func TestWriter_ShedsOverTheCapAndCountsIt(t *testing.T) {
	fs := newFakeStore()
	fs.block = make(chan struct{}) // every insert hangs
	w := NewWriter(fs, 100, time.Hour)

	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.Add(Batch{Metrics: rows(50)})
		}()
	}

	deadline := time.Now().Add(5 * time.Second)
	shed := false
	for time.Now().Before(deadline) {
		if dropped, _ := w.Stats(); dropped > 0 {
			shed = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	close(fs.block) // release the hung inserts so the goroutines can finish
	wg.Wait()
	w.Close(context.Background())

	if !shed {
		t.Fatal("nothing was shed despite the store never completing a write")
	}
}

// A failed insert is counted, and does not requeue: a schema or type problem
// fails identically forever and would block every table behind it.
func TestWriter_CountsFailedWrites(t *testing.T) {
	fs := newFakeStore()
	fs.fail = true
	w := NewWriter(fs, 1000, time.Hour)
	defer w.Close(context.Background())

	w.Add(Batch{Metrics: rows(10)})
	w.Flush(context.Background())

	if _, failed := w.Stats(); failed != 10 {
		t.Fatalf("failed count = %d, want 10", failed)
	}
	// Not requeued — a second flush must not retry the same rows forever.
	before := fs.callCount()
	w.Flush(context.Background())
	if fs.callCount() != before {
		t.Error("a failed batch was retried; it should have been dropped and counted")
	}
}

// Shutdown must not discard what is buffered.
func TestWriter_CloseFlushes(t *testing.T) {
	fs := newFakeStore()
	w := NewWriter(fs, 1_000_000, time.Hour)

	w.Add(Batch{Metrics: rows(11)})
	w.Close(context.Background())

	if got := fs.rows("metrics"); got != 11 {
		t.Fatalf("wrote %d rows on close, want 11", got)
	}
}

func TestWriter_AddAfterCloseIsIgnored(t *testing.T) {
	fs := newFakeStore()
	w := NewWriter(fs, 1000, time.Hour)
	w.Close(context.Background())

	w.Add(Batch{Metrics: rows(5)})
	w.Flush(context.Background())

	if got := fs.rows("metrics"); got != 0 {
		t.Fatalf("wrote %d rows after Close", got)
	}
}

func TestWriter_CloseIsIdempotent(t *testing.T) {
	w := NewWriter(newFakeStore(), 1000, time.Hour)
	w.Close(context.Background())
	w.Close(context.Background()) // must not panic on a second close
}

func TestWriter_EmptyBatchIsANoOp(t *testing.T) {
	fs := newFakeStore()
	w := NewWriter(fs, 1000, time.Hour)
	defer w.Close(context.Background())

	w.Add(Batch{})
	w.Flush(context.Background())

	if fs.callCount() != 0 {
		t.Fatal("an empty batch produced an insert")
	}
	if fs.total() != 0 {
		t.Fatal("an empty batch produced rows")
	}
}

// Concurrent producers are the normal case: one goroutine per in-flight
// request.
func TestWriter_ConcurrentAddsAreSafe(t *testing.T) {
	fs := newFakeStore()
	w := NewWriter(fs, 1_000_000, time.Hour)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				w.Add(Batch{Metrics: rows(1)})
			}
		}()
	}
	wg.Wait()
	w.Close(context.Background())

	if got := fs.rows("metrics"); got != 800 {
		t.Fatalf("wrote %d rows, want 800 — rows were lost under concurrency", got)
	}
}
