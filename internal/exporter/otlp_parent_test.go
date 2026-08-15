package exporter

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agent-i/agent/internal/collector"
	"github.com/agent-i/agent/internal/config"
)

// readGzipBody returns the decompressed request body. Distinct from
// decodeGzipJSON because this test asserts on the raw JSON text, not just the
// decoded struct: omitempty behaviour is invisible once unmarshalled.
func readGzipBody(t *testing.T, r *http.Request) []byte {
	t.Helper()
	gr, err := gzip.NewReader(r.Body)
	if err != nil {
		t.Fatalf("gzip decode: %v", err)
	}
	defer gr.Close()
	body, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return body
}

// The parent link has to reach the backend as a first-class OTLP field, not
// merely as a span attribute — a backend reconstructs the call tree from
// parentSpanId and ignores attributes for that purpose. Sending it only as an
// attribute would leave SigNoz rendering a flat span list, which is what the
// agent was doing before.
func TestOTLPHTTPExporter_SetsParentSpanIDField(t *testing.T) {
	var got otlpTracesRequest
	var raw string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readGzipBody(t, r)
		raw = string(body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	exp, err := newOTLPHTTPExporter(config.ExporterConfig{
		Endpoint: server.URL, BatchSize: 2, FlushInterval: time.Hour, MaxRetries: 1,
	})
	if err != nil {
		t.Fatalf("newOTLPHTTPExporter: %v", err)
	}
	defer exp.Close()

	ts := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	// A root and its child, the minimum needed to prove a tree survives.
	if err := exp.Export(collector.Envelope{
		Kind: collector.KindTrace, AgentID: "host-001", Timestamp: ts, Value: 100,
		Labels: map[string]string{
			"trace_id": "t1", "span_id": "root1", "name": "POST /checkout", "service.name": "checkout",
		},
	}); err != nil {
		t.Fatalf("Export root: %v", err)
	}
	if err := exp.Export(collector.Envelope{
		Kind: collector.KindTrace, AgentID: "host-001", Timestamp: ts, Value: 40,
		Labels: map[string]string{
			"trace_id": "t1", "span_id": "child1", "parent_span_id": "root1",
			"name": "SELECT orders", "service.name": "checkout",
		},
	}); err != nil {
		t.Fatalf("Export child: %v", err)
	}

	if len(got.ResourceSpans) == 0 {
		t.Fatalf("no resourceSpans received")
	}
	spans := got.ResourceSpans[0].ScopeSpans[0].Spans
	if len(spans) != 2 {
		t.Fatalf("expected 2 spans, got %d", len(spans))
	}

	byID := map[string]otlpSpan{}
	for _, s := range spans {
		byID[s.SpanID] = s
	}
	if byID["child1"].ParentSpanID != "root1" {
		t.Errorf("child parentSpanId = %q, want root1 — the backend cannot build a waterfall without it", byID["child1"].ParentSpanID)
	}
	if byID["root1"].ParentSpanID != "" {
		t.Errorf("root parentSpanId = %q, want empty", byID["root1"].ParentSpanID)
	}
	// omitempty must actually drop the key on the root, since OTLP marks the
	// root by the field's absence rather than by an empty value.
	if strings.Count(raw, `"parentSpanId"`) != 1 {
		t.Errorf("expected parentSpanId to appear exactly once (child only), got %d occurrences: %s",
			strings.Count(raw, `"parentSpanId"`), raw)
	}
}
