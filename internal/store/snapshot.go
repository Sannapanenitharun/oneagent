package store

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Assembling a host's telemetry into the payload the dashboard already reads.
//
// The shape here is the agent's /api/snapshot shape, deliberately and exactly:
// the same field names, the same units, the same conventions about what is
// derived by the consumer rather than sent. That is what lets every view and
// every adapter in the dashboard render a host the browser cannot reach
// without knowing it is looking at a database instead of an agent.
//
// The alternative — a second payload shape with its own endpoints — would mean
// the logs view, the trace waterfall, the flame graph, the service map and
// every percentile in front of them existing twice, once per source, drifting
// apart from the first day. One shape, two producers.

// Point is one sample. T is unix milliseconds, matching the agent.
type Point struct {
	T int64   `json:"t"`
	V float64 `json:"v"`
}

// Series is one metric name and label set over time.
type Series struct {
	Name       string            `json:"name"`
	Labels     map[string]string `json:"labels,omitempty"`
	Cumulative bool              `json:"cumulative"`
	Points     []Point           `json:"points"`
}

// LogLine is one stored log record.
type LogLine struct {
	T       int64             `json:"t"`
	Source  string            `json:"source"`
	Message string            `json:"message"`
	Labels  map[string]string `json:"labels,omitempty"`
}

// Span is one stored span.
type Span struct {
	T        int64             `json:"t"`
	TraceID  string            `json:"trace_id"`
	SpanID   string            `json:"span_id"`
	ParentID string            `json:"parent_id,omitempty"`
	Service  string            `json:"service"`
	Name     string            `json:"name"`
	DurMs    float64           `json:"dur_ms"`
	Status   string            `json:"status,omitempty"`
	Kind     string            `json:"kind,omitempty"`
	Peer     map[string]string `json:"peer,omitempty"`
	// Exception is what the application threw, when it recorded one.
	//
	// Carried as its own field rather than left in the attribute map because
	// it is the only part of a span the UI groups BY rather than displays: a
	// list of exceptions is a list of types with counts, and a type buried in
	// a map every consumer has to know to look in is one nobody looks in.
	Exception *SpanException `json:"exception,omitempty"`
}

// SpanException is a thrown error, as OTel's conventions record it.
//
// Stacktrace is frequently absent — plenty of SDKs record a type and a message
// and nothing else — so a consumer must not treat its absence as an absence of
// the exception.
type SpanException struct {
	Type       string `json:"type"`
	Message    string `json:"message,omitempty"`
	Stacktrace string `json:"stacktrace,omitempty"`
	// Count is set only when one span recorded more than one exception, in
	// which case Type and Message describe the first — usually the cause,
	// with the rest being wrappers that re-threw it.
	Count int `json:"count,omitempty"`
}

// Snapshot is one host's telemetry over a window.
type Snapshot struct {
	AgentID         string            `json:"agent_id"`
	Version         string            `json:"version"`
	AdapterContract string            `json:"adapter_contract"`
	Now             int64             `json:"now"`
	RetainSec       int               `json:"retain_sec"`
	Counts          map[string]uint64 `json:"counts"`
	Series          []Series          `json:"series"`
	// SeriesDropped counts series the cap refused. The agent's own dashboard
	// has reported this since it gained a cap, and the frontend already reads
	// it — the backend simply never set it, so a truncated view looked
	// complete.
	SeriesDropped uint64            `json:"series_dropped"`
	Logs          []LogLine         `json:"logs"`
	Spans         []Span            `json:"spans"`
	Host          map[string]string `json:"host,omitempty"`
	// Source marks where this came from, so the UI can say so rather than
	// implying a database read is a live agent. The agent never sets it.
	Source string `json:"source,omitempty"`
}

// Caps on what one snapshot carries.
//
// The agent's own limits, for the same reason it has them: these payloads are
// read by a browser at a polling interval, and a host with a busy log is
// otherwise capable of producing a response large enough that rendering it is
// slower than collecting it. Bounded here rather than in the query alone so a
// wide window degrades in resolution rather than in responsiveness.
const (
	maxSnapshotLogs   = 300
	maxSnapshotSpans  = 400
	maxSeriesPoints   = 240
	snapshotContract  = "1"
	minSnapshotWindow = time.Minute
	// defaultMaxSeriesPerHost is the starting cap, overridable through
	// Config.MaxSeriesPerHost. Four hundred series at the point limit is about
	// a megabyte of JSON, which is as much as a browser should be asked to
	// parse on a poll interval — but that is a judgement about the reader, not
	// a fact about the host, so it is a default and not a law.
	defaultMaxSeriesPerHost = 400
	// maxSeriesScanned bounds what is assembled before the cap is applied.
	// Selecting fairly across metric names requires seeing them all, so this is
	// deliberately far above the per-host cap — it is a guard against a
	// pathological host, not the cap itself. At the point limit it costs about
	// 5,000 x 240 points, which is large but bounded and never leaves this
	// process.
	maxSeriesScanned = 5000
)

// Snapshot assembles one host's series, logs and spans.
//
// Three queries rather than one join: they read different tables with
// different ordering keys and nothing correlates a log with a metric except
// the host they share. A join would force ClickHouse to scan all three in one
// plan for no result a caller can use.
func (c *Client) Snapshot(ctx context.Context, hostID string, window time.Duration) (*Snapshot, error) {
	if hostID == "" {
		return nil, fmt.Errorf("snapshot: host is required")
	}
	if window < minSnapshotWindow {
		window = 15 * time.Minute
	}
	now := time.Now().UTC()
	snap := &Snapshot{
		AdapterContract: snapshotContract,
		Now:             now.UnixMilli(),
		RetainSec:       int(window.Seconds()),
		Counts:          map[string]uint64{},
		Series:          []Series{},
		Logs:            []LogLine{},
		Spans:           []Span{},
		Source:          "backend",
	}

	// Identity first. A host with no rows in any signal table still answers,
	// with its description and empty series — which is a different thing from
	// an error, and the difference is what tells you the host is quiet rather
	// than that the query failed.
	hosts, err := c.Hosts(ctx, window)
	if err != nil {
		return nil, err
	}
	for _, h := range hosts {
		if h.HostID == hostID {
			snap.AgentID = h.AgentID
			snap.Host = h.Attributes
			snap.Version = h.Attributes["service.version"]
			break
		}
	}
	if snap.AgentID == "" {
		snap.AgentID = hostID
	}

	if snap.Series, snap.SeriesDropped, err = c.hostSeries(ctx, hostID, window); err != nil {
		return nil, err
	}
	if snap.Logs, err = c.hostLogs(ctx, hostID, window); err != nil {
		return nil, err
	}
	if snap.Spans, err = c.hostSpans(ctx, hostID, window); err != nil {
		return nil, err
	}

	snap.Counts["series"] = uint64(len(snap.Series))
	snap.Counts["logs"] = uint64(len(snap.Logs))
	snap.Counts["spans"] = uint64(len(snap.Spans))
	return snap, nil
}

// hostSeries returns every metric the host reported, bucketed.
//
// Bucketed in the database, at a step derived from the window, so a one-hour
// view and a one-day view cost the browser the same. Sending raw points and
// averaging them client-side would mean transferring a day of 15-second
// samples to draw two hundred pixels.
//
// Grouped by name AND label set, because that is what a series is:
// system.disk.io for two devices is two lines, and summing them into one would
// hide the device that is saturated behind the one that is idle.
func (c *Client) hostSeries(ctx context.Context, hostID string, window time.Duration) ([]Series, uint64, error) {
	step := window / maxSeriesPoints
	if step < time.Second {
		step = time.Second
	}
	const q = `
SELECT
    name                                                    AS name,
    -- The label set, minus the resource attributes that are identical on
    -- every row for this host. Keeping those would make each series carry the
    -- machine's whole description, and would split nothing: a label that never
    -- varies cannot distinguish one series from another.
    arraySort(arrayFilter(
        (k) -> NOT startsWith(k, 'host.')
           AND NOT startsWith(k, 'os.')
           AND NOT startsWith(k, 'cloud.')
           AND k NOT IN ('service.name', 'service.version', 'unit'),
        mapKeys(attributes)))                               AS label_keys,
    arrayMap((k) -> attributes[k], label_keys)              AS label_values,
    max(is_monotonic)                                       AS cumulative,
    toUnixTimestamp64Milli(toDateTime64(
        toStartOfInterval(timestamp, INTERVAL {step:UInt32} SECOND), 3)) AS t,
    avg(value)                                              AS v
FROM metrics
WHERE host_id = {host:String}
  AND timestamp >= now() - INTERVAL {window:UInt32} SECOND
GROUP BY name, label_keys, label_values, t
ORDER BY name, label_values, t`

	var rows []struct {
		Name        string   `json:"name"`
		LabelKeys   []string `json:"label_keys"`
		LabelValues []string `json:"label_values"`
		Cumulative  uint8    `json:"cumulative"`
		T           int64    `json:"t,string"`
		V           float64  `json:"v"`
	}
	params := map[string]string{
		"host":   hostID,
		"window": strconv.Itoa(int(window.Seconds())),
		"step":   strconv.Itoa(int(step.Seconds())),
	}
	if err := c.Query(ctx, q, params, &rows); err != nil {
		return nil, 0, fmt.Errorf("series for %s: %w", hostID, err)
	}

	// Rows arrive ordered by series then time, so points accumulate onto
	// whichever series is current rather than needing a second pass.
	all := make([]Series, 0, 32)
	index := map[string]int{}
	for _, r := range rows {
		labels := map[string]string{}
		for i, k := range r.LabelKeys {
			if i < len(r.LabelValues) {
				labels[k] = r.LabelValues[i]
			}
		}
		key := r.Name + "\x00" + joinLabels(r.LabelKeys, r.LabelValues)
		i, ok := index[key]
		if !ok {
			if len(all) >= maxSeriesScanned {
				continue
			}
			all = append(all, Series{
				Name: r.Name, Labels: labels, Cumulative: r.Cumulative == 1,
				Points: make([]Point, 0, maxSeriesPoints),
			})
			i = len(all) - 1
			index[key] = i
		}
		all[i].Points = append(all[i].Points, Point{T: r.T, V: r.V})
	}
	return capSeries(all, c.maxSeriesPerHost)
}

// capSeries reduces a host's series to at most max, taking from every metric
// name in turn.
//
// The naive cap — keep the first max as they arrive — is what this replaces,
// and it failed in a way that looked like a collection bug. Rows arrive ordered
// by name, so the cap fell wherever the alphabet put it: on one real host,
// system.network.dropped and system.network.errors were admitted and
// system.network.io and system.network.packets were refused entirely, because
// "d" and "e" sort before "i" and "p". Two whole metric families vanished, the
// dashboard rendered their panels as "not wired", and four thousand points sat
// in the database that nothing would ever ask for again.
//
// Round-robin makes the loss proportional instead of alphabetical. Every metric
// name keeps representation, so a panel degrades to fewer devices rather than
// going dark — and the count of what was refused is returned, because a view
// that is quietly partial is the failure this whole function exists to avoid.
func capSeries(all []Series, max int) ([]Series, uint64, error) {
	if len(all) <= max {
		return all, 0, nil
	}

	// Grouped by name, preserving the order names were first seen so the
	// output is deterministic for a given query result.
	order := make([]string, 0, 16)
	byName := make(map[string][]Series, 16)
	for _, s := range all {
		if _, seen := byName[s.Name]; !seen {
			order = append(order, s.Name)
		}
		byName[s.Name] = append(byName[s.Name], s)
	}

	out := make([]Series, 0, max)
	for round := 0; len(out) < max; round++ {
		progressed := false
		for _, name := range order {
			group := byName[name]
			if round >= len(group) {
				continue
			}
			progressed = true
			out = append(out, group[round])
			if len(out) >= max {
				break
			}
		}
		// Every group is exhausted. Cannot happen while len(all) > max, but a
		// loop whose termination depends on arithmetic elsewhere is a loop that
		// hangs the query the day that arithmetic changes.
		if !progressed {
			break
		}
	}
	return out, uint64(len(all) - len(out)), nil
}

func joinLabels(keys, values []string) string {
	s := ""
	for i, k := range keys {
		s += k + "="
		if i < len(values) {
			s += values[i]
		}
		s += ","
	}
	return s
}

// hostLogs returns the most recent log lines.
//
// Newest first from the database — that is the ordering the index supports and
// the only way a LIMIT means "the latest" rather than "the oldest" — then
// reversed, because the agent sends oldest-first and the log view scrolls that
// way. Reversing a bounded slice is free; asking the database for the oldest N
// of a descending index is not.
func (c *Client) hostLogs(ctx context.Context, hostID string, window time.Duration) ([]LogLine, error) {
	const q = `
SELECT
    toUnixTimestamp64Milli(timestamp)   AS t,
    service                             AS service,
    severity                            AS severity,
    body                                AS body,
    trace_id                            AS trace_id,
    attributes                          AS attrs
FROM logs
WHERE host_id = {host:String}
  AND timestamp >= now() - INTERVAL {window:UInt32} SECOND
ORDER BY timestamp DESC
LIMIT {limit:UInt32}`

	var rows []struct {
		T        int64             `json:"t,string"`
		Service  string            `json:"service"`
		Severity string            `json:"severity"`
		Body     string            `json:"body"`
		TraceID  string            `json:"trace_id"`
		Attrs    map[string]string `json:"attrs"`
	}
	params := map[string]string{
		"host":   hostID,
		"window": strconv.Itoa(int(window.Seconds())),
		"limit":  strconv.Itoa(maxSnapshotLogs),
	}
	if err := c.Query(ctx, q, params, &rows); err != nil {
		return nil, fmt.Errorf("logs for %s: %w", hostID, err)
	}

	out := make([]LogLine, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		labels := perRecordAttrs(r.Attrs)
		// The dashboard reads severity from a label, which is where the agent
		// puts it. Stored as its own column here because it is filtered on;
		// it moves back into the label set on the way out so one log view can
		// read both sources.
		if r.Severity != "" {
			labels["level"] = r.Severity
		}
		if r.Service != "" {
			labels["service"] = r.Service
		}
		if r.TraceID != "" {
			labels["trace_id"] = r.TraceID
		}
		out = append(out, LogLine{
			T: r.T, Source: firstNonEmptyStr(r.Attrs["source"], r.Service, "otlp"),
			Message: r.Body, Labels: labels,
		})
	}
	return out, nil
}

// maxLogLabels bounds how many attributes one log line carries out of the
// store. A producer is free to attach as many as it likes; this response is
// rendered by a browser at a polling interval, and one pathological service
// must not be able to make the snapshot expensive for every other.
const maxLogLabels = 24

// resourcePrefixes are the attribute namespaces that describe the HOST rather
// than the record.
//
// Every log line from a machine carries an identical copy of them — twenty-odd
// keys naming the instance, its image, its region and its OS — and the snapshot
// already reports all of that once, in its own Host block. Returning them again
// on each of three hundred log lines would be several thousand repeated strings
// per poll, describing something the client already knows.
//
// Stripping by namespace rather than by an allowlist of what to keep is the
// point. The bug this replaces was a query that selected one hardcoded
// attribute and discarded the rest, so container.name, container.id and stream
// were stored and then thrown away on the way out. An allowlist would have
// reproduced that failure the next time a collector started emitting something
// new. This keeps everything by default and removes only what is provably
// redundant.
var resourcePrefixes = []string{"host.", "cloud.", "os.", "telemetry."}

// perRecordAttrs returns the attributes that describe this record.
func perRecordAttrs(attrs map[string]string) map[string]string {
	out := make(map[string]string, len(attrs))
	for k, v := range attrs {
		if v == "" || isResourceAttr(k) {
			continue
		}
		if len(out) >= maxLogLabels {
			break
		}
		out[k] = v
	}
	return out
}

func isResourceAttr(k string) bool {
	// service.name is the one resource attribute without a namespace prefix.
	// It is already returned as the record's own service field.
	if k == "service.name" {
		return true
	}
	for _, p := range resourcePrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// hostSpans returns the most recent spans.
//
// Whole traces are what the waterfall needs, and a plain LIMIT cuts across
// them: it would return the newest 400 spans, which is the tail of a trace
// whose root fell outside the limit and cannot be drawn. So the newest traces
// are selected first, and then every span belonging to them is fetched.
func (c *Client) hostSpans(ctx context.Context, hostID string, window time.Duration) ([]Span, error) {
	const q = `
WITH recent_traces AS (
    SELECT trace_id
    FROM spans
    WHERE host_id = {host:String}
      AND timestamp >= now() - INTERVAL {window:UInt32} SECOND
    GROUP BY trace_id
    ORDER BY max(timestamp) DESC
    LIMIT {traces:UInt32}
)
SELECT
    toUnixTimestamp64Milli(timestamp)   AS t,
    trace_id                            AS trace_id,
    span_id                             AS span_id,
    parent_span_id                      AS parent_id,
    service                             AS service,
    name                                AS name,
    duration_ns / 1000000               AS dur_ms,
    status_code                         AS status,
    kind                                AS kind,
    attributes                          AS attributes
FROM spans
WHERE host_id = {host:String}
  AND timestamp >= now() - INTERVAL {window:UInt32} SECOND
  AND trace_id IN (SELECT trace_id FROM recent_traces)
ORDER BY timestamp
LIMIT {limit:UInt32}`

	var rows []struct {
		T          int64             `json:"t,string"`
		TraceID    string            `json:"trace_id"`
		SpanID     string            `json:"span_id"`
		ParentID   string            `json:"parent_id"`
		Service    string            `json:"service"`
		Name       string            `json:"name"`
		DurMs      float64           `json:"dur_ms"`
		Status     string            `json:"status"`
		Kind       string            `json:"kind"`
		Attributes map[string]string `json:"attributes"`
	}
	params := map[string]string{
		"host":   hostID,
		"window": strconv.Itoa(int(window.Seconds())),
		"traces": strconv.Itoa(maxSnapshotSpans / 4),
		"limit":  strconv.Itoa(maxSnapshotSpans),
	}
	if err := c.Query(ctx, q, params, &rows); err != nil {
		return nil, fmt.Errorf("spans for %s: %w", hostID, err)
	}

	out := make([]Span, 0, len(rows))
	for _, r := range rows {
		s := Span{
			T: r.T, TraceID: r.TraceID, SpanID: r.SpanID, ParentID: r.ParentID,
			Service: r.Service, Name: r.Name, DurMs: r.DurMs,
			Kind: r.Kind,
		}
		// The dashboard reads "2" or "ERROR" as an error, matching what the
		// agent sends. Only an explicit error is reported: "unset" is not a
		// success and must not be rendered as one, and mapping it to OK would
		// make an error rate depend on how thoroughly an application annotates
		// its spans.
		if r.Status == "error" {
			s.Status = "ERROR"
		}
		if peer := peerAttrs(r.Attributes); len(peer) > 0 {
			s.Peer = peer
		}
		s.Exception = spanException(r.Attributes)
		out = append(out, s)
	}
	return out, nil
}

// spanException reads a thrown error out of a span's attributes.
//
// The agent normalises OTel's exception EVENT into these attributes before
// export, because a span row holds one attribute map and no event list. That
// flattening is why this lives here rather than in a span-events table, and
// why exception.type is the key it hinges on: a record with no type cannot be
// grouped, and an ungroupable exception is one the view cannot show.
func spanException(attrs map[string]string) *SpanException {
	if len(attrs) == 0 {
		return nil
	}
	typ := attrs["exception.type"]
	if typ == "" {
		return nil
	}
	e := &SpanException{
		Type:       typ,
		Message:    attrs["exception.message"],
		Stacktrace: attrs["exception.stacktrace"],
	}
	if n, err := strconv.Atoi(attrs["exception.count"]); err == nil && n > 1 {
		e.Count = n
	}
	return e
}

// peerAttrs picks out the attributes naming what an outbound span was talking
// to. Without them a service's databases, queues and third-party APIs are
// absent from the service map entirely, because an uninstrumented peer has no
// span of its own anywhere in the trace.
func peerAttrs(attrs map[string]string) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	wanted := []string{
		"peer.service",
		"db.system", "db.name", "db.operation",
		"messaging.system", "messaging.destination", "messaging.destination.name",
		"rpc.service", "rpc.system",
		"server.address", "net.peer.name",
		"http.host", "url.full",
	}
	out := map[string]string{}
	for _, k := range wanted {
		if v := attrs[k]; v != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// SortSeries orders series by name then label set, so a repeated poll returns
// them in the same order and the UI's list does not reshuffle under the
// pointer between refreshes.
func SortSeries(s []Series) {
	sort.SliceStable(s, func(i, j int) bool {
		if s[i].Name != s[j].Name {
			return s[i].Name < s[j].Name
		}
		return fmt.Sprint(s[i].Labels) < fmt.Sprint(s[j].Labels)
	})
}
