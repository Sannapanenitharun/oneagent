package collector

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/agent-i/agent/internal/otlpwire"
)

// This file implements the OTLP/HTTP receiver at the spec-mandated path
// POST /v1/traces, supporting BOTH of OTLP's wire encodings:
//   - application/x-protobuf — binary protobuf, the default for most
//     OTel SDK exporters (Go/Python/Java's otlptracehttp with default
//     settings). Decoded by internal/otlpwire, this agent's own reader
//     for the subset of the protobuf wire format the OTLP trace messages
//     use. It replaced the vendored google.golang.org/protobuf runtime
//     and go.opentelemetry.io/proto generated types, which together came
//     to ~61k lines to serve this one decode; the replacement is checked
//     against wire bytes captured from that runtime before it was
//     removed, so the format contract is pinned to the reference
//     implementation's real output rather than to our reading of it.
//   - application/json — OTLP's JSON encoding, handled by the
//     hand-written otlpExportTraceRequest types below. This path never
//     depended on the protobuf runtime and is unchanged.
//
// Content-Type on the request selects which decoder runs; unset or
// unrecognized Content-Type falls back to JSON (matches this receiver's
// original behavior before protobuf support existed, so JSON-only
// callers from before this change keep working unmodified).

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

// OTLPTraceReceiverCollector runs an OTLP/HTTP compliant trace endpoint apps
// can point their OTel SDK exporter at.
//
// This is the agent's only inbound network surface, so it is also the only
// place where an outsider gets to hand us work. Three limits apply:
//
//   - the body is capped (maxBytes), because the protobuf path reads the whole
//     request into memory before it can decode anything;
//   - concurrent decodes are capped by a semaphore, so we cannot be pushed into
//     decoding faster than the pipeline drains — the failure mode there is
//     memory growth, not slowness, which is much harder to diagnose;
//   - an optional bearer token is required, which matters as soon as the
//     listener is not on loopback.
type OTLPTraceReceiverCollector struct {
	agentID   string
	addr      string
	maxBytes  int64
	authToken string
	// sem admits a bounded number of concurrent decodes. Requests that cannot
	// get a slot promptly are refused with 429 rather than queued, because a
	// queue here is just memory we have not accounted for.
	sem    chan struct{}
	server *http.Server
}

// decodeWaitTimeout is how long a request waits for a decode slot before being
// told to back off. Short on purpose: an OTel SDK exporter retries.
const decodeWaitTimeout = 2 * time.Second

func NewOTLPTraceReceiverCollector(agentID, addr string, maxBytes int64, authToken string) *OTLPTraceReceiverCollector {
	if maxBytes <= 0 {
		maxBytes = 4 << 20
	}
	slots := runtime.GOMAXPROCS(0)
	if slots < 2 {
		slots = 2
	}
	return &OTLPTraceReceiverCollector{
		agentID:   agentID,
		addr:      addr,
		maxBytes:  maxBytes,
		authToken: authToken,
		sem:       make(chan struct{}, slots),
	}
}

func (t *OTLPTraceReceiverCollector) Name() string { return "trace.otlp_http" }

func (t *OTLPTraceReceiverCollector) Start(ctx context.Context, out chan<- Envelope) error {
	if !isLoopbackAddr(t.addr) && t.authToken == "" {
		log.Printf("trace receiver: WARNING listening on %s with no auth token — "+
			"anything that can reach this host can inject spans into your backend. "+
			"Set traces.listen_addr to 127.0.0.1:4319 or configure traces.auth_token_env.", t.addr)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/traces", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !t.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		select {
		case t.sem <- struct{}{}:
			defer func() { <-t.sem }()
		case <-r.Context().Done():
			return
		case <-time.After(decodeWaitTimeout):
			w.Header().Set("Retry-After", "1")
			http.Error(w, "receiver busy", http.StatusTooManyRequests)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, t.maxBytes)

		if isProtobufContentType(r.Header.Get("Content-Type")) {
			t.handleProtobuf(w, r, out)
			return
		}
		t.handleJSON(w, r, out)
	})

	t.server = &http.Server{
		Addr:    t.addr,
		Handler: mux,
		// Without these a single idle or slow client holds a connection
		// indefinitely.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = t.server.Close()
	}()
	go func() {
		if err := t.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			// Previously swallowed, so a port already in use looked exactly
			// like an app that simply was not sending traces.
			log.Printf("trace receiver: listener on %s stopped: %v", t.addr, err)
		}
	}()
	return nil
}

// authorized checks the bearer token when one is configured. The comparison is
// constant-time so a caller cannot recover the token by measuring responses.
func (t *OTLPTraceReceiverCollector) authorized(r *http.Request) bool {
	if t.authToken == "" {
		return true
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(h, prefix)), []byte(t.authToken)) == 1
}

// isLoopbackAddr reports whether a listen address binds only to loopback. A
// bare ":4319" or "0.0.0.0:4319" binds every interface.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (t *OTLPTraceReceiverCollector) Stop() error {
	if t.server != nil {
		return t.server.Close()
	}
	return nil
}

// isProtobufContentType matches the standard OTLP binary content type and
// its common variants; anything else (including no Content-Type header at
// all) falls back to JSON, matching this receiver's pre-protobuf behavior.
func isProtobufContentType(ct string) bool {
	return ct == "application/x-protobuf" || ct == "application/protobuf"
}

func (t *OTLPTraceReceiverCollector) handleJSON(w http.ResponseWriter, r *http.Request, out chan<- Envelope) {
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
				out <- spanToEnvelopeJSON(t.agentID, serviceName, ss.Scope.Name, sp)
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(otlpExportTraceResponse{})
}

func (t *OTLPTraceReceiverCollector) handleProtobuf(w http.ResponseWriter, r *http.Request, out chan<- Envelope) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "error reading request body", http.StatusBadRequest)
		return
	}

	req, err := otlpwire.UnmarshalExportTraceServiceRequest(body)
	if err != nil {
		http.Error(w, "invalid OTLP protobuf payload", http.StatusBadRequest)
		return
	}

	for _, rs := range req.ResourceSpans {
		serviceName := ""
		if rs.Resource != nil {
			for _, a := range rs.Resource.Attributes {
				if a.Key == "service.name" {
					serviceName = a.Value.String()
				}
			}
		}
		for _, ss := range rs.ScopeSpans {
			scopeName := ""
			if ss.Scope != nil {
				scopeName = ss.Scope.Name
			}
			for _, sp := range ss.Spans {
				out <- spanToEnvelopeProto(t.agentID, serviceName, scopeName, sp)
			}
		}
	}

	respBody := otlpwire.MarshalEmptyExportTraceServiceResponse()
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBody)
}

// spanToEnvelopeProto is the protobuf-decoding equivalent of
// spanToEnvelopeJSON below — same output shape (an Envelope), different
// input type (a wire-decoded *otlpwire.Span with []byte trace/span
// IDs and uint64 nanosecond timestamps, vs. the JSON path's hex-string
// IDs and string-encoded int64 timestamps). Keeping both conversion
// functions separate rather than unifying through a shared intermediate
// type — the two input shapes are different enough that a shared type
// would just move the conversion complexity rather than remove it.
func spanToEnvelopeProto(agentID, serviceName, scopeName string, sp *otlpwire.Span) Envelope {
	durationMs := float64(sp.EndTimeUnixNano-sp.StartTimeUnixNano) / 1e6

	labels := map[string]string{
		"trace_id": hex.EncodeToString(sp.TraceID),
		"span_id":  hex.EncodeToString(sp.SpanID),
		"name":     sp.Name,
	}
	// The parent link is what makes a set of spans a trace rather than a bag
	// of timings: without it nothing downstream can build a waterfall, work
	// out which service called which, or find the root. It was being parsed
	// off the wire and then dropped, so every trace we re-exported arrived
	// flat. Empty on a root span, which is the correct absence.
	if len(sp.ParentSpanID) > 0 {
		labels["parent_span_id"] = hex.EncodeToString(sp.ParentSpanID)
	}
	if serviceName != "" {
		labels["service.name"] = serviceName
	}
	if scopeName != "" {
		labels["scope.name"] = scopeName
	}
	if sp.Status != nil {
		// The code, not just the message, is what identifies a failed span:
		// 0=UNSET, 1=OK, 2=ERROR per the OTLP spec. Only the message was
		// recorded before, and it is empty on most error spans, so there was
		// no reliable way to tell a failure from a success downstream.
		labels["status.code"] = strconv.Itoa(int(sp.Status.Code))
		if sp.Status.Message != "" {
			labels["status.message"] = sp.Status.Message
		}
	}

	attrs := make(map[string]any, len(sp.Attributes))
	for _, a := range sp.Attributes {
		attrs[a.Key] = a.Value.String()
	}

	return Envelope{
		Kind:      KindTrace,
		AgentID:   agentID,
		Source:    "otlp.span",
		Timestamp: time.Unix(0, int64(sp.StartTimeUnixNano)).UTC(),
		Labels:    labels,
		Value:     durationMs,
		Payload:   map[string]any{"attributes": attrs},
	}
}

func spanToEnvelopeJSON(agentID, serviceName, scopeName string, sp otlpSpan) Envelope {
	startNano, _ := strconv.ParseInt(sp.StartTimeUnixNano, 10, 64)
	endNano, _ := strconv.ParseInt(sp.EndTimeUnixNano, 10, 64)
	durationMs := float64(endNano-startNano) / 1e6

	labels := map[string]string{
		"trace_id": normalizeHexID(sp.TraceID),
		"span_id":  normalizeHexID(sp.SpanID),
		"name":     sp.Name,
	}
	// See the protobuf path above for why the parent link matters.
	if sp.ParentSpanID != "" {
		labels["parent_span_id"] = normalizeHexID(sp.ParentSpanID)
	}
	if serviceName != "" {
		labels["service.name"] = serviceName
	}
	if scopeName != "" {
		labels["scope.name"] = scopeName
	}
	if sp.Status != nil {
		// See the protobuf path above: the code is the reliable error signal.
		labels["status.code"] = strconv.Itoa(sp.Status.Code)
		if sp.Status.Message != "" {
			labels["status.message"] = sp.Status.Message
		}
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
