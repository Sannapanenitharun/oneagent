package store

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// The queries the dashboard asks.
//
// They live here rather than in the HTTP layer so the SQL is in one place and
// can be read against the schema it depends on. Every one is parameterised;
// none interpolates a value from a request.

// Host is one row of the fleet inventory.
type Host struct {
	HostID     string            `json:"host_id"`
	AgentID    string            `json:"agent_id"`
	LastSeen   string            `json:"last_seen"`
	Attributes map[string]string `json:"attributes"`
	// Metrics the fleet table shows. Absent rather than zero when the host has
	// not reported them, so the UI can tell "0% CPU" from "no CPU reading" —
	// a distinction the agent's own envelope format goes out of its way to
	// preserve, and it would be lost here by defaulting to 0.
	CPUPct  *float64 `json:"cpu_pct"`
	MemPct  *float64 `json:"mem_pct"`
	DiskPct *float64 `json:"disk_pct"`
}

// Hosts returns the fleet: every host that has reported, newest activity
// first, with its latest resource attributes and headline metrics.
//
// FINAL on the hosts table collapses the ReplacingMergeTree duplicates that
// background merges have not yet reached. It is affordable precisely here and
// nowhere else in the schema: hosts is one row per machine.
//
// The metrics come from a separate scan restricted to a short window, joined
// on host_id. Reading the whole retention to find the newest sample per host
// would make the fleet page the most expensive query in the product.
func (c *Client) Hosts(ctx context.Context, window time.Duration) ([]Host, error) {
	if window <= 0 {
		window = 10 * time.Minute
	}
	const q = `
SELECT
    h.host_id                            AS host_id,
    h.agent_id                           AS agent_id,
    formatDateTime(h.last_seen, '%Y-%m-%dT%H:%i:%SZ') AS last_seen,
    h.attributes                         AS attributes,
    m.cpu_pct                            AS cpu_pct,
    m.mem_pct                            AS mem_pct,
    m.disk_pct                           AS disk_pct
FROM (SELECT host_id, agent_id, last_seen, attributes FROM hosts FINAL) AS h
LEFT JOIN (
    SELECT
        host_id,
        -- argMax rather than max: the newest reading, not the largest one
        -- ever seen. max would make a host that briefly spiked look pinned
        -- there forever.
        argMaxIf(value, timestamp, name = 'host.cpu.used_pct')    AS cpu_pct,
        argMaxIf(value, timestamp, name = 'host.memory.used_pct') AS mem_pct,
        argMaxIf(value, timestamp, name = 'host.disk.used_pct')   AS disk_pct
    FROM metrics
    WHERE timestamp >= now() - INTERVAL {window:UInt32} SECOND
      AND name IN ('host.cpu.used_pct', 'host.memory.used_pct', 'host.disk.used_pct')
    GROUP BY host_id
) AS m ON m.host_id = h.host_id
ORDER BY h.last_seen DESC`

	var rows []struct {
		HostID     string            `json:"host_id"`
		AgentID    string            `json:"agent_id"`
		LastSeen   string            `json:"last_seen"`
		Attributes map[string]string `json:"attributes"`
		CPU        float64           `json:"cpu_pct"`
		Mem        float64           `json:"mem_pct"`
		Disk       float64           `json:"disk_pct"`
	}
	params := map[string]string{"window": strconv.Itoa(int(window.Seconds()))}
	if err := c.Query(ctx, q, params, &rows); err != nil {
		return nil, err
	}

	out := make([]Host, 0, len(rows))
	for _, r := range rows {
		h := Host{
			HostID: r.HostID, AgentID: r.AgentID, LastSeen: r.LastSeen,
			Attributes: r.Attributes,
		}
		// A LEFT JOIN with no match yields 0, which is indistinguishable from
		// a genuine zero reading in the result. The host having reported the
		// metric at all is the thing being tested, so presence is decided by
		// the join, not by the value.
		if r.CPU != 0 || r.Mem != 0 || r.Disk != 0 {
			cpu, mem, disk := r.CPU, r.Mem, r.Disk
			h.CPUPct, h.MemPct, h.DiskPct = &cpu, &mem, &disk
		}
		out = append(out, h)
	}
	return out, nil
}

// SeriesPoint is one sample in a time series.
type SeriesPoint struct {
	T float64 `json:"t"` // unix millis, matching the agent snapshot's shape
	V float64 `json:"v"`
}

// Series returns one metric for one host over a window, bucketed.
//
// Bucketing happens in the database rather than the browser. A 24-hour window
// at the agent's 15-second interval is 5,760 points per series, and sending
// them all so the client can average them into 200 pixels is most of a
// dashboard's latency for none of its information.
func (c *Client) Series(ctx context.Context, hostID, name string, window, step time.Duration) ([]SeriesPoint, error) {
	if window <= 0 {
		window = time.Hour
	}
	if step <= 0 {
		step = 15 * time.Second
	}
	const q = `
SELECT
    toUnixTimestamp64Milli(toDateTime64(toStartOfInterval(timestamp, INTERVAL {step:UInt32} SECOND), 3)) AS t,
    avg(value) AS v
FROM metrics
WHERE host_id = {host:String}
  AND name = {name:String}
  AND timestamp >= now() - INTERVAL {window:UInt32} SECOND
GROUP BY t
ORDER BY t`

	var rows []struct {
		T float64 `json:"t,string"`
		V float64 `json:"v"`
	}
	params := map[string]string{
		"host":   hostID,
		"name":   name,
		"window": strconv.Itoa(int(window.Seconds())),
		"step":   strconv.Itoa(int(step.Seconds())),
	}
	if err := c.Query(ctx, q, params, &rows); err != nil {
		return nil, fmt.Errorf("series %s for %s: %w", name, hostID, err)
	}
	out := make([]SeriesPoint, 0, len(rows))
	for _, r := range rows {
		out = append(out, SeriesPoint{T: r.T, V: r.V})
	}
	return out, nil
}

// MetricNames lists what a host has reported, so the UI can offer a real
// choice rather than a hardcoded list that goes stale.
func (c *Client) MetricNames(ctx context.Context, hostID string, window time.Duration) ([]string, error) {
	if window <= 0 {
		window = time.Hour
	}
	const q = `
SELECT DISTINCT name
FROM metrics
WHERE host_id = {host:String}
  AND timestamp >= now() - INTERVAL {window:UInt32} SECOND
ORDER BY name`

	var rows []struct {
		Name string `json:"name"`
	}
	params := map[string]string{"host": hostID, "window": strconv.Itoa(int(window.Seconds()))}
	if err := c.Query(ctx, q, params, &rows); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out, nil
}
