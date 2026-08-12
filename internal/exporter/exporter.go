// Package exporter takes normalized collector.Envelope values and writes
// them somewhere. Kept separate from collection so adding a new sink
// (e.g. push to OneAgent's ingestion bus) never touches collector code.
package exporter

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/oneagent/agent/internal/collector"
	"github.com/oneagent/agent/internal/config"
)

// Exporter writes a single envelope to its configured sink.
type Exporter interface {
	Export(e collector.Envelope) error
	Close() error
}

// New constructs the exporter named in the config. Returns an error for an
// unknown type rather than silently defaulting — a typo'd config value
// (e.g. "htpp") should fail fast at startup, not silently drop all data.
func New(cfg config.ExporterConfig) (Exporter, error) {
	switch cfg.Type {
	case "stdout", "":
		return &stdoutExporter{}, nil
	case "file":
		return newFileExporter(cfg.Path)
	case "http":
		return newHTTPExporter(cfg)
	case "otlp_http":
		return newOTLPHTTPExporter(cfg)
	default:
		return nil, fmt.Errorf("unknown exporter type %q", cfg.Type)
	}
}

type stdoutExporter struct{}

func (s *stdoutExporter) Export(e collector.Envelope) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}
func (s *stdoutExporter) Close() error { return nil }

type fileExporter struct {
	f *os.File
}

func newFileExporter(path string) (*fileExporter, error) {
	if path == "" {
		return nil, fmt.Errorf("exporter type 'file' requires 'path' to be set")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("opening export file %s: %w", path, err)
	}
	return &fileExporter{f: f}, nil
}

func (fe *fileExporter) Export(e collector.Envelope) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = fe.f.Write(append(b, '\n'))
	return err
}
func (fe *fileExporter) Close() error { return fe.f.Close() }

// httpExporter batches envelopes, gzip-compresses each batch, and POSTs
// with exponential-backoff retry on transient failures. Batching+gzip
// matters because a single-envelope-per-request exporter (the original
// implementation) would mean one HTTP round trip per metric sample —
// fine for a demo, but it's the first thing that falls over under real
// telemetry volume (thousands of envelopes/minute per host).
type httpExporter struct {
	endpoint      string
	client        *http.Client
	batchSize     int
	flushInterval time.Duration
	maxRetries    int

	mu      sync.Mutex
	buf     []collector.Envelope
	stopCh  chan struct{}
	flushWg sync.WaitGroup
}

func newHTTPExporter(cfg config.ExporterConfig) (*httpExporter, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("exporter type 'http' requires 'endpoint' to be set")
	}
	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = 100
	}
	flushInterval := cfg.FlushInterval
	if flushInterval <= 0 {
		flushInterval = 5 * time.Second
	}
	maxRetries := cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}

	h := &httpExporter{
		endpoint:      cfg.Endpoint,
		client:        &http.Client{Timeout: 10 * time.Second},
		batchSize:     batchSize,
		flushInterval: flushInterval,
		maxRetries:    maxRetries,
		stopCh:        make(chan struct{}),
	}

	h.flushWg.Add(1)
	go h.flushLoop()

	return h, nil
}

// flushLoop guarantees a bounded-latency flush for low-throughput periods:
// without it, a batch that never reaches batchSize would sit buffered
// indefinitely and never actually get exported.
func (h *httpExporter) flushLoop() {
	defer h.flushWg.Done()
	ticker := time.NewTicker(h.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-h.stopCh:
			return
		case <-ticker.C:
			_ = h.flush()
		}
	}
}

func (h *httpExporter) Export(e collector.Envelope) error {
	h.mu.Lock()
	h.buf = append(h.buf, e)
	shouldFlush := len(h.buf) >= h.batchSize
	h.mu.Unlock()

	if shouldFlush {
		return h.flush()
	}
	return nil
}

func (h *httpExporter) flush() error {
	h.mu.Lock()
	if len(h.buf) == 0 {
		h.mu.Unlock()
		return nil
	}
	batch := h.buf
	h.buf = nil
	h.mu.Unlock()

	payload, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("marshaling batch of %d envelopes: %w", len(batch), err)
	}

	compressed, err := gzipCompress(payload)
	if err != nil {
		return fmt.Errorf("compressing batch: %w", err)
	}

	return h.postWithRetry(compressed, len(batch))
}

// postWithRetry sends one POST attempt, retrying with exponential backoff
// (plus jitter, to avoid a thundering herd of agents retrying in lockstep
// after a shared backend blip) on 5xx responses and network errors. 4xx
// responses are NOT retried — a malformed batch will fail identically on
// every retry, so retrying just delays surfacing the real problem.
func (h *httpExporter) postWithRetry(compressed []byte, batchLen int) error {
	var lastErr error
	for attempt := 0; attempt <= h.maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * 200 * time.Millisecond
			jitter := time.Duration(rand.Int63n(int64(backoff) / 2))
			time.Sleep(backoff + jitter)
		}

		req, err := http.NewRequest(http.MethodPost, h.endpoint, bytes.NewReader(compressed))
		if err != nil {
			return fmt.Errorf("building export request: %w", err) // malformed endpoint URL — retrying won't help
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Content-Encoding", "gzip")

		resp, err := h.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("exporting batch of %d (attempt %d/%d): %w", batchLen, attempt+1, h.maxRetries+1, err)
			continue
		}
		func() {
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body) // drain so the connection can be reused
		}()

		if resp.StatusCode < 300 {
			return nil
		}
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return fmt.Errorf("exporter http %d from %s (batch of %d, not retrying — client error)", resp.StatusCode, h.endpoint, batchLen)
		}
		lastErr = fmt.Errorf("exporter http %d from %s (attempt %d/%d)", resp.StatusCode, h.endpoint, attempt+1, h.maxRetries+1)
	}
	return lastErr
}

func (h *httpExporter) Close() error {
	close(h.stopCh)
	h.flushWg.Wait()
	return h.flush() // ship whatever was still buffered rather than dropping it on shutdown
}

func gzipCompress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
