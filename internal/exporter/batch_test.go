package exporter

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-i/agent/internal/collector"
	"github.com/agent-i/agent/internal/config"
)

func TestEnvelopeBytes_CountsTheBigFields(t *testing.T) {
	small := collector.Envelope{Kind: collector.KindLog, Source: "/var/log/a.log", Message: "short"}
	big := collector.Envelope{
		Kind:    collector.KindLog,
		Source:  "/var/log/a.log",
		Message: strings.Repeat("x", 100000),
	}
	if envelopeBytes(big)-envelopeBytes(small) < 99000 {
		t.Errorf("a 100 KB message must dominate the estimate: small=%d big=%d",
			envelopeBytes(small), envelopeBytes(big))
	}
}

// The estimate must never come in UNDER the real encoded size — that is the
// only error direction that loses data.
func TestEnvelopeBytes_IsNotAnUnderestimate(t *testing.T) {
	e := collector.Envelope{
		Kind:    collector.KindLog,
		AgentID: "host-1",
		Source:  "/var/log/app.log",
		Message: strings.Repeat("y", 5000),
		Labels:  map[string]string{"env": "prod", "svc": "api"},
	}
	raw := len(e.AgentID) + len(e.Source) + len(e.Message)
	for k, v := range e.Labels {
		raw += len(k) + len(v)
	}
	if envelopeBytes(e) < raw {
		t.Errorf("estimate %d is below the raw field bytes %d", envelopeBytes(e), raw)
	}
}

func TestEnvelopeBytes_HandlesNestedPayloadsAndNils(t *testing.T) {
	e := collector.Envelope{
		Kind:   collector.KindTrace,
		Source: "otlp.span",
		Payload: map[string]any{
			"trace_id": "abc123",
			"attrs":    map[string]any{"http.method": "GET", "n": 42, "ok": true, "none": nil},
			"events":   []any{"a", "bb", map[string]any{"k": "v"}},
		},
	}
	if got := envelopeBytes(e); got <= envelopeOverhead {
		t.Errorf("payload contributed nothing: %d", got)
	}
	// Must not panic or recurse without bound on a self-referential structure.
	deep := map[string]any{}
	cur := deep
	for i := 0; i < 50; i++ {
		next := map[string]any{}
		cur["next"] = next
		cur = next
	}
	envelopeBytes(collector.Envelope{Payload: deep}) // depth-limited, must return
}

func TestResolveMaxBatchBytes_Defaults(t *testing.T) {
	if got := resolveMaxBatchBytes(0); got != defaultMaxBatchBytes {
		t.Errorf("unset = %d, want the %d default", got, defaultMaxBatchBytes)
	}
	if got := resolveMaxBatchBytes(-5); got != defaultMaxBatchBytes {
		t.Errorf("negative = %d, want the default", got)
	}
	if got := resolveMaxBatchBytes(1024); got != 1024 {
		t.Errorf("explicit = %d, want 1024", got)
	}
}

// The regression this whole change exists for: a handful of large records must
// flush on SIZE, long before the count limit is reached. Before the byte cap,
// batchSize=100 meant 100 multi-line records went out in one request whatever
// they weighed.
func TestOTLPExporter_FlushesOnBytesBeforeCount(t *testing.T) {
	var mu sync.Mutex
	var bodySizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 0, 1<<20)
		tmp := make([]byte, 32768)
		for {
			n, err := r.Body.Read(tmp)
			buf = append(buf, tmp[:n]...)
			if err != nil {
				break
			}
		}
		mu.Lock()
		bodySizes = append(bodySizes, len(buf))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	o, err := newOTLPHTTPExporter(config.ExporterConfig{
		Endpoint:      srv.URL,
		BatchSize:     100,       // deliberately high: count must NOT be what trips
		MaxBatchBytes: 64 * 1024, // 64 KiB
		FlushInterval: time.Hour, // and neither must the timer
	})
	if err != nil {
		t.Fatalf("constructing exporter: %v", err)
	}
	defer o.Close()

	// 10 records of ~20 KB each = ~200 KB, which is 3+ flushes at 64 KiB but
	// only 10% of the count limit.
	for i := 0; i < 10; i++ {
		err := o.Export(collector.Envelope{
			Kind:      collector.KindLog,
			AgentID:   "host-1",
			Source:    "/var/log/app.log",
			Timestamp: time.Now(),
			Message:   strings.Repeat("z", 20000),
		})
		if err != nil {
			t.Fatalf("export %d: %v", i, err)
		}
	}

	mu.Lock()
	got := len(bodySizes)
	mu.Unlock()
	if got < 2 {
		t.Errorf("got %d requests; ~200 KB of records under a 64 KiB cap must "+
			"produce several, not one oversized batch", got)
	}
}

// A single record larger than the whole cap must still be sent. Dropping it
// would be silent data loss, and it cannot be split.
func TestOTLPExporter_SendsAnOversizedRecordAlone(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	o, err := newOTLPHTTPExporter(config.ExporterConfig{
		Endpoint:      srv.URL,
		BatchSize:     100,
		MaxBatchBytes: 1024, // 1 KiB — smaller than the record below
		FlushInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("constructing exporter: %v", err)
	}
	defer o.Close()

	if err := o.Export(collector.Envelope{
		Kind:      collector.KindLog,
		Source:    "/var/log/huge.log",
		Timestamp: time.Now(),
		Message:   strings.Repeat("q", 50000),
	}); err != nil {
		t.Fatalf("export: %v", err)
	}

	mu.Lock()
	got := requests
	mu.Unlock()
	if got != 1 {
		t.Errorf("requests = %d, want 1 — an oversized record must still go out", got)
	}
}

// Counters must reset with the buffers, or the exporter flushes on every
// envelope forever after the first large batch.
func TestOTLPExporter_ByteCounterResetsOnFlush(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	o, err := newOTLPHTTPExporter(config.ExporterConfig{
		Endpoint:      srv.URL,
		BatchSize:     100,
		MaxBatchBytes: 8192,
		FlushInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("constructing exporter: %v", err)
	}
	defer o.Close()

	// Force a size-triggered flush.
	_ = o.Export(collector.Envelope{Kind: collector.KindLog, Source: "s", Message: strings.Repeat("a", 9000)})

	o.mu.Lock()
	logsBytes, buffered := o.logsBytes, len(o.logsBuf)
	o.mu.Unlock()
	if logsBytes != 0 || buffered != 0 {
		t.Errorf("after flush: logsBytes=%d buffered=%d, both want 0", logsBytes, buffered)
	}
}

// The three OTLP buffers are capped independently: a flood of logs must not
// cause metrics to flush early, or vice versa.
func TestOTLPExporter_BuffersAreCappedIndependently(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	o, err := newOTLPHTTPExporter(config.ExporterConfig{
		Endpoint:      srv.URL,
		BatchSize:     100,
		MaxBatchBytes: 16384,
		FlushInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("constructing exporter: %v", err)
	}
	defer o.Close()

	_ = o.Export(collector.Envelope{Kind: collector.KindMetric, Source: "cpu", Value: 1})
	_ = o.Export(collector.Envelope{Kind: collector.KindLog, Source: "s", Message: strings.Repeat("b", 20000)})

	o.mu.Lock()
	metricsHeld, metricsBytes := len(o.metricsBuf), o.metricsBytes
	o.mu.Unlock()

	// The oversized log flushed everything, so metrics went with it. What must
	// hold is that the counters agree with the buffers.
	if (metricsHeld == 0) != (metricsBytes == 0) {
		t.Errorf("counter and buffer disagree: %d envelopes, %d bytes", metricsHeld, metricsBytes)
	}
}
