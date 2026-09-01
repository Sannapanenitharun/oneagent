package store

import (
	"context"
	"testing"
	"time"
)

// The contract these test is the dashboard's, not the database's: the payload
// has to match what frontend/src/adapters.js already reads, field for field
// and unit for unit. A snapshot that is correct about the data and wrong about
// the shape renders as an empty page with no error anywhere.

func writeLog(t *testing.T, c *Client, host, service, severity, body string, at time.Time, attrs map[string]string) {
	t.Helper()
	if attrs == nil {
		attrs = map[string]string{}
	}
	row := map[string]any{
		"timestamp":    at.UTC().Format("2006-01-02 15:04:05.000000000"),
		"host_id":      host,
		"service":      service,
		"severity":     severity,
		"severity_num": uint8(9),
		"body":         body,
		"trace_id":     "",
		"span_id":      "",
		"attributes":   attrs,
	}
	if err := c.Insert(context.Background(), "logs", []map[string]any{row}); err != nil {
		t.Fatalf("insert log: %v", err)
	}
}

func writeSpan(t *testing.T, c *Client, host string, sp map[string]any) {
	t.Helper()
	row := map[string]any{
		"timestamp": time.Now().UTC().Format("2006-01-02 15:04:05.000000000"),
		"trace_id":  "aa", "span_id": "bb", "parent_span_id": "",
		"service": "svc", "name": "op", "kind": "internal",
		"duration_ns": uint64(0), "status_code": "unset", "status_message": "",
		"host_id": host, "attributes": map[string]string{},
	}
	for k, v := range sp {
		row[k] = v
	}
	if err := c.Insert(context.Background(), "spans", []map[string]any{row}); err != nil {
		t.Fatalf("insert span: %v", err)
	}
}

func TestSnapshot_RequiresAHost(t *testing.T) {
	c := testClient(t)
	if _, err := c.Snapshot(context.Background(), "", time.Hour); err == nil {
		t.Fatal("an empty host produced a snapshot instead of an error")
	}
}

// A host that exists but has reported nothing must answer, with empty slices.
// Nil would encode as JSON null, and the views iterate these — a null where an
// array is expected is a render error, not an empty state.
func TestSnapshot_QuietHostAnswersWithEmptySlices(t *testing.T) {
	c := testClient(t)
	const id = "i-quiet"
	writeHost(t, c, id, "quiet-1")

	snap, err := c.Snapshot(context.Background(), id, 15*time.Minute)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Series == nil || snap.Logs == nil || snap.Spans == nil {
		t.Error("nil slice in the payload; the dashboard iterates these")
	}
	if snap.AgentID != "quiet-1" {
		t.Errorf("agent_id = %q, want quiet-1", snap.AgentID)
	}
	if snap.Now == 0 {
		t.Error("now is zero; host age is measured against it")
	}
}

// A host with no inventory row at all still answers under its own id, rather
// than an empty name that renders as a blank heading.
func TestSnapshot_UnknownHostFallsBackToItsID(t *testing.T) {
	c := testClient(t)
	snap, err := c.Snapshot(context.Background(), "i-never-seen", 15*time.Minute)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.AgentID != "i-never-seen" {
		t.Errorf("agent_id = %q, want the host id", snap.AgentID)
	}
}

func TestSnapshot_SeriesCarryPointsAndCumulativeFlag(t *testing.T) {
	c := testClient(t)
	const id = "i-series"
	writeHost(t, c, id, "series-1")
	writeMetric(t, c, id, "host.cpu.used_pct", 12.5, nil)

	// A counter, which the UI must differentiate into a rate before plotting.
	row := map[string]any{
		"timestamp": time.Now().UTC().Format("2006-01-02 15:04:05.000"),
		"name":      "system.network.io", "host_id": id, "service": "",
		"value": 1000.0, "is_monotonic": uint8(1),
		"attributes": map[string]string{"device": "eth0"},
	}
	if err := c.Insert(context.Background(), "metrics", []map[string]any{row}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	snap, err := c.Snapshot(context.Background(), id, 15*time.Minute)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	var gauge, counter *Series
	for i := range snap.Series {
		switch snap.Series[i].Name {
		case "host.cpu.used_pct":
			gauge = &snap.Series[i]
		case "system.network.io":
			counter = &snap.Series[i]
		}
	}
	if gauge == nil || counter == nil {
		t.Fatalf("missing series; got %d", len(snap.Series))
	}
	if gauge.Cumulative {
		t.Error("a gauge was marked cumulative; the UI would differentiate it into nonsense")
	}
	if !counter.Cumulative {
		t.Error("a counter was not marked cumulative; the UI would plot a climbing line")
	}
	if len(gauge.Points) == 0 || gauge.Points[0].V != 12.5 {
		t.Errorf("gauge points = %v, want one at 12.5", gauge.Points)
	}
	// Milliseconds, not seconds or nanoseconds: the browser's Date reads this
	// directly, and the wrong unit puts every point in 1970 or in the year
	// 55000 without failing anywhere.
	if gauge.Points[0].T < 1_600_000_000_000 || gauge.Points[0].T > 4_000_000_000_000 {
		t.Errorf("timestamp %d is not unix milliseconds", gauge.Points[0].T)
	}
}

// One metric name with different labels is several series. Collapsing them
// would hide a saturated disk behind an idle one.
func TestSnapshot_SplitsSeriesByLabelSet(t *testing.T) {
	c := testClient(t)
	const id = "i-labels"
	writeHost(t, c, id, "labels-1")
	writeMetric(t, c, id, "system.disk.io", 100, map[string]string{"device": "sda", "direction": "read"})
	writeMetric(t, c, id, "system.disk.io", 200, map[string]string{"device": "sdb", "direction": "read"})

	snap, err := c.Snapshot(context.Background(), id, 15*time.Minute)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	n := 0
	for _, s := range snap.Series {
		if s.Name == "system.disk.io" {
			n++
			if s.Labels["device"] == "" {
				t.Error("series lost its device label")
			}
		}
	}
	if n != 2 {
		t.Errorf("%d series for two devices, want 2", n)
	}
}

// Resource attributes are on every row for a host, so they cannot distinguish
// one series from another — carrying them would put the machine's whole
// description on every line of the legend.
func TestSnapshot_DropsResourceAttributesFromLabels(t *testing.T) {
	c := testClient(t)
	const id = "i-reslabels"
	writeHost(t, c, id, "res-1")
	writeMetric(t, c, id, "host.cpu.used_pct", 5, map[string]string{
		"host.id": id, "os.type": "linux", "cloud.account.id": "1234", "state": "user",
	})

	snap, err := c.Snapshot(context.Background(), id, 15*time.Minute)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Series) == 0 {
		t.Fatal("no series")
	}
	l := snap.Series[0].Labels
	for _, k := range []string{"host.id", "os.type", "cloud.account.id"} {
		if _, ok := l[k]; ok {
			t.Errorf("label %q survived; it is identical on every row", k)
		}
	}
	if l["state"] != "user" {
		t.Errorf("labels = %v, want the state label kept", l)
	}
}

// Oldest first, matching the agent, because the log view scrolls that way.
func TestSnapshot_LogsAreOldestFirst(t *testing.T) {
	c := testClient(t)
	const id = "i-logs"
	writeHost(t, c, id, "logs-1")
	now := time.Now()
	writeLog(t, c, id, "api", "INFO", "first", now.Add(-2*time.Second), nil)
	writeLog(t, c, id, "api", "ERROR", "second", now.Add(-time.Second), nil)

	snap, err := c.Snapshot(context.Background(), id, 15*time.Minute)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Logs) != 2 {
		t.Fatalf("%d log lines, want 2", len(snap.Logs))
	}
	if snap.Logs[0].Message != "first" {
		t.Errorf("logs[0] = %q, want the older line first", snap.Logs[0].Message)
	}
	// Severity moves back into the label set, which is where the agent puts it
	// and where the log view reads it from.
	if snap.Logs[1].Labels["level"] != "ERROR" {
		t.Errorf("labels = %v, want level=ERROR", snap.Logs[1].Labels)
	}
	if snap.Logs[0].T < 1_600_000_000_000 {
		t.Errorf("log timestamp %d is not unix milliseconds", snap.Logs[0].T)
	}
}

// Duration in milliseconds as a float. Stored in nanoseconds, and a waterfall
// fed nanoseconds draws every span as the full width of the window.
func TestSnapshot_SpanDurationIsMilliseconds(t *testing.T) {
	c := testClient(t)
	const id = "i-spans"
	writeHost(t, c, id, "spans-1")
	writeSpan(t, c, id, map[string]any{
		"trace_id": "t1", "span_id": "s1", "duration_ns": uint64(2_500_000),
	})

	snap, err := c.Snapshot(context.Background(), id, 15*time.Minute)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Spans) != 1 {
		t.Fatalf("%d spans, want 1", len(snap.Spans))
	}
	if got := snap.Spans[0].DurMs; got != 2.5 {
		t.Errorf("dur_ms = %v, want 2.5", got)
	}
}

// The dashboard treats "2" or "ERROR" as a failed span. An unset status is
// not a success and must not arrive looking like one.
func TestSnapshot_OnlyExplicitErrorsAreMarked(t *testing.T) {
	c := testClient(t)
	const id = "i-status"
	writeHost(t, c, id, "status-1")
	writeSpan(t, c, id, map[string]any{"trace_id": "t1", "span_id": "ok", "status_code": "ok"})
	writeSpan(t, c, id, map[string]any{"trace_id": "t1", "span_id": "un", "status_code": "unset"})
	writeSpan(t, c, id, map[string]any{"trace_id": "t1", "span_id": "er", "status_code": "error"})

	snap, err := c.Snapshot(context.Background(), id, 15*time.Minute)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	byID := map[string]Span{}
	for _, s := range snap.Spans {
		byID[s.SpanID] = s
	}
	if byID["er"].Status != "ERROR" {
		t.Errorf("error span status = %q, want ERROR", byID["er"].Status)
	}
	if byID["ok"].Status != "" || byID["un"].Status != "" {
		t.Errorf("non-error spans carried a status: ok=%q unset=%q",
			byID["ok"].Status, byID["un"].Status)
	}
}

// Whole traces, not the newest N spans. A plain LIMIT returns the tail of a
// trace whose root fell outside it, which is a waterfall that cannot be drawn.
func TestSnapshot_KeepsWholeTraces(t *testing.T) {
	c := testClient(t)
	const id = "i-traces"
	writeHost(t, c, id, "traces-1")
	writeSpan(t, c, id, map[string]any{"trace_id": "t1", "span_id": "root", "parent_span_id": ""})
	writeSpan(t, c, id, map[string]any{"trace_id": "t1", "span_id": "child", "parent_span_id": "root"})

	snap, err := c.Snapshot(context.Background(), id, 15*time.Minute)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	seen := map[string]bool{}
	for _, s := range snap.Spans {
		seen[s.SpanID] = true
	}
	if !seen["root"] || !seen["child"] {
		t.Errorf("trace was split: got %v", seen)
	}
}

// An uninstrumented peer has no span anywhere in the trace, so without these
// attributes a service's databases and queues are absent from the map.
func TestSnapshot_CarriesPeerAttributes(t *testing.T) {
	c := testClient(t)
	const id = "i-peer"
	writeHost(t, c, id, "peer-1")
	writeSpan(t, c, id, map[string]any{
		"trace_id": "t1", "span_id": "s1", "kind": "client",
		"attributes": map[string]string{
			"db.system": "postgresql", "db.name": "orders", "irrelevant": "x",
		},
	})

	snap, err := c.Snapshot(context.Background(), id, 15*time.Minute)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Spans) != 1 {
		t.Fatalf("%d spans, want 1", len(snap.Spans))
	}
	peer := snap.Spans[0].Peer
	if peer["db.system"] != "postgresql" || peer["db.name"] != "orders" {
		t.Errorf("peer = %v, want the db attributes", peer)
	}
	if _, ok := peer["irrelevant"]; ok {
		t.Error("peer carried an unrelated attribute")
	}
}

// Another host's telemetry must not appear under this one.
func TestSnapshot_IsScopedToOneHost(t *testing.T) {
	c := testClient(t)
	const mine, theirs = "i-mine", "i-theirs"
	writeHost(t, c, mine, "mine-1")
	writeHost(t, c, theirs, "theirs-1")
	writeMetric(t, c, mine, "host.cpu.used_pct", 1, nil)
	writeMetric(t, c, theirs, "host.cpu.used_pct", 99, nil)
	writeLog(t, c, theirs, "api", "INFO", "not mine", time.Now(), nil)
	writeSpan(t, c, theirs, map[string]any{"trace_id": "t9", "span_id": "s9"})

	snap, err := c.Snapshot(context.Background(), mine, 15*time.Minute)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	for _, s := range snap.Series {
		for _, p := range s.Points {
			if p.V == 99 {
				t.Error("another host's metric appeared in this snapshot")
			}
		}
	}
	if len(snap.Logs) != 0 {
		t.Errorf("%d logs leaked from another host", len(snap.Logs))
	}
	if len(snap.Spans) != 0 {
		t.Errorf("%d spans leaked from another host", len(snap.Spans))
	}
}

// Data older than the window is not this host's current state.
func TestSnapshot_RespectsTheWindow(t *testing.T) {
	c := testClient(t)
	const id = "i-window"
	writeHost(t, c, id, "window-1")
	old := time.Now().Add(-2 * time.Hour)
	writeLog(t, c, id, "api", "INFO", "ancient", old, nil)

	snap, err := c.Snapshot(context.Background(), id, 15*time.Minute)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Logs) != 0 {
		t.Errorf("a two-hour-old line appeared in a 15-minute window: %v", snap.Logs)
	}
}

// The cap used to keep the first N series as they arrived, and rows arrive
// ordered by metric name — so the cut fell wherever the alphabet put it. On a
// real host that admitted system.network.dropped and system.network.errors and
// refused system.network.io and system.network.packets outright, because "d"
// and "e" sort before "i" and "p". The dashboard rendered those panels as "not
// wired" while four thousand points sat in the database.
func TestCapSeries_NoMetricFamilyDisappears(t *testing.T) {
	var all []Series
	// Mirrors the shape that broke: several families, each with many devices,
	// in alphabetical order.
	for _, name := range []string{
		"system.network.dropped", "system.network.errors",
		"system.network.io", "system.network.packets",
	} {
		for i := 0; i < 74; i++ {
			all = append(all, Series{Name: name, Labels: map[string]string{"device": string(rune('a' + i%26))}})
		}
	}

	got, dropped, err := capSeries(all, 100)
	if err != nil {
		t.Fatalf("capSeries: %v", err)
	}
	if len(got) != 100 {
		t.Errorf("returned %d series, want the full cap of 100", len(got))
	}
	if dropped != uint64(len(all)-100) {
		t.Errorf("dropped = %d, want %d", dropped, len(all)-100)
	}

	seen := map[string]int{}
	for _, s := range got {
		seen[s.Name]++
	}
	for _, name := range []string{
		"system.network.dropped", "system.network.errors",
		"system.network.io", "system.network.packets",
	} {
		if seen[name] == 0 {
			t.Errorf("%s got no series at all — a whole metric family vanished, "+
				"which renders its panel as though nothing collects it", name)
		}
	}
	// Proportional, not merely non-zero: with four equal families and a cap of
	// 100, each should get about a quarter.
	for name, n := range seen {
		if n < 20 || n > 30 {
			t.Errorf("%s got %d of 100 series; want roughly an equal share of the cap", name, n)
		}
	}
}

// Under the cap nothing is touched and nothing is reported as dropped.
func TestCapSeries_UnderTheCapIsUnchanged(t *testing.T) {
	all := []Series{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	got, dropped, err := capSeries(all, 10)
	if err != nil {
		t.Fatalf("capSeries: %v", err)
	}
	if len(got) != 3 || dropped != 0 {
		t.Errorf("got %d series and %d dropped, want 3 and 0", len(got), dropped)
	}
}

// A family with fewer series than its share must not stop the others being
// filled — the rounds simply skip it.
func TestCapSeries_UnevenFamiliesStillFillTheCap(t *testing.T) {
	all := []Series{{Name: "solo"}}
	for i := 0; i < 50; i++ {
		all = append(all, Series{Name: "many"})
	}
	got, dropped, err := capSeries(all, 10)
	if err != nil {
		t.Fatalf("capSeries: %v", err)
	}
	if len(got) != 10 {
		t.Errorf("returned %d series, want the cap of 10 filled", len(got))
	}
	if dropped != uint64(len(all)-10) {
		t.Errorf("dropped = %d, want %d", dropped, len(all)-10)
	}
	var solo int
	for _, s := range got {
		if s.Name == "solo" {
			solo++
		}
	}
	if solo != 1 {
		t.Errorf("the single-series family appears %d times, want exactly once", solo)
	}
}
