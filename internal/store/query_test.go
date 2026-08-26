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
