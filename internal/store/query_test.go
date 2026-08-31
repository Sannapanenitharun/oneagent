package store

import (
	"context"
	"testing"
	"time"
)

// The fleet query is the one place where the backend has to know what the
// agent actually names its metrics. Getting that wrong is invisible: the
// query succeeds, the column is simply always empty, and it reads as a quiet
// host rather than as a wrong name. So these tests write the exact metric
// names the collectors emit and assert a number comes back.

func writeMetric(t *testing.T, c *Client, host, name string, value float64, attrs map[string]string) {
	t.Helper()
	if attrs == nil {
		attrs = map[string]string{}
	}
	row := map[string]any{
		"timestamp":    time.Now().UTC().Format("2006-01-02 15:04:05.000"),
		"name":         name,
		"host_id":      host,
		"service":      "",
		"value":        value,
		"is_monotonic": uint8(0),
		"attributes":   attrs,
	}
	if err := c.Insert(context.Background(), "metrics", []map[string]any{row}); err != nil {
		t.Fatalf("insert %s: %v", name, err)
	}
}

func writeHost(t *testing.T, c *Client, host, agent string) {
	t.Helper()
	ts := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	row := map[string]any{
		"host_id": host, "agent_id": agent,
		"last_seen": ts, "first_seen": ts,
		"attributes": map[string]string{"host.name": agent},
	}
	if err := c.Insert(context.Background(), "hosts", []map[string]any{row}); err != nil {
		t.Fatalf("insert host: %v", err)
	}
}

func hostByID(t *testing.T, hosts []Host, id string) Host {
	t.Helper()
	for _, h := range hosts {
		if h.HostID == id {
			return h
		}
	}
	t.Fatalf("host %q not in fleet result (%d hosts)", id, len(hosts))
	return Host{}
}

// The names here are the ones internal/collector/metrics.go emits. If a
// collector is renamed, this fails rather than the column silently emptying.
func TestHosts_ReadsTheAgentsOwnMetricNames(t *testing.T) {
	c := testClient(t)
	const id = "i-agentnames"
	writeHost(t, c, id, "web-1")
	writeMetric(t, c, id, "host.cpu.used_pct", 42.5, nil)
	writeMetric(t, c, id, "host.memory.used_pct", 71.25, nil)

	hosts, err := c.Hosts(context.Background(), 10*time.Minute)
	if err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	h := hostByID(t, hosts, id)
	if h.CPUPct == nil || *h.CPUPct != 42.5 {
		t.Errorf("cpu = %v, want 42.5", h.CPUPct)
	}
	if h.MemPct == nil || *h.MemPct != 71.25 {
		t.Errorf("mem = %v, want 71.25", h.MemPct)
	}
}

// Disk was the one that was wrong: nothing emits a disk percentage at all.
// The agent reports system.filesystem.usage in bytes, split used/free per
// mountpoint, so the percentage has to be computed from the pair.
func TestHosts_ComputesDiskFromFilesystemBytes(t *testing.T) {
	c := testClient(t)
	const id = "i-diskbytes"
	writeHost(t, c, id, "db-1")
	root := func(state string) map[string]string {
		return map[string]string{"state": state, "mountpoint": "/", "device": "/dev/root"}
	}
	writeMetric(t, c, id, "system.filesystem.usage", 30e9, root("used"))
	writeMetric(t, c, id, "system.filesystem.usage", 70e9, root("free"))

	hosts, err := c.Hosts(context.Background(), 10*time.Minute)
	if err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	h := hostByID(t, hosts, id)
	if h.DiskPct == nil {
		t.Fatal("disk is empty despite filesystem usage being reported")
	}
	if got := *h.DiskPct; got < 29.9 || got > 30.1 {
		t.Errorf("disk = %v, want ~30 (30GB used of 100GB)", got)
	}
}

// A non-root mount must not be mistaken for the host disk, or a full /boot
// would show the machine as full.
func TestHosts_IgnoresNonRootMountsForDisk(t *testing.T) {
	c := testClient(t)
	const id = "i-bootmount"
	writeHost(t, c, id, "boot-1")
	writeMetric(t, c, id, "system.filesystem.usage", 95e6,
		map[string]string{"state": "used", "mountpoint": "/boot"})
	writeMetric(t, c, id, "system.filesystem.usage", 5e6,
		map[string]string{"state": "free", "mountpoint": "/boot"})

	hosts, err := c.Hosts(context.Background(), 10*time.Minute)
	if err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	if h := hostByID(t, hosts, id); h.DiskPct != nil {
		t.Errorf("disk = %v from a /boot-only host, want empty", *h.DiskPct)
	}
}

// The README promises any OTLP producer can send here. The semantic
// convention names carry a 0..1 ratio, not a percentage.
func TestHosts_AcceptsSemconvNamesAndScalesTheRatio(t *testing.T) {
	c := testClient(t)
	const id = "i-semconv"
	writeHost(t, c, id, "otel-1")
	writeMetric(t, c, id, "system.cpu.utilization", 0.271, nil)
	writeMetric(t, c, id, "system.memory.utilization", 0.813, nil)

	hosts, err := c.Hosts(context.Background(), 10*time.Minute)
	if err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	h := hostByID(t, hosts, id)
	if h.CPUPct == nil || *h.CPUPct < 27.0 || *h.CPUPct > 27.2 {
		t.Errorf("cpu = %v, want ~27.1 (0.271 scaled)", h.CPUPct)
	}
	if h.MemPct == nil || *h.MemPct < 81.2 || *h.MemPct > 81.4 {
		t.Errorf("mem = %v, want ~81.3", h.MemPct)
	}
}

// Both families present: the agent's own value is the one it computed, so it
// wins rather than being overwritten by the other.
func TestHosts_PrefersTheAgentsOwnValueOverSemconv(t *testing.T) {
	c := testClient(t)
	const id = "i-both"
	writeHost(t, c, id, "both-1")
	writeMetric(t, c, id, "host.cpu.used_pct", 42.0, nil)
	writeMetric(t, c, id, "system.cpu.utilization", 0.99, nil)

	hosts, err := c.Hosts(context.Background(), 10*time.Minute)
	if err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	if h := hostByID(t, hosts, id); h.CPUPct == nil || *h.CPUPct != 42.0 {
		t.Errorf("cpu = %v, want 42 (the value the agent computed)", h.CPUPct)
	}
}

// Presence is per metric. A host reporting CPU but no filesystem usage —
// every host with the infra collector disabled — must still show its CPU.
func TestHosts_MissingMetricDoesNotBlankThePresentOnes(t *testing.T) {
	c := testClient(t)
	const id = "i-partial"
	writeHost(t, c, id, "partial-1")
	writeMetric(t, c, id, "host.cpu.used_pct", 15.0, nil)

	hosts, err := c.Hosts(context.Background(), 10*time.Minute)
	if err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	h := hostByID(t, hosts, id)
	if h.CPUPct == nil || *h.CPUPct != 15.0 {
		t.Errorf("cpu = %v, want 15", h.CPUPct)
	}
	if h.MemPct != nil {
		t.Errorf("mem = %v, want empty", *h.MemPct)
	}
	if h.DiskPct != nil {
		t.Errorf("disk = %v, want empty", *h.DiskPct)
	}
}

// A genuine zero is a reading, not an absence. This is what the pointer type
// exists for, and why presence cannot be inferred from the value being 0.
func TestHosts_ZeroIsAReadingNotAnAbsence(t *testing.T) {
	c := testClient(t)
	const id = "i-idle"
	writeHost(t, c, id, "idle-1")
	writeMetric(t, c, id, "host.cpu.used_pct", 0, nil)

	hosts, err := c.Hosts(context.Background(), 10*time.Minute)
	if err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	h := hostByID(t, hosts, id)
	if h.CPUPct == nil {
		t.Fatal("an idle host reporting 0% CPU came back as having no reading")
	}
	if *h.CPUPct != 0 {
		t.Errorf("cpu = %v, want 0", *h.CPUPct)
	}
}

// A host that has not reported inside the window keeps its inventory row but
// has no current numbers.
func TestHosts_OutsideTheWindowHasNoNumbers(t *testing.T) {
	c := testClient(t)
	const id = "i-stale"
	writeHost(t, c, id, "stale-1")
	old := time.Now().UTC().Add(-2 * time.Hour).Format("2006-01-02 15:04:05.000")
	row := map[string]any{
		"timestamp": old, "name": "host.cpu.used_pct", "host_id": id,
		"service": "", "value": 88.0, "is_monotonic": uint8(0),
		"attributes": map[string]string{},
	}
	if err := c.Insert(context.Background(), "metrics", []map[string]any{row}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	hosts, err := c.Hosts(context.Background(), 5*time.Minute)
	if err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	h := hostByID(t, hosts, id)
	if h.CPUPct != nil {
		t.Errorf("cpu = %v from a reading two hours old in a 5m window", *h.CPUPct)
	}
	if h.AgentID != "stale-1" {
		t.Errorf("agent_id = %q, want the host still listed", h.AgentID)
	}
}

// Load average is a gauge, so the fleet table can read it directly. IOWait is
// deliberately not here: it is a share of a cumulative counter and needs a
// rate across two points, which this query does not attempt.
func TestHosts_ReadsLoadAverage(t *testing.T) {
	c := testClient(t)
	const id = "i-load"
	writeHost(t, c, id, "load-1")
	writeMetric(t, c, id, "system.cpu.load_average.15m", 1.75, nil)

	hosts, err := c.Hosts(context.Background(), 10*time.Minute)
	if err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	h := hostByID(t, hosts, id)
	if h.Load15 == nil || *h.Load15 != 1.75 {
		t.Errorf("load15 = %v, want 1.75", h.Load15)
	}
}

// An unloaded machine reports 0.00, which is a reading like any other.
func TestHosts_ZeroLoadIsAReading(t *testing.T) {
	c := testClient(t)
	const id = "i-noload"
	writeHost(t, c, id, "noload-1")
	writeMetric(t, c, id, "system.cpu.load_average.15m", 0, nil)

	hosts, err := c.Hosts(context.Background(), 10*time.Minute)
	if err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	if h := hostByID(t, hosts, id); h.Load15 == nil {
		t.Fatal("a load of 0.00 came back as no reading")
	}
}

// A host with no load metric leaves the column empty rather than showing 0.
func TestHosts_AbsentLoadIsEmpty(t *testing.T) {
	c := testClient(t)
	const id = "i-cpuonly"
	writeHost(t, c, id, "cpuonly-1")
	writeMetric(t, c, id, "host.cpu.used_pct", 20, nil)

	hosts, err := c.Hosts(context.Background(), 10*time.Minute)
	if err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	if h := hostByID(t, hosts, id); h.Load15 != nil {
		t.Errorf("load15 = %v, want empty", *h.Load15)
	}
}

// writeHostAttrs writes an inventory row with an explicit attribute set.
func writeHostAttrs(t *testing.T, c *Client, host, agent string, seen time.Time, attrs map[string]string) {
	t.Helper()
	ts := seen.UTC().Format("2006-01-02 15:04:05.000")
	row := map[string]any{
		"host_id": host, "agent_id": agent,
		"last_seen": ts, "first_seen": ts, "attributes": attrs,
	}
	if err := c.Insert(context.Background(), "hosts", []map[string]any{row}); err != nil {
		t.Fatalf("insert host: %v", err)
	}
}

// A later export carrying a thinner resource must not erase what a richer one
// established. ReplacingMergeTree keeps the newest row, so without the query
// preferring the fullest description, one sparse export blanks the OS,
// account and zone columns for that host — with no error anywhere.
func TestHosts_SparseExportDoesNotEraseTheDescription(t *testing.T) {
	c := testClient(t)
	const id = "i-sparse"
	now := time.Now()
	writeHostAttrs(t, c, id, "rich-1", now.Add(-time.Minute), map[string]string{
		"host.id":                 id,
		"host.name":               "rich-1",
		"os.description":          "Ubuntu 24.04.4 LTS",
		"cloud.account.id":        "123456789012",
		"cloud.availability_zone": "us-east-1d",
	})
	// An application on the same host sending its own telemetry, which knows
	// the host id and nothing else about the machine.
	writeHostAttrs(t, c, id, "rich-1", now, map[string]string{"host.id": id})

	hosts, err := c.Hosts(context.Background(), 10*time.Minute)
	if err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	h := hostByID(t, hosts, id)
	if got := h.Attributes["os.description"]; got != "Ubuntu 24.04.4 LTS" {
		t.Errorf("os.description = %q, want it to survive the sparse export", got)
	}
	if got := h.Attributes["cloud.account.id"]; got != "123456789012" {
		t.Errorf("cloud.account.id = %q, want it to survive", got)
	}
}

// Liveness must still come from the newest row, not from the row the
// description was taken from — otherwise a host whose fullest export is an
// hour old would read as an hour stale while actively reporting.
func TestHosts_LastSeenTracksTheNewestRowNotTheRichest(t *testing.T) {
	c := testClient(t)
	const id = "i-liveness"
	now := time.Now().UTC().Truncate(time.Second)
	writeHostAttrs(t, c, id, "live-1", now.Add(-time.Hour), map[string]string{
		"host.id": id, "os.description": "Debian GNU/Linux 12", "host.arch": "amd64",
	})
	writeHostAttrs(t, c, id, "live-1", now, map[string]string{"host.id": id})

	hosts, err := c.Hosts(context.Background(), 10*time.Minute)
	if err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	h := hostByID(t, hosts, id)
	if want := now.Format("2006-01-02T15:04:05Z"); h.LastSeen != want {
		t.Errorf("last_seen = %q, want %q (the newest row)", h.LastSeen, want)
	}
	if h.Attributes["os.description"] != "Debian GNU/Linux 12" {
		t.Errorf("description was lost while taking last_seen from the newer row")
	}
}

// An updated value on an equally complete row is the newer one, not the older.
func TestHosts_EqualDetailPrefersTheNewerRow(t *testing.T) {
	c := testClient(t)
	const id = "i-updated"
	now := time.Now()
	writeHostAttrs(t, c, id, "up-1", now.Add(-time.Minute), map[string]string{
		"host.id": id, "os.description": "Ubuntu 22.04",
	})
	writeHostAttrs(t, c, id, "up-1", now, map[string]string{
		"host.id": id, "os.description": "Ubuntu 24.04",
	})

	hosts, err := c.Hosts(context.Background(), 10*time.Minute)
	if err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	if h := hostByID(t, hosts, id); h.Attributes["os.description"] != "Ubuntu 24.04" {
		t.Errorf("os.description = %q, want the upgraded value", h.Attributes["os.description"])
	}
}

// The display name must come from the same row as the description. A sparse
// export has no host.name and falls back to the host id, so taking the name
// from the newest row renames the machine in the table while its OS and
// account stay right — a row that disagrees with itself.
func TestHosts_NameAndDescriptionComeFromTheSameRow(t *testing.T) {
	c := testClient(t)
	const id = "i-naming"
	now := time.Now()
	writeHostAttrs(t, c, id, "web-prod-1", now.Add(-time.Minute), map[string]string{
		"host.id": id, "host.name": "web-prod-1", "os.description": "Ubuntu 24.04.4 LTS",
	})
	// The fallback an id-only resource produces in convert.go.
	writeHostAttrs(t, c, id, id, now, map[string]string{"host.id": id})

	hosts, err := c.Hosts(context.Background(), 10*time.Minute)
	if err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	h := hostByID(t, hosts, id)
	if h.AgentID != "web-prod-1" {
		t.Errorf("agent_id = %q, want web-prod-1", h.AgentID)
	}
	if h.Attributes["os.description"] != "Ubuntu 24.04.4 LTS" {
		t.Errorf("description = %q, want it to match the name's row", h.Attributes["os.description"])
	}
}

// The one that a query-side fix alone does not survive.
//
// ReplacingMergeTree does not merely hide the losing row, it deletes it during
// a background merge. So a query that prefers the fullest description works
// only until ClickHouse compacts the part, after which the fuller row does not
// exist anywhere and no query can recover it. OPTIMIZE FINAL forces that merge
// immediately rather than waiting for the scheduler to do it at some
// unpredictable point — which is exactly how this got through the first time.
func TestHosts_DescriptionSurvivesAMerge(t *testing.T) {
	c := testClient(t)
	const id = "i-merged"
	now := time.Now()
	writeHostAttrs(t, c, id, "prod-1", now.Add(-time.Minute), map[string]string{
		"host.id":                 id,
		"host.name":               "prod-1",
		"os.description":          "Ubuntu 24.04.4 LTS",
		"cloud.account.id":        "123456789012",
		"cloud.availability_zone": "us-east-1d",
	})
	writeHostAttrs(t, c, id, id, now, map[string]string{"host.id": id})

	if _, err := c.execRaw(context.Background(), "OPTIMIZE TABLE hosts FINAL", nil, true); err != nil {
		t.Fatalf("OPTIMIZE: %v", err)
	}

	hosts, err := c.Hosts(context.Background(), 10*time.Minute)
	if err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	h := hostByID(t, hosts, id)
	if h.Attributes["os.description"] != "Ubuntu 24.04.4 LTS" {
		t.Errorf("os.description = %q — the merge destroyed the fuller row",
			h.Attributes["os.description"])
	}
	if h.AgentID != "prod-1" {
		t.Errorf("agent_id = %q, want prod-1", h.AgentID)
	}
}

// Equally complete rows must still collapse, or the table grows without bound:
// a steady agent writes one of these per flush, forever.
func TestHosts_EqualRowsStillDeduplicate(t *testing.T) {
	c := testClient(t)
	const id = "i-dedup"
	now := time.Now()
	attrs := map[string]string{"host.id": id, "host.name": "dedup-1", "os.type": "linux"}
	for i := 0; i < 5; i++ {
		writeHostAttrs(t, c, id, "dedup-1", now.Add(time.Duration(i)*time.Second), attrs)
	}
	if _, err := c.execRaw(context.Background(), "OPTIMIZE TABLE hosts FINAL", nil, true); err != nil {
		t.Fatalf("OPTIMIZE: %v", err)
	}

	var rows []struct {
		N string `json:"n"`
	}
	if err := c.Query(context.Background(),
		"SELECT toString(count()) AS n FROM hosts WHERE host_id = {id:String}",
		map[string]string{"id": id}, &rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if len(rows) != 1 || rows[0].N != "1" {
		t.Errorf("%v rows after a merge of five identical descriptions, want 1", rows)
	}
}
