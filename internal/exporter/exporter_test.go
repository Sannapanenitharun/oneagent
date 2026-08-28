package exporter

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-i/agent/internal/collector"
	"github.com/agent-i/agent/internal/config"
)

func decodeGzipBatch(t *testing.T, r *http.Request) []collector.Envelope {
	t.Helper()
	if r.Header.Get("Content-Encoding") != "gzip" {
		t.Errorf("request missing Content-Encoding: gzip header")
	}
	gr, err := gzip.NewReader(r.Body)
	if err != nil {
		t.Fatalf("decompressing request body: %v", err)
	}
	defer gr.Close()
	body, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("reading decompressed body: %v", err)
	}
	var batch []collector.Envelope
	if err := json.Unmarshal(body, &batch); err != nil {
		t.Fatalf("unmarshaling batch: %v", err)
	}
	return batch
}

func TestHTTPExporter_BatchesBySize(t *testing.T) {
	var requestCount int32
	var lastBatchSize int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		batch := decodeGzipBatch(t, r)
		lastBatchSize = len(batch)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	exp, err := newHTTPExporter(config.ExporterConfig{
		Endpoint:      server.URL,
		BatchSize:     3,
		FlushInterval: time.Hour, // effectively disabled — force size-triggered flush
		MaxRetries:    2,
	})
	if err != nil {
		t.Fatalf("newHTTPExporter: %v", err)
	}
	defer exp.Close()

	for i := 0; i < 3; i++ {
		if err := exp.Export(collector.Envelope{Source: "test", Value: float64(i)}); err != nil {
			t.Fatalf("Export #%d: %v", i, err)
		}
	}

	if got := atomic.LoadInt32(&requestCount); got != 1 {
		t.Errorf("expected exactly 1 flush request after 3 envelopes with batch_size=3, got %d", got)
	}
	if lastBatchSize != 3 {
		t.Errorf("expected batch of 3 envelopes, got %d", lastBatchSize)
	}
}

func TestHTTPExporter_FlushIntervalFlushesPartialBatch(t *testing.T) {
	received := make(chan int, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		batch := decodeGzipBatch(t, r)
		received <- len(batch)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	exp, err := newHTTPExporter(config.ExporterConfig{
		Endpoint:      server.URL,
		BatchSize:     100, // never reached
		FlushInterval: 50 * time.Millisecond,
		MaxRetries:    1,
	})
	if err != nil {
		t.Fatalf("newHTTPExporter: %v", err)
	}
	defer exp.Close()

	if err := exp.Export(collector.Envelope{Source: "test"}); err != nil {
		t.Fatalf("Export: %v", err)
	}

	select {
	case n := <-received:
		if n != 1 {
			t.Errorf("expected partial batch of 1, got %d", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("flush_interval did not trigger a flush of the partial batch in time")
	}
}

func TestHTTPExporter_RetriesOn5xxThenSucceeds(t *testing.T) {
	var attempts int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		decodeGzipBatch(t, r) // still must be valid gzip+JSON on every attempt
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	exp, err := newHTTPExporter(config.ExporterConfig{
		Endpoint:      server.URL,
		BatchSize:     1,
		FlushInterval: time.Hour,
		MaxRetries:    5,
	})
	if err != nil {
		t.Fatalf("newHTTPExporter: %v", err)
	}
	defer exp.Close()

	if err := exp.Export(collector.Envelope{Source: "test"}); err != nil {
		t.Fatalf("Export should have succeeded after retries, got error: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("expected exactly 3 attempts (2 failures + 1 success), got %d", got)
	}
}

func TestHTTPExporter_DoesNotRetryOn4xx(t *testing.T) {
	var attempts int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	exp, err := newHTTPExporter(config.ExporterConfig{
		Endpoint:      server.URL,
		BatchSize:     1,
		FlushInterval: time.Hour,
		MaxRetries:    5,
	})
	if err != nil {
		t.Fatalf("newHTTPExporter: %v", err)
	}
	defer exp.Close()

	err = exp.Export(collector.Envelope{Source: "test"})
	if err == nil {
		t.Fatal("expected an error for a 400 response, got nil")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("expected exactly 1 attempt for a 4xx (no retry), got %d", got)
	}
}

func TestHTTPExporter_CloseFlushesRemainingBuffer(t *testing.T) {
	received := make(chan int, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		batch := decodeGzipBatch(t, r)
		received <- len(batch)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	exp, err := newHTTPExporter(config.ExporterConfig{
		Endpoint:      server.URL,
		BatchSize:     100, // never reached during the test
		FlushInterval: time.Hour,
		MaxRetries:    1,
	})
	if err != nil {
		t.Fatalf("newHTTPExporter: %v", err)
	}

	for i := 0; i < 5; i++ {
		if err := exp.Export(collector.Envelope{Source: "test"}); err != nil {
			t.Fatalf("Export: %v", err)
		}
	}

	if err := exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case n := <-received:
		if n != 5 {
			t.Errorf("expected Close to flush all 5 buffered envelopes, got %d", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not flush the buffered envelopes")
	}
}

// The stdout exporter is the shipped default, so a fresh install collects
// correctly, reports healthy, and delivers nothing. On a systemd host the
// envelopes land in the journal and are rotated away, which means the evidence
// that collection works is also the reason the missing delivery goes unnoticed.
// It has to announce itself.
func TestNew_StdoutExporterAnnouncesThatItSendsNowhere(t *testing.T) {
	for _, tc := range []struct{ name, typ, want string }{
		{"explicit", "stdout", `"stdout"`},
		// An unset value is worth distinguishing: the operator never chose it.
		{"unset", "", `"stdout (unset)"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := log.Writer()
			log.SetOutput(&buf)
			defer log.SetOutput(prev)

			exp, err := New(config.ExporterConfig{Type: tc.typ}, nil)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer exp.Close()

			out := buf.String()
			if !strings.Contains(out, "sent NOWHERE") {
				t.Errorf("startup said nothing about telemetry going nowhere; got %q", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("message does not name the configured type %s; got %q", tc.want, out)
			}
			// The message is useless without the way out of it.
			if !strings.Contains(out, "otlp_http") {
				t.Errorf("message does not say what to set instead; got %q", out)
			}
		})
	}
}

// A configured sink must not be told it is going nowhere — a false warning on
// a working deployment trains people to ignore the real one.
func TestNew_ConfiguredExporterIsNotWarnedAbout(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	exp, err := New(config.ExporterConfig{
		Type: "otlp_http", Endpoint: "http://127.0.0.1:4318", QueueSize: 4,
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer exp.Close()

	if strings.Contains(buf.String(), "sent NOWHERE") {
		t.Errorf("an otlp_http exporter was warned about as if it were stdout: %q", buf.String())
	}
}
