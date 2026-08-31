package exporter

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-i/agent/internal/collector"
)

func testSpool(t *testing.T, opts spoolOptions) *spool {
	t.Helper()
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	if opts.SyncInterval == 0 {
		opts.SyncInterval = time.Hour // never sync on the timer; tests call Sync
	}
	s, err := openSpool(opts)
	if err != nil {
		t.Fatalf("openSpool: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func env(source string) collector.Envelope {
	return collector.Envelope{
		Kind:      collector.KindLog,
		AgentID:   "test-host",
		Source:    source,
		Timestamp: time.Unix(1700000000, 0).UTC(),
		Message:   "line for " + source,
	}
}

func drainSpool(t *testing.T, s *spool) []string {
	t.Helper()
	var got []string
	for {
		e, ok := s.Peek()
		if !ok {
			return got
		}
		got = append(got, e.Source)
		s.Ack()
	}
}

func TestSpool_RoundTripsInOrder(t *testing.T) {
	s := testSpool(t, spoolOptions{})

	want := make([]string, 0, 50)
	for i := 0; i < 50; i++ {
		src := fmt.Sprintf("evt-%02d", i)
		want = append(want, src)
		if err := s.Append(env(src)); err != nil {
			t.Fatalf("Append(%s): %v", src, err)
		}
	}
	if s.empty() {
		t.Fatal("spool reports empty after 50 appends")
	}

	got := drainSpool(t, s)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("out of order\n got %v\nwant %v", got, want)
	}
	if !s.empty() {
		t.Fatalf("spool not empty after draining everything: %d bytes", s.bytes())
	}
}

func TestSpool_PreservesEnvelopeFields(t *testing.T) {
	s := testSpool(t, spoolOptions{})
	in := collector.Envelope{
		Kind:      collector.KindTrace,
		AgentID:   "i-00aab1097c1a58ac5",
		Source:    "otlp.span",
		Timestamp: time.Unix(1700000000, 123456789).UTC(),
		Labels:    map[string]string{"service": "checkout", "kind": "client"},
		Value:     0, // must survive: a real 0 reading is not "no value"
		Payload:   map[string]any{"trace_id": "abc123"},
	}
	if err := s.Append(in); err != nil {
		t.Fatalf("Append: %v", err)
	}
	out, ok := s.Peek()
	if !ok {
		t.Fatal("Peek returned nothing")
	}
	if out.Kind != in.Kind || out.AgentID != in.AgentID || out.Source != in.Source {
		t.Fatalf("identity fields lost: %+v", out)
	}
	if !out.Timestamp.Equal(in.Timestamp) {
		t.Fatalf("timestamp: got %v want %v", out.Timestamp, in.Timestamp)
	}
	if out.Labels["service"] != "checkout" || out.Labels["kind"] != "client" {
		t.Fatalf("labels lost: %v", out.Labels)
	}
	if out.Payload["trace_id"] != "abc123" {
		t.Fatalf("payload lost: %v", out.Payload)
	}
}

// A failed delivery must re-offer the same envelope. This is the whole reason
// Peek and Ack are separate calls.
func TestSpool_PeekWithoutAckReOffers(t *testing.T) {
	s := testSpool(t, spoolOptions{})
	if err := s.Append(env("a")); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(env("b")); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		e, ok := s.Peek()
		if !ok || e.Source != "a" {
			t.Fatalf("peek %d: got %q ok=%v, want %q", i, e.Source, ok, "a")
		}
	}
	s.Ack()
	e, ok := s.Peek()
	if !ok || e.Source != "b" {
		t.Fatalf("after Ack: got %q ok=%v, want %q", e.Source, ok, "b")
	}
}

// The point of the whole exercise: what is on disk must outlive the process.
func TestSpool_SurvivesRestart(t *testing.T) {
	dir := t.TempDir()

	first := testSpool(t, spoolOptions{Dir: dir})
	for i := 0; i < 20; i++ {
		if err := first.Append(env(fmt.Sprintf("evt-%02d", i))); err != nil {
			t.Fatal(err)
		}
	}
	// Consume the first five, as a run that delivered some before dying.
	for i := 0; i < 5; i++ {
		if _, ok := first.Peek(); !ok {
			t.Fatalf("peek %d returned nothing", i)
		}
		first.Ack()
	}
	first.Commit()
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second := testSpool(t, spoolOptions{Dir: dir})
	got := drainSpool(t, second)
	if len(got) != 15 {
		t.Fatalf("after restart got %d envelopes, want 15: %v", len(got), got)
	}
	if got[0] != "evt-05" || got[14] != "evt-19" {
		t.Fatalf("wrong range after restart: first=%s last=%s", got[0], got[14])
	}
}

// Nothing acknowledged means nothing may be skipped on restart.
func TestSpool_RestartReplaysEverythingWhenNothingWasAcked(t *testing.T) {
	dir := t.TempDir()

	first := testSpool(t, spoolOptions{Dir: dir})
	for i := 0; i < 10; i++ {
		if err := first.Append(env(fmt.Sprintf("evt-%02d", i))); err != nil {
			t.Fatal(err)
		}
	}
	// Peeked but never acked — a delivery that was in flight when we died.
	if _, ok := first.Peek(); !ok {
		t.Fatal("peek returned nothing")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := testSpool(t, spoolOptions{Dir: dir})
	if got := drainSpool(t, second); len(got) != 10 || got[0] != "evt-00" {
		t.Fatalf("got %d envelopes starting at %v, want 10 starting at evt-00", len(got), got)
	}
}

func TestSpool_RotatesAndReclaimsSegments(t *testing.T) {
	dir := t.TempDir()
	s := testSpool(t, spoolOptions{
		Dir:          dir,
		MaxBytes:     4 << 20,
		SegmentBytes: minSpoolSegmentBytes, // clamped floor, ~64 KiB
	})

	// Enough to span several segments.
	const n = 2000
	for i := 0; i < n; i++ {
		if err := s.Append(env(fmt.Sprintf("evt-%04d", i))); err != nil {
			t.Fatal(err)
		}
	}
	segs, err := s.segments()
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) < 2 {
		t.Fatalf("expected rotation across multiple segments, got %d", len(segs))
	}

	if got := drainSpool(t, s); len(got) != n {
		t.Fatalf("drained %d envelopes, want %d", len(got), n)
	}
	// Consumed segments must actually be deleted, or the spool grows without
	// bound however much of it has been delivered.
	after, err := s.segments()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Fatalf("after draining, %d segments remain, want only the active one: %v", len(after), after)
	}
}

// Over the cap, the oldest data goes — and it must be reported as settled, or
// a tailed file's offset would stick behind it forever.
func TestSpool_ShedsOldestOverCapAndRetires(t *testing.T) {
	var mu sync.Mutex
	var retired []string

	s := testSpool(t, spoolOptions{
		MaxBytes:     256 << 10,
		SegmentBytes: minSpoolSegmentBytes,
		Retire: func(e collector.Envelope) {
			mu.Lock()
			defer mu.Unlock()
			retired = append(retired, e.Source)
		},
	})

	const n = 8000
	for i := 0; i < n; i++ {
		if err := s.Append(env(fmt.Sprintf("evt-%04d", i))); err != nil {
			t.Fatal(err)
		}
	}

	if s.total > s.opts.MaxBytes {
		t.Fatalf("spool is %d bytes, over its %d cap", s.total, s.opts.MaxBytes)
	}

	mu.Lock()
	nRetired := len(retired)
	firstRetired := ""
	if nRetired > 0 {
		firstRetired = retired[0]
	}
	mu.Unlock()
	if nRetired == 0 {
		t.Fatal("shed envelopes were never reported as settled")
	}
	if firstRetired != "evt-0000" {
		t.Fatalf("shedding did not start from the oldest: first retired %q", firstRetired)
	}

	// What survives must be the newest, contiguous, and in order.
	got := drainSpool(t, s)
	if len(got) == 0 {
		t.Fatal("everything was shed")
	}
	if got[len(got)-1] != fmt.Sprintf("evt-%04d", n-1) {
		t.Fatalf("newest envelope did not survive: last is %q", got[len(got)-1])
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("out of order at %d: %q then %q", i, got[i-1], got[i])
		}
	}
	if nRetired+len(got) != n {
		t.Fatalf("%d retired + %d surviving = %d, want %d — envelopes vanished unaccounted for",
			nRetired, len(got), nRetired+len(got), n)
	}
}

// A crash mid-write leaves a torn record at the tail of the last segment. The
// reader must stop there and move on rather than stalling on it.
func TestSpool_TornTailIsDroppedNotFatal(t *testing.T) {
	dir := t.TempDir()

	first := testSpool(t, spoolOptions{Dir: dir})
	for i := 0; i < 5; i++ {
		if err := first.Append(env(fmt.Sprintf("evt-%02d", i))); err != nil {
			t.Fatal(err)
		}
	}
	first.Sync()
	name := first.wName
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// Chop the last few bytes, simulating a power loss mid-append.
	path := filepath.Join(dir, name)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, fi.Size()-6); err != nil {
		t.Fatal(err)
	}

	second := testSpool(t, spoolOptions{Dir: dir})
	got := drainSpool(t, second)
	if len(got) != 4 {
		t.Fatalf("got %d envelopes, want the 4 intact ones: %v", len(got), got)
	}
	if got[0] != "evt-00" || got[3] != "evt-03" {
		t.Fatalf("wrong survivors: %v", got)
	}
	if !second.empty() {
		t.Fatalf("reader stalled on the torn tail: %d bytes still reported unread", second.bytes())
	}
}

// Bit rot inside a record must not be delivered as if it were real telemetry.
func TestSpool_CorruptRecordStopsAtTheDamage(t *testing.T) {
	dir := t.TempDir()

	first := testSpool(t, spoolOptions{Dir: dir})
	for i := 0; i < 5; i++ {
		if err := first.Append(env(fmt.Sprintf("evt-%02d", i))); err != nil {
			t.Fatal(err)
		}
	}
	first.Sync()
	name := first.wName
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte inside the first record's payload, past its header.
	raw[spoolHeaderBytes+4] ^= 0xFF
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	second := testSpool(t, spoolOptions{Dir: dir})
	got := drainSpool(t, second)
	for _, src := range got {
		if src == "evt-00" {
			t.Fatal("a record failing its checksum was delivered anyway")
		}
	}
	if !second.empty() {
		t.Fatalf("reader stalled on the corrupt record: %d bytes still unread", second.bytes())
	}
}

func TestSpool_ClampsSegmentToHalfTheCap(t *testing.T) {
	// A segment as large as the cap could never be shed, so the spool would
	// grow past its own limit forever.
	s := testSpool(t, spoolOptions{MaxBytes: 1 << 20, SegmentBytes: 4 << 20})
	if s.opts.SegmentBytes > s.opts.MaxBytes/2 {
		t.Fatalf("segment %d is more than half of cap %d", s.opts.SegmentBytes, s.opts.MaxBytes)
	}
}

func TestSpool_AppendAfterCloseFails(t *testing.T) {
	s := testSpool(t, spoolOptions{})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(env("a")); err == nil {
		t.Fatal("Append succeeded on a closed spool")
	}
}

func TestSpool_EmptyDirStartsClean(t *testing.T) {
	s := testSpool(t, spoolOptions{})
	if !s.empty() {
		t.Fatalf("fresh spool reports %d unread bytes", s.bytes())
	}
	if _, ok := s.Peek(); ok {
		t.Fatal("fresh spool returned an envelope")
	}
}

// A position file naming a segment that was later shed must not send the
// reader looking for a file that is gone.
func TestSpool_StalePositionFallsBackToOldestSegment(t *testing.T) {
	dir := t.TempDir()
	first := testSpool(t, spoolOptions{Dir: dir})
	if err := first.Append(env("a")); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, spoolPositionName),
		[]byte(segmentName(999)+"\n4096\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	second := testSpool(t, spoolOptions{Dir: dir})
	if got := drainSpool(t, second); len(got) != 1 || got[0] != "a" {
		t.Fatalf("got %v, want [a] — a stale position lost readable data", got)
	}
}

// A run that never overflows still opens a segment. Restarting must not leave
// a growing pile of them behind.
func TestSpool_RestartsDoNotAccumulateEmptySegments(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		s, err := openSpool(spoolOptions{Dir: dir, SyncInterval: time.Hour})
		if err != nil {
			t.Fatalf("restart %d: %v", i, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	last, err := openSpool(spoolOptions{Dir: dir, SyncInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer last.Close()

	segs, err := last.segments()
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 1 {
		t.Fatalf("after 6 quiet starts the directory holds %d segments, want 1: %v", len(segs), segs)
	}
}

// Segment numbers must never be reused, or a recycled name could sort ahead of
// data written earlier and be read out of order.
func TestSpool_DoesNotReuseSegmentNumbers(t *testing.T) {
	dir := t.TempDir()
	first := testSpool(t, spoolOptions{Dir: dir})
	seen := map[string]bool{first.wName: true}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 4; i++ {
		s, err := openSpool(spoolOptions{Dir: dir, SyncInterval: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		if seen[s.wName] {
			t.Fatalf("restart %d reused segment name %s", i, s.wName)
		}
		seen[s.wName] = true
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
