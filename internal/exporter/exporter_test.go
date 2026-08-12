package exporter

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oneagent/agent/internal/collector"
	"github.com/oneagent/agent/internal/config"
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
