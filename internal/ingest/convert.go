// Package ingest turns OTLP export requests into the rows the store persists.
//
// It is deliberately separate from the HTTP handling around it. Everything
// here is a pure function from a decoded OTLP request to a slice of rows,
// which is what makes the mapping — the part that is easy to get subtly wrong
// and impossible to notice — testable without a server or a database.
package ingest

import (
	"encoding/hex"
	"strings"
	"time"

	"github.com/agent-i/agent/internal/otlpwire"
)

// Row is one record destined for one table.
type Row map[string]any

// Batch is what a single export request produced, grouped by table so the
// store can write each with one round trip.
type Batch struct {
	Metrics []Row
	Logs    []Row
	Spans   []Row
	// Hosts carries the inventory upsert for every distinct host seen in the
	// request. Derived from the same resource attributes rather than from a
	// separate registration call: an agent that is sending data has by
	// definition announced itself, and a registration step is another thing
	// that can be skipped, fail, or go stale.
	Hosts []Row
}

func (b Batch) Empty() bool {
	return len(b.Metrics) == 0 && len(b.Logs) == 0 && len(b.Spans) == 0 && len(b.Hosts) == 0
}

// Resource attributes the backend promotes to their own columns. Everything
// else stays in the attributes map.
const (
	attrHostID      = "host.id"
	attrHostName    = "host.name"
	attrServiceName = "service.name"
)

// resourceView is the part of a resource the row builders need, resolved once
// per ResourceMetrics/Logs/Spans rather than per data point.
type resourceView struct {
	hostID  string
	agentID string
	service string
	attrs   map[string]string
}

// readResource flattens resource attributes and picks out the identity fields.
//
// hostID falls back through host.id, then host.name, then service.name. The
// fallback matters: host.id is the EC2 instance id and is absent off a cloud
// host, and a row with an empty host_id would be unattributable — it would
// land in the fleet table as a nameless machine that cannot be filtered,
// queried or told apart from any other unidentified sender.
func readResource(res *otlpwire.Resource) resourceView {
	v := resourceView{attrs: map[string]string{}}
	if res == nil {
		return v
	}
	for _, kv := range res.Attributes {
		if kv == nil || kv.Key == "" {
			continue
		}
		val := kv.Value.String()
		if val == "" {
			continue
		}
		v.attrs[kv.Key] = val
	}
	v.hostID = firstNonEmpty(v.attrs[attrHostID], v.attrs[attrHostName], v.attrs[attrServiceName])
	v.agentID = firstNonEmpty(v.attrs[attrHostName], v.attrs[attrHostID])
	v.service = v.attrs[attrServiceName]
	return v
}

// hostRow is the inventory upsert for a resource.
//
// first_seen is set to the same instant as last_seen on every write. The
// ReplacingMergeTree keeps the row with the greatest last_seen, so a later
// write would overwrite an earlier first_seen and the column would always
// equal last_seen — which is why the query side computes the real first_seen
// from the data instead of trusting this. It is written anyway so the column
// is never null for a host that has only ever reported once.
func (v resourceView) hostRow(now time.Time) Row {
	if v.hostID == "" {
		return nil
	}
	ts := formatMillis(now)
	return Row{
		"host_id":    v.hostID,
		"agent_id":   v.agentID,
		"last_seen":  ts,
		"first_seen": ts,
		"attributes": v.attrs,
	}
}

// Metrics converts an OTLP metric export.
func Metrics(req *otlpwire.ExportMetricsServiceRequest, now time.Time) Batch {
	var b Batch
	if req == nil {
		return b
	}
	seen := map[string]bool{}

	for _, rm := range req.ResourceMetrics {
		if rm == nil {
			continue
		}
		res := readResource(rm.Resource)
		addHost(&b, res, now, seen)

		for _, sm := range rm.ScopeMetrics {
			if sm == nil {
				continue
			}
			for _, m := range sm.Metrics {
				if m == nil || m.Name == "" {
					continue
				}
				for _, p := range m.NumberPoints {
					if p == nil {
						continue
					}
					b.Metrics = append(b.Metrics, Row{
						"timestamp":    formatNanos(p.TimeUnixNano, now),
						"name":         m.Name,
						"host_id":      res.hostID,
						"service":      res.service,
						"value":        p.Value,
						"is_monotonic": boolToUint8(m.IsMonotonic),
						"attributes":   mergeAttrs(res.attrs, p.Attributes, m.Unit),
					})
				}
				// A histogram becomes two series, .count and .sum, rather than
				// one row carrying both. The store has a single Float64 value
				// column, and the alternative — a nullable second column used
				// only by histograms — would be a column that is empty on
				// every other row in the table.
				for _, p := range m.HistogramPoints {
					if p == nil {
						continue
					}
					attrs := mergeAttrs(res.attrs, p.Attributes, m.Unit)
					ts := formatNanos(p.TimeUnixNano, now)
					b.Metrics = append(b.Metrics, Row{
						"timestamp": ts, "name": m.Name + ".count", "host_id": res.hostID,
						"service": res.service, "value": float64(p.Count),
						"is_monotonic": uint8(1), "attributes": attrs,
					})
					if p.HasSum {
						b.Metrics = append(b.Metrics, Row{
							"timestamp": ts, "name": m.Name + ".sum", "host_id": res.hostID,
							"service": res.service, "value": p.Sum,
							"is_monotonic": uint8(1), "attributes": attrs,
						})
					}
				}
			}
		}
	}
	return b
}

// Logs converts an OTLP log export.
func Logs(req *otlpwire.ExportLogsServiceRequest, now time.Time) Batch {
	var b Batch
	if req == nil {
		return b
	}
	seen := map[string]bool{}

	for _, rl := range req.ResourceLogs {
		if rl == nil {
			continue
		}
		res := readResource(rl.Resource)
		addHost(&b, res, now, seen)

		for _, sl := range rl.ScopeLogs {
			if sl == nil {
				continue
			}
			for _, rec := range sl.LogRecords {
				if rec == nil {
					continue
				}
				b.Logs = append(b.Logs, Row{
					"timestamp": formatNanos(rec.TimeUnixNano, now),
					"host_id":   res.hostID,
					"service":   res.service,
					// The text is free-form and frequently absent; the number
					// is well defined. Deriving the name from the number when
					// the text is missing keeps the severity column usable as
					// a filter rather than half empty.
					"severity":     firstNonEmpty(rec.SeverityText, severityName(rec.SeverityNumber)),
					"severity_num": clampSeverity(rec.SeverityNumber),
					"body":         rec.Body.String(),
					"trace_id":     hex.EncodeToString(rec.TraceID),
					"span_id":      hex.EncodeToString(rec.SpanID),
					"attributes":   mergeAttrs(res.attrs, rec.Attributes, ""),
				})
			}
		}
	}
	return b
}

// Spans converts an OTLP trace export.
func Spans(req *otlpwire.ExportTraceServiceRequest, now time.Time) Batch {
	var b Batch
	if req == nil {
		return b
	}
	seen := map[string]bool{}

	for _, rs := range req.ResourceSpans {
		if rs == nil {
			continue
		}
		res := readResource(rs.Resource)
		addHost(&b, res, now, seen)

		for _, ss := range rs.ScopeSpans {
			if ss == nil {
				continue
			}
			for _, sp := range ss.Spans {
				if sp == nil {
					continue
				}
				// End before start happens: clocks step, and a span whose
				// duration underflowed to a huge unsigned number would poison
				// every latency percentile it landed in.
				var duration uint64
				if sp.EndTimeUnixNano > sp.StartTimeUnixNano {
					duration = sp.EndTimeUnixNano - sp.StartTimeUnixNano
				}
				b.Spans = append(b.Spans, Row{
					"timestamp":      formatNanos(sp.StartTimeUnixNano, now),
					"trace_id":       hex.EncodeToString(sp.TraceID),
					"span_id":        hex.EncodeToString(sp.SpanID),
					"parent_span_id": hex.EncodeToString(sp.ParentSpanID),
					"service":        res.service,
					"name":           sp.Name,
					"kind":           spanKindName(sp.Kind),
					"duration_ns":    duration,
					"status_code":    statusCodeName(sp.Status),
					"status_message": statusMessage(sp.Status),
					"host_id":        res.hostID,
					"attributes":     mergeAttrs(res.attrs, sp.Attributes, ""),
				})
			}
		}
	}
	return b
}

// --- helpers ---

func addHost(b *Batch, res resourceView, now time.Time, seen map[string]bool) {
	if res.hostID == "" || seen[res.hostID] {
		return
	}
	seen[res.hostID] = true
	if row := res.hostRow(now); row != nil {
		b.Hosts = append(b.Hosts, row)
	}
}

// mergeAttrs combines resource and point attributes.
//
// Resource attributes are included on every row rather than only in the hosts
// table, because a query filtering by cloud.account.id or os.name must not
// have to join to find them — and at ClickHouse's compression, a
// LowCardinality map key that repeats on every row of a partition costs very
// little. Point attributes win on conflict: they are the more specific
// statement about that particular measurement.
func mergeAttrs(resource map[string]string, point []*otlpwire.KeyValue, unit string) map[string]string {
	out := make(map[string]string, len(resource)+len(point)+1)
	for k, v := range resource {
		out[k] = v
	}
	for _, kv := range point {
		if kv == nil || kv.Key == "" {
			continue
		}
		if v := kv.Value.String(); v != "" {
			out[kv.Key] = v
		}
	}
	if unit != "" {
		out["unit"] = unit
	}
	return out
}

// formatNanos renders an OTLP nanosecond timestamp for ClickHouse.
//
// A zero timestamp becomes the receive time. OTLP permits it, some SDKs send
// it, and storing 1970 would put the row outside every query's time range and
// in a partition of its own — data that is present, unfindable, and never
// aged out because its TTL expired decades ago.
func formatNanos(ns uint64, now time.Time) string {
	if ns == 0 {
		return formatNanosTime(now)
	}
	return formatNanosTime(time.Unix(0, int64(ns)).UTC())
}

func formatNanosTime(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05.000000000") }
func formatMillis(t time.Time) string    { return t.UTC().Format("2006-01-02 15:04:05.000") }

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

func boolToUint8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

// clampSeverity keeps the value inside the UInt8 column and inside OTLP's
// defined 1..24 range, so a malformed export cannot make the column
// unsortable.
func clampSeverity(n int32) uint8 {
	if n < 0 {
		return 0
	}
	if n > 24 {
		return 24
	}
	return uint8(n)
}

// severityName maps OTLP's numeric severity onto its band names, which is what
// the spec defines the ranges to mean.
func severityName(n int32) string {
	switch {
	case n <= 0:
		return ""
	case n <= 4:
		return "TRACE"
	case n <= 8:
		return "DEBUG"
	case n <= 12:
		return "INFO"
	case n <= 16:
		return "WARN"
	case n <= 20:
		return "ERROR"
	default:
		return "FATAL"
	}
}

func spanKindName(kind int32) string {
	switch kind {
	case 1:
		return "internal"
	case 2:
		return "server"
	case 3:
		return "client"
	case 4:
		return "producer"
	case 5:
		return "consumer"
	default:
		return "unspecified"
	}
}

// statusCodeName renders OTLP's status. Unset is distinct from Ok: a span
// nobody set a status on is not the same as one explicitly marked successful,
// and collapsing them would make an error rate depend on how thoroughly an
// application annotates its spans.
func statusCodeName(s *otlpwire.Status) string {
	if s == nil {
		return "unset"
	}
	switch s.Code {
	case 1:
		return "ok"
	case 2:
		return "error"
	default:
		return "unset"
	}
}

func statusMessage(s *otlpwire.Status) string {
	if s == nil {
		return ""
	}
	return s.Message
}
