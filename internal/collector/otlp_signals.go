package collector

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/agent-i/agent/internal/otlpwire"
)

// This file adds the other two OTLP/HTTP signal endpoints beside the trace
// receiver in traces.go:
//
//	POST /v1/logs      application logs
//	POST /v1/metrics   application metrics
//
// They exist because an OTel SDK configured the ordinary way sends all three
// signals to one base endpoint. scripts/auto-instrument.sh sets
// OTEL_EXPORTER_OTLP_ENDPOINT, not the per-signal OTEL_EXPORTER_OTLP_*_ENDPOINT
// variables, so an auto-instrumented application was already posting its
// metrics and logs at this listener and getting 404s for them — the traces
// arrived and the other two signals were silently lost.
//
// Both encodings are supported on both paths, selected by Content-Type exactly
// as the trace path does it, because the same SDK export pipeline produces all
// three and it would be odd for one to accept protobuf and another not.
//
// Everything here converts into the same Envelope the rest of the agent
// already carries, so nothing downstream needed changing: an application log
// becomes a KindLog like a tailed file line, and an application metric becomes
// a KindMetric like a /proc reading.

// --- OTLP JSON wire types ---
//
// The proto3 JSON mapping is the same one traces.go documents: int64 and
// uint64 fields are strings, and doubles are numbers. otlpKeyValue,
// otlpAnyValue, otlpScope and otlpResource are shared with the trace types
// rather than restated.

type otlpExportLogsRequest struct {
	ResourceLogs []otlpResourceLogs `json:"resourceLogs"`
}

type otlpResourceLogs struct {
	Resource  otlpResource    `json:"resource"`
	ScopeLogs []otlpScopeLogs `json:"scopeLogs"`
}

type otlpScopeLogs struct {
	Scope      otlpScope       `json:"scope"`
	LogRecords []otlpLogRecord `json:"logRecords"`
}

type otlpLogRecord struct {
	TimeUnixNano         string         `json:"timeUnixNano"`
	ObservedTimeUnixNano string         `json:"observedTimeUnixNano"`
	SeverityNumber       int            `json:"severityNumber"`
	SeverityText         string         `json:"severityText"`
	Body                 otlpAnyValue   `json:"body"`
	Attributes           []otlpKeyValue `json:"attributes"`
	TraceID              string         `json:"traceId"`
	SpanID               string         `json:"spanId"`
}

type otlpExportMetricsRequest struct {
	ResourceMetrics []otlpResourceMetrics `json:"resourceMetrics"`
}

type otlpResourceMetrics struct {
	Resource     otlpResource       `json:"resource"`
	ScopeMetrics []otlpScopeMetrics `json:"scopeMetrics"`
}

type otlpScopeMetrics struct {
	Scope   otlpScope    `json:"scope"`
	Metrics []otlpMetric `json:"metrics"`
}

// otlpMetric models the data oneof as three optional pointers. Exactly one is
// set by a conforming producer; a payload setting none is a metric of a type
// this agent does not decode (summary, exponential histogram) and is dropped.
type otlpMetric struct {
	Name      string         `json:"name"`
	Unit      string         `json:"unit"`
	Gauge     *otlpGauge     `json:"gauge,omitempty"`
	Sum       *otlpSum       `json:"sum,omitempty"`
	Histogram *otlpHistogram `json:"histogram,omitempty"`
}

type otlpGauge struct {
	DataPoints []otlpNumberDataPoint `json:"dataPoints"`
}

type otlpSum struct {
	DataPoints  []otlpNumberDataPoint `json:"dataPoints"`
	IsMonotonic bool                  `json:"isMonotonic"`
}

type otlpHistogram struct {
	DataPoints []otlpHistogramDataPoint `json:"dataPoints"`
}

// otlpNumberDataPoint carries the value oneof as two optional fields. AsInt is
// a string per the proto3 JSON mapping for int64; AsDouble is a plain number.
type otlpNumberDataPoint struct {
	TimeUnixNano string         `json:"timeUnixNano"`
	AsDouble     *float64       `json:"asDouble,omitempty"`
	AsInt        *string        `json:"asInt,omitempty"`
	Attributes   []otlpKeyValue `json:"attributes"`
}

// value resolves the oneof. A point with neither field set is a zero reading
// rather than an error: OTLP's JSON encoding omits default values, so a
// genuine asInt of 0 arrives as no field at all.
func (p otlpNumberDataPoint) value() float64 {
	if p.AsDouble != nil {
		return *p.AsDouble
	}
	if p.AsInt != nil {
		n, _ := strconv.ParseInt(*p.AsInt, 10, 64)
		return float64(n)
	}
	return 0
}

type otlpHistogramDataPoint struct {
	TimeUnixNano string         `json:"timeUnixNano"`
	Count        string         `json:"count"`
	Sum          *float64       `json:"sum,omitempty"`
	Attributes   []otlpKeyValue `json:"attributes"`
}

// --- envelope conversion ---

// otlpLogSource and otlpMetricSource name where a signal entered the agent, in
// the same style as the trace path's "otlp.span".
const otlpLogSource = "otlp.log"

// logEnvelope builds the Envelope for one decoded log record.
//
// Shared by the JSON and protobuf paths so the two encodings cannot drift into
// producing different telemetry for the same input — the parameters are the
// already-decoded values rather than either wire type.
func logEnvelope(agentID, serviceName, scopeName, body, severityText string,
	severityNumber int, traceID, spanID string, timeNano int64, attrs map[string]any) Envelope {

	labels := map[string]string{}
	if serviceName != "" {
		labels["service.name"] = serviceName
	}
	if scopeName != "" {
		labels["scope.name"] = scopeName
	}
	// Both are kept. severity_text is what the application called the level and
	// is what someone reads; severity_number is OTLP's ordered scale and is the
	// only one of the two a filter can compare with > or <. Either can be
	// absent — an SDK emitting one without the other is common.
	if severityText != "" {
		labels["severity"] = severityText
	}
	if severityNumber != 0 {
		labels["severity.number"] = strconv.Itoa(severityNumber)
	}
	// The link back to the trace this line was emitted inside. Without it an
	// application's logs and its traces are two unrelated lists.
	if traceID != "" {
		labels["trace_id"] = traceID
	}
	if spanID != "" {
		labels["span_id"] = spanID
	}

	// A record with no timestamp is legal on the wire. Falling back to now
	// rather than to the zero time keeps it in the dashboard's retention
	// window instead of placing it in 1970 where nothing will ever show it.
	ts := time.Now().UTC()
	if timeNano > 0 {
		ts = time.Unix(0, timeNano).UTC()
	}

	env := Envelope{
		Kind:      KindLog,
		AgentID:   agentID,
		Source:    otlpLogSource,
		Timestamp: ts,
		Labels:    labels,
		Message:   body,
	}
	if len(attrs) > 0 {
		env.Payload = map[string]any{"attributes": attrs}
	}
	return env
}

// metricEnvelopes builds the Envelopes for one decoded metric data point.
//
// Returns a slice because a histogram point becomes two series. The name is
// the Envelope's Source, matching how every other metric in the agent is
// identified, so an application metric lands in the dashboard's metric list
// beside the host ones with no special handling.
func metricEnvelope(agentID, name, unit, serviceName, scopeName string,
	timeNano int64, value float64, attrs map[string]string) Envelope {

	labels := make(map[string]string, len(attrs)+3)
	// Point attributes first, so a metric that carries its own service.name
	// attribute cannot overwrite the resource-level one below — the resource
	// is the authority on which service emitted this.
	for k, v := range attrs {
		labels[k] = v
	}
	if serviceName != "" {
		labels["service.name"] = serviceName
	}
	if scopeName != "" {
		labels["scope.name"] = scopeName
	}
	if unit != "" {
		labels["unit"] = unit
	}

	ts := time.Now().UTC()
	if timeNano > 0 {
		ts = time.Unix(0, timeNano).UTC()
	}

	return Envelope{
		Kind:      KindMetric,
		AgentID:   agentID,
		Source:    name,
		Timestamp: ts,
		Labels:    labels,
		Value:     value,
	}
}

// --- logs: handlers ---

func (t *OTLPReceiverCollector) handleLogsJSON(w http.ResponseWriter, r *http.Request, out chan<- Envelope) {
	var req otlpExportLogsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid OTLP JSON payload"}`, http.StatusBadRequest)
		return
	}

	for _, rl := range req.ResourceLogs {
		serviceName := serviceNameJSON(rl.Resource)
		for _, sl := range rl.ScopeLogs {
			for _, lr := range sl.LogRecords {
				attrs := make(map[string]any, len(lr.Attributes))
				for _, a := range lr.Attributes {
					attrs[a.Key] = a.Value.toString()
				}
				// Observed time is the fallback the spec intends when the
				// emitter did not stamp one.
				nano, _ := strconv.ParseInt(lr.TimeUnixNano, 10, 64)
				if nano == 0 {
					nano, _ = strconv.ParseInt(lr.ObservedTimeUnixNano, 10, 64)
				}
				out <- logEnvelope(t.agentID, serviceName, sl.Scope.Name,
					lr.Body.toString(), lr.SeverityText, lr.SeverityNumber,
					normalizeHexID(lr.TraceID), normalizeHexID(lr.SpanID), nano, attrs)
			}
		}
	}

	writeEmptyJSONOK(w)
}

func (t *OTLPReceiverCollector) handleLogsProtobuf(w http.ResponseWriter, r *http.Request, out chan<- Envelope) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "error reading request body", http.StatusBadRequest)
		return
	}
	req, err := otlpwire.UnmarshalExportLogsServiceRequest(body)
	if err != nil {
		http.Error(w, "invalid OTLP protobuf payload", http.StatusBadRequest)
		return
	}

	for _, rl := range req.ResourceLogs {
		serviceName := serviceNameProto(rl.Resource)
		for _, sl := range rl.ScopeLogs {
			scopeName := ""
			if sl.Scope != nil {
				scopeName = sl.Scope.Name
			}
			for _, lr := range sl.LogRecords {
				attrs := make(map[string]any, len(lr.Attributes))
				for _, a := range lr.Attributes {
					attrs[a.Key] = a.Value.String()
				}
				out <- logEnvelope(t.agentID, serviceName, scopeName,
					lr.Body.String(), lr.SeverityText, int(lr.SeverityNumber),
					hexOrEmpty(lr.TraceID), hexOrEmpty(lr.SpanID),
					int64(lr.TimeUnixNano), attrs)
			}
		}
	}

	writeEmptyProtoOK(w, otlpwire.MarshalEmptyExportLogsServiceResponse())
}

// --- metrics: handlers ---

func (t *OTLPReceiverCollector) handleMetricsJSON(w http.ResponseWriter, r *http.Request, out chan<- Envelope) {
	var req otlpExportMetricsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid OTLP JSON payload"}`, http.StatusBadRequest)
		return
	}

	for _, rm := range req.ResourceMetrics {
		serviceName := serviceNameJSON(rm.Resource)
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name == "" {
					continue // an unnamed metric has nothing to key a series on
				}
				var points []otlpNumberDataPoint
				switch {
				case m.Gauge != nil:
					points = m.Gauge.DataPoints
				case m.Sum != nil:
					points = m.Sum.DataPoints
				}
				for _, p := range points {
					nano, _ := strconv.ParseInt(p.TimeUnixNano, 10, 64)
					out <- metricEnvelope(t.agentID, m.Name, m.Unit, serviceName,
						sm.Scope.Name, nano, p.value(), attrsJSON(p.Attributes))
				}
				if m.Histogram == nil {
					continue
				}
				for _, p := range m.Histogram.DataPoints {
					nano, _ := strconv.ParseInt(p.TimeUnixNano, 10, 64)
					count, _ := strconv.ParseUint(p.Count, 10, 64)
					attrs := attrsJSON(p.Attributes)
					sum, hasSum := 0.0, false
					if p.Sum != nil {
						sum, hasSum = *p.Sum, true
					}
					for _, env := range histogramEnvelopes(t.agentID, m.Name, m.Unit,
						serviceName, sm.Scope.Name, nano, count, sum, hasSum, attrs) {
						out <- env
					}
				}
			}
		}
	}

	writeEmptyJSONOK(w)
}

func (t *OTLPReceiverCollector) handleMetricsProtobuf(w http.ResponseWriter, r *http.Request, out chan<- Envelope) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "error reading request body", http.StatusBadRequest)
		return
	}
	req, err := otlpwire.UnmarshalExportMetricsServiceRequest(body)
	if err != nil {
		http.Error(w, "invalid OTLP protobuf payload", http.StatusBadRequest)
		return
	}

	for _, rm := range req.ResourceMetrics {
		serviceName := serviceNameProto(rm.Resource)
		for _, sm := range rm.ScopeMetrics {
			scopeName := ""
			if sm.Scope != nil {
				scopeName = sm.Scope.Name
			}
			for _, m := range sm.Metrics {
				if m.Name == "" {
					continue
				}
				for _, p := range m.NumberPoints {
					out <- metricEnvelope(t.agentID, m.Name, m.Unit, serviceName,
						scopeName, int64(p.TimeUnixNano), p.Value, attrsProto(p.Attributes))
				}
				for _, p := range m.HistogramPoints {
					for _, env := range histogramEnvelopes(t.agentID, m.Name, m.Unit,
						serviceName, scopeName, int64(p.TimeUnixNano),
						p.Count, p.Sum, p.HasSum, attrsProto(p.Attributes)) {
						out <- env
					}
				}
			}
		}
	}

	writeEmptyProtoOK(w, otlpwire.MarshalEmptyExportMetricsServiceResponse())
}

// histogramEnvelopes reduces a histogram data point to the two series the
// agent's metric shape can carry.
//
// A histogram's buckets do not fit an Envelope, whose Value is one float. The
// count and the sum do, and they are the two that stay correct under
// aggregation: their rates give throughput and total time, and their ratio
// gives the mean. Quantiles are deliberately not synthesised — they cannot be
// recovered from count and sum, and producing a plausible-looking p95 from
// data that does not contain one would be worse than offering none.
//
// The ".count" and ".sum" suffixes follow the convention Prometheus uses for
// exactly this reduction, so the names are ones an operator already reads.
func histogramEnvelopes(agentID, name, unit, serviceName, scopeName string,
	timeNano int64, count uint64, sum float64, hasSum bool, attrs map[string]string) []Envelope {

	envs := []Envelope{
		metricEnvelope(agentID, name+".count", unit, serviceName, scopeName,
			timeNano, float64(count), attrs),
	}
	// Omitted rather than sent as 0 when the producer sent no sum: a zero here
	// is a real reading (every observation was 0) and must not be manufactured.
	if hasSum {
		envs = append(envs, metricEnvelope(agentID, name+".sum", unit, serviceName,
			scopeName, timeNano, sum, attrs))
	}
	return envs
}

// --- small shared helpers ---

func serviceNameJSON(res otlpResource) string {
	for _, a := range res.Attributes {
		if a.Key == "service.name" {
			return a.Value.toString()
		}
	}
	return ""
}

func serviceNameProto(res *otlpwire.Resource) string {
	if res == nil {
		return ""
	}
	for _, a := range res.Attributes {
		if a.Key == "service.name" {
			return a.Value.String()
		}
	}
	return ""
}

func attrsJSON(in []otlpKeyValue) map[string]string {
	out := make(map[string]string, len(in))
	for _, a := range in {
		out[a.Key] = a.Value.toString()
	}
	return out
}

func attrsProto(in []*otlpwire.KeyValue) map[string]string {
	out := make(map[string]string, len(in))
	for _, a := range in {
		out[a.Key] = a.Value.String()
	}
	return out
}

// hexOrEmpty renders an id, returning "" for an absent one rather than the
// empty-string encoding of no bytes, so the caller can omit the label.
func hexOrEmpty(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return hex.EncodeToString(b)
}

func writeEmptyJSONOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("{}"))
}

func writeEmptyProtoOK(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
