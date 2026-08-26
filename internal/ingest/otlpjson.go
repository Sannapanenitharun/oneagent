package ingest

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/agent-i/agent/internal/otlpwire"
)

// OTLP/JSON decoding.
//
// OTLP defines two encodings over HTTP and both are in use here: this
// project's own agent exports JSON, while most third-party SDKs and
// collectors default to protobuf. Rather than two conversion paths, JSON is
// decoded into the same otlpwire types the protobuf decoder produces, so
// everything downstream — the row builders, their tests, the schema — sees one
// representation and cannot drift between encodings.
//
// The protobuf JSON mapping has two traps that account for most of the code
// below. 64-bit integers are encoded as STRINGS, because JSON numbers cannot
// carry them without loss, so timeUnixNano arrives as "1786800000000000000"
// and unmarshalling it into a uint64 fails. And trace and span ids are hex
// strings rather than byte arrays. Both are easy to get wrong in a way that
// silently produces zero values instead of an error.

type jsonAnyValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
	IntValue    *string  `json:"intValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
}

func (v *jsonAnyValue) toWire() *otlpwire.AnyValue {
	if v == nil {
		return nil
	}
	switch {
	case v.StringValue != nil:
		return &otlpwire.AnyValue{Kind: otlpwire.ValueString, Str: *v.StringValue}
	case v.BoolValue != nil:
		return &otlpwire.AnyValue{Kind: otlpwire.ValueBool, Bool: *v.BoolValue}
	case v.IntValue != nil:
		n, _ := strconv.ParseInt(*v.IntValue, 10, 64)
		return &otlpwire.AnyValue{Kind: otlpwire.ValueInt, Int: n}
	case v.DoubleValue != nil:
		return &otlpwire.AnyValue{Kind: otlpwire.ValueDouble, Double: *v.DoubleValue}
	}
	return nil
}

type jsonKeyValue struct {
	Key   string        `json:"key"`
	Value *jsonAnyValue `json:"value"`
}

func toWireAttrs(in []jsonKeyValue) []*otlpwire.KeyValue {
	if len(in) == 0 {
		return nil
	}
	out := make([]*otlpwire.KeyValue, 0, len(in))
	for _, kv := range in {
		if kv.Key == "" {
			continue
		}
		out = append(out, &otlpwire.KeyValue{Key: kv.Key, Value: kv.Value.toWire()})
	}
	return out
}

type jsonResource struct {
	Attributes []jsonKeyValue `json:"attributes"`
}

func (r *jsonResource) toWire() *otlpwire.Resource {
	if r == nil {
		return nil
	}
	return &otlpwire.Resource{Attributes: toWireAttrs(r.Attributes)}
}

// parseUnixNano reads the string-encoded uint64 the protobuf JSON mapping
// requires. A number is accepted too, since some producers emit one despite
// the spec and rejecting them would mean rejecting real telemetry over a
// formatting detail.
func parseUnixNano(raw json.RawMessage) uint64 {
	if len(raw) == 0 {
		return 0
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		n, _ := strconv.ParseUint(s, 10, 64)
		return n
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil && f > 0 {
		return uint64(f)
	}
	return 0
}

// --- metrics ---

type jsonNumberDataPoint struct {
	TimeUnixNano json.RawMessage `json:"timeUnixNano"`
	AsDouble     *float64        `json:"asDouble,omitempty"`
	AsInt        *string         `json:"asInt,omitempty"`
	Attributes   []jsonKeyValue  `json:"attributes,omitempty"`
}

func (p jsonNumberDataPoint) value() float64 {
	switch {
	case p.AsDouble != nil:
		return *p.AsDouble
	case p.AsInt != nil:
		n, _ := strconv.ParseInt(*p.AsInt, 10, 64)
		return float64(n)
	}
	return 0
}

type jsonHistogramDataPoint struct {
	TimeUnixNano json.RawMessage `json:"timeUnixNano"`
	Count        json.RawMessage `json:"count"`
	Sum          *float64        `json:"sum,omitempty"`
	Attributes   []jsonKeyValue  `json:"attributes,omitempty"`
}

type jsonMetric struct {
	Name  string `json:"name"`
	Unit  string `json:"unit,omitempty"`
	Gauge *struct {
		DataPoints []jsonNumberDataPoint `json:"dataPoints"`
	} `json:"gauge,omitempty"`
	Sum *struct {
		DataPoints  []jsonNumberDataPoint `json:"dataPoints"`
		IsMonotonic bool                  `json:"isMonotonic"`
	} `json:"sum,omitempty"`
	Histogram *struct {
		DataPoints []jsonHistogramDataPoint `json:"dataPoints"`
	} `json:"histogram,omitempty"`
	ExponentialHistogram *struct {
		DataPoints []jsonHistogramDataPoint `json:"dataPoints"`
	} `json:"exponentialHistogram,omitempty"`
}

type jsonMetricsRequest struct {
	ResourceMetrics []struct {
		Resource     *jsonResource `json:"resource"`
		ScopeMetrics []struct {
			Metrics []jsonMetric `json:"metrics"`
		} `json:"scopeMetrics"`
	} `json:"resourceMetrics"`
}

// UnmarshalJSONMetrics decodes an OTLP/JSON metric export.
func UnmarshalJSONMetrics(body []byte) (*otlpwire.ExportMetricsServiceRequest, error) {
	var req jsonMetricsRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("otlp/json metrics: %w", err)
	}
	out := &otlpwire.ExportMetricsServiceRequest{}
	for _, rm := range req.ResourceMetrics {
		wrm := &otlpwire.ResourceMetrics{Resource: rm.Resource.toWire()}
		for _, sm := range rm.ScopeMetrics {
			wsm := &otlpwire.ScopeMetrics{}
			for _, m := range sm.Metrics {
				wm := &otlpwire.Metric{Name: m.Name, Unit: m.Unit}
				switch {
				case m.Gauge != nil:
					wm.Kind = otlpwire.MetricGauge
					wm.NumberPoints = toWireNumbers(m.Gauge.DataPoints)
				case m.Sum != nil:
					wm.Kind = otlpwire.MetricSum
					wm.IsMonotonic = m.Sum.IsMonotonic
					wm.NumberPoints = toWireNumbers(m.Sum.DataPoints)
				case m.Histogram != nil:
					wm.Kind = otlpwire.MetricHistogram
					wm.HistogramPoints = toWireHistograms(m.Histogram.DataPoints)
				case m.ExponentialHistogram != nil:
					// Only count and sum are taken. Reconstructing buckets
					// would mean a bucket-aware path through the whole store,
					// and inventing quantiles from them here would be worse
					// than not offering them — the same call the agent's own
					// decoder makes.
					wm.Kind = otlpwire.MetricHistogram
					wm.HistogramPoints = toWireHistograms(m.ExponentialHistogram.DataPoints)
				default:
					continue // a metric with no data points carries nothing
				}
				wsm.Metrics = append(wsm.Metrics, wm)
			}
			wrm.ScopeMetrics = append(wrm.ScopeMetrics, wsm)
		}
		out.ResourceMetrics = append(out.ResourceMetrics, wrm)
	}
	return out, nil
}

func toWireNumbers(in []jsonNumberDataPoint) []*otlpwire.NumberDataPoint {
	out := make([]*otlpwire.NumberDataPoint, 0, len(in))
	for _, p := range in {
		out = append(out, &otlpwire.NumberDataPoint{
			TimeUnixNano: parseUnixNano(p.TimeUnixNano),
			Value:        p.value(),
			Attributes:   toWireAttrs(p.Attributes),
		})
	}
	return out
}

func toWireHistograms(in []jsonHistogramDataPoint) []*otlpwire.HistogramDataPoint {
	out := make([]*otlpwire.HistogramDataPoint, 0, len(in))
	for _, p := range in {
		hp := &otlpwire.HistogramDataPoint{
			TimeUnixNano: parseUnixNano(p.TimeUnixNano),
			Count:        parseUnixNano(p.Count), // same string-encoded uint64 rule
			Attributes:   toWireAttrs(p.Attributes),
		}
		if p.Sum != nil {
			hp.Sum, hp.HasSum = *p.Sum, true
		}
		out = append(out, hp)
	}
	return out
}

// --- traces ---

type jsonSpan struct {
	TraceID           string          `json:"traceId"`
	SpanID            string          `json:"spanId"`
	ParentSpanID      string          `json:"parentSpanId,omitempty"`
	Name              string          `json:"name"`
	Kind              int32           `json:"kind,omitempty"`
	StartTimeUnixNano json.RawMessage `json:"startTimeUnixNano"`
	EndTimeUnixNano   json.RawMessage `json:"endTimeUnixNano"`
	Attributes        []jsonKeyValue  `json:"attributes,omitempty"`
	Status            *struct {
		Message string `json:"message,omitempty"`
		Code    int32  `json:"code,omitempty"`
	} `json:"status,omitempty"`
}

type jsonTracesRequest struct {
	ResourceSpans []struct {
		Resource   *jsonResource `json:"resource"`
		ScopeSpans []struct {
			Spans []jsonSpan `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"resourceSpans"`
}

// UnmarshalJSONTraces decodes an OTLP/JSON trace export.
func UnmarshalJSONTraces(body []byte) (*otlpwire.ExportTraceServiceRequest, error) {
	var req jsonTracesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("otlp/json traces: %w", err)
	}
	out := &otlpwire.ExportTraceServiceRequest{}
	for _, rs := range req.ResourceSpans {
		wrs := &otlpwire.ResourceSpans{Resource: rs.Resource.toWire()}
		for _, ss := range rs.ScopeSpans {
			wss := &otlpwire.ScopeSpans{}
			for _, sp := range ss.Spans {
				wsp := &otlpwire.Span{
					TraceID:           decodeHex(sp.TraceID),
					SpanID:            decodeHex(sp.SpanID),
					ParentSpanID:      decodeHex(sp.ParentSpanID),
					Name:              sp.Name,
					Kind:              sp.Kind,
					StartTimeUnixNano: parseUnixNano(sp.StartTimeUnixNano),
					EndTimeUnixNano:   parseUnixNano(sp.EndTimeUnixNano),
					Attributes:        toWireAttrs(sp.Attributes),
				}
				if sp.Status != nil {
					wsp.Status = &otlpwire.Status{Message: sp.Status.Message, Code: sp.Status.Code}
				}
				wss.Spans = append(wss.Spans, wsp)
			}
			wrs.ScopeSpans = append(wrs.ScopeSpans, wss)
		}
		out.ResourceSpans = append(out.ResourceSpans, wrs)
	}
	return out, nil
}

// --- logs ---

type jsonLogRecord struct {
	TimeUnixNano   json.RawMessage `json:"timeUnixNano"`
	SeverityNumber int32           `json:"severityNumber,omitempty"`
	SeverityText   string          `json:"severityText,omitempty"`
	Body           *jsonAnyValue   `json:"body"`
	Attributes     []jsonKeyValue  `json:"attributes,omitempty"`
	TraceID        string          `json:"traceId,omitempty"`
	SpanID         string          `json:"spanId,omitempty"`
}

type jsonLogsRequest struct {
	ResourceLogs []struct {
		Resource  *jsonResource `json:"resource"`
		ScopeLogs []struct {
			LogRecords []jsonLogRecord `json:"logRecords"`
		} `json:"scopeLogs"`
	} `json:"resourceLogs"`
}

// UnmarshalJSONLogs decodes an OTLP/JSON log export.
func UnmarshalJSONLogs(body []byte) (*otlpwire.ExportLogsServiceRequest, error) {
	var req jsonLogsRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("otlp/json logs: %w", err)
	}
	out := &otlpwire.ExportLogsServiceRequest{}
	for _, rl := range req.ResourceLogs {
		wrl := &otlpwire.ResourceLogs{Resource: rl.Resource.toWire()}
		for _, sl := range rl.ScopeLogs {
			wsl := &otlpwire.ScopeLogs{}
			for _, rec := range sl.LogRecords {
				wsl.LogRecords = append(wsl.LogRecords, &otlpwire.LogRecord{
					TimeUnixNano:   parseUnixNano(rec.TimeUnixNano),
					SeverityNumber: rec.SeverityNumber,
					SeverityText:   rec.SeverityText,
					Body:           rec.Body.toWire(),
					Attributes:     toWireAttrs(rec.Attributes),
					TraceID:        decodeHex(rec.TraceID),
					SpanID:         decodeHex(rec.SpanID),
				})
			}
			wrl.ScopeLogs = append(wrl.ScopeLogs, wsl)
		}
		out.ResourceLogs = append(out.ResourceLogs, wrl)
	}
	return out, nil
}

// decodeHex turns an OTLP/JSON id back into bytes. A malformed id yields nil
// rather than an error: one unreadable id should cost that field, not the
// whole export that carried it.
func decodeHex(s string) []byte {
	if s == "" {
		return nil
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}
