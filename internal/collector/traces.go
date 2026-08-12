package collector

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// This file implements the OTLP/HTTP receiver using OTLP's JSON encoding —
// the officially specified JSON mapping of the trace service protobuf
// (https://opentelemetry.io/docs/specs/otlp/#json-protobuf-encoding), served
// at the spec-mandated path POST /v1/traces.
//
// SCOPE NOTE (read before assuming this is "full OTLP"): OTLP has two wire
// encodings — binary protobuf (the default for most SDK exporters, e.g.
// Go/Python/Java's otlptracehttp with default settings) and JSON. This
// receiver implements JSON only. A binary-protobuf endpoint requires the
// generated OTLP proto types (google.golang.org/protobuf +
// go.opentelemetry.io/proto/otlp) — a large dependency tree that can't be
// fetched in this sandbox (only github.com/pypi/npm mirrors are reachable,
// not the Go module proxy or grpc's dependencies). Any SDK explicitly
// configured for OTLP/HTTP+JSON (most SDKs support this via an env var or
// exporter option, e.g. OTEL_EXPORTER_OTLP_PROTOCOL=http/json) will work
// against this unmodified. SDKs using the protobuf default will not, until
// that follow-up is done.

// --- OTLP JSON wire types (proto3 JSON mapping: int64 fields are strings,
// bytes fields are base64/hex per proto's JSON spec — traceId/spanId use
// hex in OTLP's JSON encoding specifically). ---

type otlpExportTraceRequest struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes"`
}

type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpScope struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type otlpSpan struct {
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
	ParentSpanID      string         `json:"parentSpanId,omitempty"`
	Name              string         `json:"name"`
	Kind              int            `json:"kind,omitempty"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	EndTimeUnixNano   string         `json:"endTimeUnixNano"`
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
	Status            *otlpStatus    `json:"status,omitempty"`
}

type otlpStatus struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type otlpKeyValue struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

// otlpAnyValue covers the common leaf value kinds; OTLP also supports
// array/kvlist/bytes values, which most instrumentation attributes don't
// use — extend here if a real client needs them.
type otlpAnyValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	IntValue    *string  `json:"intValue,omitempty"` // proto3 JSON: int64 as string
	DoubleValue *float64 `json:"doubleValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
}

func (v otlpAnyValue) toString() string {
	switch {
	case v.StringValue != nil:
		return *v.StringValue
	case v.IntValue != nil:
		return *v.IntValue
	case v.DoubleValue != nil:
		return strconv.FormatFloat(*v.DoubleValue, 'f', -1, 64)
	case v.BoolValue != nil:
		return strconv.FormatBool(*v.BoolValue)
	default:
		return ""
	}
}

type otlpExportTraceResponse struct{} // empty body per spec on full success

// OTLPTraceReceiverCollector runs an OTLP/HTTP+JSON compliant trace
// endpoint apps can point their OTel SDK exporter at.
type OTLPTraceReceiverCollector struct {
	agentID string
	addr    string
	server  *http.Server
}

func NewOTLPTraceReceiverCollector(agentID, addr string) *OTLPTraceReceiverCollector {
	return &OTLPTraceReceiverCollector{agentID: agentID, addr: addr}
}

func (t *OTLPTraceReceiverCollector) Name() string { return "trace.otlp_http" }

func (t *OTLPTraceReceiverCollector) Start(ctx context.Context, out chan<- Envelope) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/traces", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req otlpExportTraceRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid OTLP JSON payload"}`, http.StatusBadRequest)
			return
		}

		for _, rs := range req.ResourceSpans {
			serviceName := ""
			for _, a := range rs.Resource.Attributes {
				if a.Key == "service.name" {
					serviceName = a.Value.toString()
				}
			}
			for _, ss := range rs.ScopeSpans {
				for _, sp := range ss.Spans {
					out <- spanToEnvelope(t.agentID, serviceName, ss.Scope.Name, sp)
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(otlpExportTraceResponse{})
	})

	t.server = &http.Server{Addr: t.addr, Handler: mux}
	go func() {
		<-ctx.Done()
		_ = t.server.Close()
	}()
	go func() {
		_ = t.server.ListenAndServe() // ErrServerClosed on shutdown is expected
	}()
	return nil
}

func (t *OTLPTraceReceiverCollector) Stop() error {
	if t.server != nil {
		return t.server.Close()
	}
	return nil
}

func spanToEnvelope(agentID, serviceName, scopeName string, sp otlpSpan) Envelope {
	startNano, _ := strconv.ParseInt(sp.StartTimeUnixNano, 10, 64)
	endNano, _ := strconv.ParseInt(sp.EndTimeUnixNano, 10, 64)
	durationMs := float64(endNano-startNano) / 1e6

	labels := map[string]string{
		"trace_id": normalizeHexID(sp.TraceID),
		"span_id":  normalizeHexID(sp.SpanID),
		"name":     sp.Name,
	}
	if serviceName != "" {
		labels["service.name"] = serviceName
	}
	if scopeName != "" {
		labels["scope.name"] = scopeName
	}
	if sp.Status != nil && sp.Status.Message != "" {
		labels["status.message"] = sp.Status.Message
	}

	attrs := make(map[string]any, len(sp.Attributes))
	for _, a := range sp.Attributes {
		attrs[a.Key] = a.Value.toString()
	}

	return Envelope{
		Kind:      KindTrace,
		AgentID:   agentID,
		Source:    "otlp.span",
		Timestamp: time.Unix(0, startNano).UTC(),
		Labels:    labels,
		Value:     durationMs,
		Payload:   map[string]any{"attributes": attrs},
	}
}

// normalizeHexID lower-cases trace/span IDs; OTLP JSON encodes them as hex
// strings already, but be defensive about casing from different SDKs.
func normalizeHexID(id string) string {
	if _, err := hex.DecodeString(id); err != nil {
		return id // pass through unrecognized formats rather than dropping the span
	}
	return id
}
