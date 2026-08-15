package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/agent-i/agent/internal/collector"
)

func metric(name string, v float64, ts time.Time, labels map[string]string) collector.Envelope {
	return collector.Envelope{
		Kind: collector.KindMetric, AgentID: "host-001", Source: name,
		Timestamp: ts, Value: v, Labels: labels,
	}
}

func TestStore_GroupsSamplesIntoOneSeriesPerLabelSet(t *testing.T) {
	s := NewStore("host-001", "v1", time.Minute, 100)
	base := time.Now().UTC()
	for i := 0; i < 5; i++ {
		ts := base.Add(time.Duration(i) * time.Second)
		s.Record(metric("system.cpu.time", float64(i), ts, map[string]string{"state": "idle", "cpu": "cpu-total"}))
		s.Record(metric("system.cpu.time", float64(i)*2, ts, map[string]string{"state": "user", "cpu": "cpu-total"}))
	}

	snap := s.Snapshot()
	if len(snap.Series) != 2 {
		t.Fatalf("expected 2 series (one per state), got %d: %+v", len(snap.Series), snap.Series)
	}
	for _, ser := range snap.Series {
		if len(ser.Points) != 5 {
			t.Errorf("series %v: expected 5 points, got %d", ser.Labels, len(ser.Points))
		}
		if !ser.Cumulative {
			t.Errorf("system.cpu.time should be marked cumulative so the UI plots a rate, not a climbing counter")
		}
	}
	if snap.Counts["metric"] != 10 {
		t.Errorf("metric count = %d, want 10", snap.Counts["metric"])
	}
}

// Go randomizes map iteration order. If the series key were built by ranging
// over labels without sorting, the same series would key differently on each
// sample and fragment into many — which is exactly the bug this guards.
func TestStore_LabelOrderDoesNotFragmentSeries(t *testing.T) {
	s := NewStore("host-001", "v1", time.Minute, 100)
	ts := time.Now().UTC()
	for i := 0; i < 40; i++ {
		s.Record(metric("system.disk.io", 1, ts.Add(time.Duration(i)*time.Second), map[string]string{
			"device": "nvme0n1", "direction": "read", "extra": "x", "more": "y",
		}))
	}
	if got := len(s.Snapshot().Series); got != 1 {
		t.Fatalf("expected 1 series regardless of label map ordering, got %d", got)
	}
}

func TestStore_InternalLabelsAreHidden(t *testing.T) {
	s := NewStore("host-001", "v1", time.Minute, 100)
	s.Record(metric("system.cpu.time", 1, time.Now().UTC(), map[string]string{
		"state": "idle", "_boot_time_unix": "1700000000",
	}))
	ser := s.Snapshot().Series[0]
	if _, leaked := ser.Labels["_boot_time_unix"]; leaked {
		t.Errorf("internal _-prefixed label leaked into the dashboard: %+v", ser.Labels)
	}
	if ser.Labels["state"] != "idle" {
		t.Errorf("real label lost: %+v", ser.Labels)
	}
}

func TestStore_DropsPointsOlderThanRetention(t *testing.T) {
	s := NewStore("host-001", "v1", 30*time.Second, 100)
	now := time.Now().UTC()
	s.nowFn = func() time.Time { return now }

	s.Record(metric("m", 1, now.Add(-5*time.Minute), nil)) // well outside the window
	s.Record(metric("m", 2, now.Add(-10*time.Second), nil))
	s.Record(metric("m", 3, now, nil))

	pts := s.Snapshot().Series[0].Points
	if len(pts) != 2 {
		t.Fatalf("expected the aged-out point to be trimmed, got %d points: %+v", len(pts), pts)
	}
	if pts[0].V != 2 || pts[1].V != 3 {
		t.Errorf("wrong points retained: %+v", pts)
	}
}

// The cap must refuse new series rather than evict existing ones — and must
// say so, since a silently partial view is worse than an obviously partial
// one when you are using this to debug whether collection works.
func TestStore_RefusesNewSeriesPastCapAndReportsIt(t *testing.T) {
	s := NewStore("host-001", "v1", time.Minute, 3)
	ts := time.Now().UTC()
	for i := 0; i < 10; i++ {
		s.Record(metric("m", 1, ts, map[string]string{"i": fmt.Sprint(i)}))
	}
	snap := s.Snapshot()
	if len(snap.Series) != 3 {
		t.Errorf("expected the series count to stop at the cap of 3, got %d", len(snap.Series))
	}
	if snap.SeriesDropped != 7 {
		t.Errorf("series_dropped = %d, want 7 — the UI needs this to warn the view is incomplete", snap.SeriesDropped)
	}
}

func TestStore_BoundsLogsAndSpans(t *testing.T) {
	s := NewStore("host-001", "v1", time.Hour, 100)
	ts := time.Now().UTC()
	for i := 0; i < maxLogs+120; i++ {
		s.Record(collector.Envelope{Kind: collector.KindLog, Source: "app.log", Timestamp: ts, Message: fmt.Sprintf("line %d", i)})
	}
	for i := 0; i < maxSpans+90; i++ {
		s.Record(collector.Envelope{
			Kind: collector.KindTrace, Source: "otlp.span", Timestamp: ts, Value: float64(i),
			Labels: map[string]string{"trace_id": "t", "span_id": fmt.Sprint(i), "name": "op", "service.name": "svc"},
		})
	}
	snap := s.Snapshot()
	if len(snap.Logs) != maxLogs {
		t.Errorf("logs = %d, want capped at %d", len(snap.Logs), maxLogs)
	}
	if len(snap.Spans) != maxSpans {
		t.Errorf("spans = %d, want capped at %d", len(snap.Spans), maxSpans)
	}
	// Oldest dropped, newest kept.
	if snap.Logs[len(snap.Logs)-1].Message != fmt.Sprintf("line %d", maxLogs+119) {
		t.Errorf("expected the newest log line to survive, got %q", snap.Logs[len(snap.Logs)-1].Message)
	}
}

// Logs and spans were previously bounded by count alone, so on a host with
// light traffic the view kept serving an hours-old span while advertising a
// 15-minute window. Age must bound them too.
func TestStore_DropsLogsAndSpansOlderThanRetention(t *testing.T) {
	s := NewStore("host-001", "v1", 30*time.Second, 100)
	now := time.Now().UTC()
	s.nowFn = func() time.Time { return now }

	old, fresh := now.Add(-5*time.Minute), now.Add(-5*time.Second)
	s.Record(collector.Envelope{Kind: collector.KindLog, Source: "app.log", Timestamp: old, Message: "stale"})
	s.Record(collector.Envelope{Kind: collector.KindLog, Source: "app.log", Timestamp: fresh, Message: "current"})
	s.Record(collector.Envelope{
		Kind: collector.KindTrace, Source: "otlp.span", Timestamp: old,
		Labels: map[string]string{"span_id": "stale"},
	})
	s.Record(collector.Envelope{
		Kind: collector.KindTrace, Source: "otlp.span", Timestamp: fresh,
		Labels: map[string]string{"span_id": "current"},
	})

	snap := s.Snapshot()
	if len(snap.Logs) != 1 || snap.Logs[0].Message != "current" {
		t.Errorf("aged-out log survived: %+v", snap.Logs)
	}
	if len(snap.Spans) != 1 || snap.Spans[0].SpanID != "current" {
		t.Errorf("aged-out span survived: %+v", snap.Spans)
	}
}

// The failure this guards is a collector going silent. Trimming used to happen
// only when a sample arrived, so no samples meant no trimming and the last
// points stayed on the chart indefinitely — which reads as "steady" when the
// truth is "stopped".
func TestStore_QuietCollectorAgesOutWithoutNewSamples(t *testing.T) {
	s := NewStore("host-001", "v1", 30*time.Second, 100)
	now := time.Now().UTC()
	s.nowFn = func() time.Time { return now }

	s.Record(metric("system.cpu.time", 1, now, map[string]string{"state": "idle"}))
	if len(s.Snapshot().Series) != 1 {
		t.Fatal("series should be present while fresh")
	}

	// Nothing else is ever recorded; only the clock moves.
	now = now.Add(10 * time.Minute)

	snap := s.Snapshot()
	if len(snap.Series) != 0 {
		t.Errorf("stale series still served after going quiet: %+v", snap.Series)
	}
	// The slot must be released too, or a host whose containers come and go
	// exhausts maxSeries with series that no longer exist.
	if got := len(s.series); got != 0 {
		t.Errorf("emptied series not released from the map: %d retained", got)
	}
	// Counts are lifetime totals, not windowed — they must survive pruning, or
	// "is this agent collecting anything at all?" becomes unanswerable.
	if snap.Counts["metric"] != 1 {
		t.Errorf("lifetime counts should not be pruned, got %d", snap.Counts["metric"])
	}
}

func TestStore_SpanFieldsAreMapped(t *testing.T) {
	s := NewStore("host-001", "v1", time.Hour, 100)
	s.Record(collector.Envelope{
		Kind: collector.KindTrace, Source: "otlp.span", Timestamp: time.Now().UTC(), Value: 42.5,
		Labels: map[string]string{
			"trace_id": "abc", "span_id": "def", "name": "GET /x",
			"service.name": "checkout", "status.code": "2",
		},
	})
	sp := s.Snapshot().Spans[0]
	if sp.TraceID != "abc" || sp.SpanID != "def" || sp.Name != "GET /x" ||
		sp.Service != "checkout" || sp.Status != "2" || sp.DurMs != 42.5 {
		t.Errorf("span mapped wrong: %+v", sp)
	}
}

// Record runs on the drain loop while Snapshot runs on an HTTP handler
// goroutine. Run with -race; without copying in Snapshot this trips.
func TestStore_ConcurrentRecordAndSnapshot(t *testing.T) {
	s := NewStore("host-001", "v1", time.Minute, 200)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		ts := time.Now().UTC()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			s.Record(metric("m", float64(i), ts.Add(time.Duration(i)*time.Millisecond), map[string]string{"k": fmt.Sprint(i % 20)}))
		}
	}()

	for i := 0; i < 200; i++ {
		snap := s.Snapshot()
		for _, ser := range snap.Series {
			_ = len(ser.Points)
		}
	}
	close(stop)
	wg.Wait()
}

func TestServer_ServesIndexAndSnapshot(t *testing.T) {
	st := NewStore("host-001", "v1.2.3", time.Minute, 100)
	st.Record(metric("system.cpu.time", 7, time.Now().UTC(), map[string]string{"state": "idle"}))

	// Port 0 lets the OS choose, so the test never collides with a real agent.
	srv, err := NewServer("127.0.0.1:0", st)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go srv.Serve()
	defer srv.Close()

	base := "http://" + srv.Addr()

	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("GET / = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("GET / content-type = %q", ct)
	}

	sr, err := http.Get(base + "/api/snapshot")
	if err != nil {
		t.Fatalf("GET /api/snapshot: %v", err)
	}
	defer sr.Body.Close()
	var snap Snapshot
	if err := json.NewDecoder(sr.Body).Decode(&snap); err != nil {
		t.Fatalf("decoding snapshot: %v", err)
	}
	if snap.AgentID != "host-001" || snap.Version != "v1.2.3" {
		t.Errorf("snapshot identity wrong: %+v", snap)
	}
	if len(snap.Series) != 1 || snap.Series[0].Name != "system.cpu.time" {
		t.Errorf("snapshot series wrong: %+v", snap.Series)
	}

	// An unknown path must 404 rather than fall through to the index — a
	// wildcard "/" handler serving HTML for every path makes a typo'd API
	// call look like a successful page load.
	nf, err := http.Get(base + "/nope")
	if err != nil {
		t.Fatalf("GET /nope: %v", err)
	}
	defer nf.Body.Close()
	if nf.StatusCode != 404 {
		t.Errorf("GET /nope = %d, want 404", nf.StatusCode)
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8088": true,
		"localhost:8088": true,
		"[::1]:8088":     true,
		"0.0.0.0:8088":   false,
		"192.168.1.5:80": false,
		":8088":          false,
	}
	for addr, want := range cases {
		if got := isLoopbackAddr(addr); got != want {
			t.Errorf("isLoopbackAddr(%q) = %t, want %t", addr, got, want)
		}
	}
}
