// Package exporter takes normalized collector.Envelope values and writes
// them somewhere. Kept separate from collection so adding a new sink
// (e.g. push to Agent-I's ingestion bus) never touches collector code.
package exporter

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/agent-i/agent/internal/collector"
	"github.com/agent-i/agent/internal/config"
)

// Exporter writes a single envelope to its configured sink.
type Exporter interface {
	Export(e collector.Envelope) error
	Close() error
}

// New constructs the exporter named in the config. Returns an error for an
// unknown type rather than silently defaulting — a typo'd config value
// (e.g. "htpp") should fail fast at startup, not silently drop all data.
//
// retire, if non-nil, is called once per envelope at the moment its fate is
// settled: delivered, or deliberately discarded to make room. It is how a
// tailed file learns that a line no longer needs to be re-read after a restart.
// It is deliberately NOT called when a send fails, because a failed line is
// exactly the one worth reading again. It may be called from the sender
// goroutine, so it must be safe for concurrent use and must not block.
func New(cfg config.ExporterConfig, retire func(collector.Envelope)) (Exporter, error) {
	switch cfg.Type {
	case "stdout", "":
		// Said out loud, because this is the configuration that looks most like
		// a working one and is not.
		//
		// stdout is the shipped default, so a fresh install collects correctly,
		// reports healthy, prints every envelope — and delivers nothing. On a
		// systemd host those envelopes go to the journal and are discarded by
		// its rotation, so the evidence that it was working is also the reason
		// nobody notices it is not. Diagnosing it means reading the journal
		// closely enough to realise the JSON in it IS the telemetry rather than
		// a debug trace of it, which is not an obvious leap.
		//
		// A warning rather than an error: stdout is genuinely the right choice
		// for a local run or a container you are watching, and refusing to
		// start would break that. But it should never be silent, which is the
		// same rule the spool applies a few lines below.
		log.Printf("exporter: type is %q — telemetry is written to this process's standard output and "+
			"sent NOWHERE. On a systemd host it lands in the journal and is rotated away. Set "+
			"exporter.type to \"otlp_http\" with an endpoint to ship it to a backend.",
			exporterTypeLabel(cfg.Type))
		return withRetire(&stdoutExporter{}, retire), nil
	case "file":
		f, err := newFileExporter(cfg.Path)
		if err != nil {
			return nil, err
		}
		return withRetire(f, retire), nil
	case "http":
		// Network exporters are wrapped so delivery happens off the
		// collectors' goroutines — see async.go for why that matters. stdout
		// and file are left synchronous: they are local, fast, and tests rely
		// on their output being deterministic. They also have no use for a
		// spool, since the sink they write to IS the disk.
		h, err := newHTTPExporter(cfg)
		if err != nil {
			return nil, err
		}
		sp, err := openConfiguredSpool(cfg, retire)
		if err != nil {
			return nil, err
		}
		return newAsyncExporter(h, cfg.QueueSize, cfg.ShutdownTimeout, retire, sp), nil
	case "otlp_http":
		o, err := newOTLPHTTPExporter(cfg)
		if err != nil {
			return nil, err
		}
		sp, err := openConfiguredSpool(cfg, retire)
		if err != nil {
			return nil, err
		}
		return newAsyncExporter(o, cfg.QueueSize, cfg.ShutdownTimeout, retire, sp), nil
	default:
		return nil, fmt.Errorf("unknown exporter type %q", cfg.Type)
	}
}

// exporterTypeLabel names the type in a message, distinguishing a deliberate
// "stdout" from an absent value that defaulted to it — the second is worth
// separating because the operator never chose it.
func exporterTypeLabel(t string) string {
	if t == "" {
		return "stdout (unset)"
	}
	return t
}

// openConfiguredSpool builds the disk spool, if one is wanted.
//
// The failure handling is deliberately asymmetric. A spool_dir written in the
// config is a promise the operator made about where this agent keeps its data,
// and quietly ignoring an unusable one would leave them believing an outage is
// survivable when it is not — so that is a startup error, in keeping with how
// this package already treats a typo'd exporter type. The default path is a
// guess we made on their behalf, and an agent that refuses to start because it
// could not create /var/lib/agent-i/spool (unprivileged run, read-only root)
// would be worse than one that collects with the pre-spool durability. So that
// case warns loudly and carries on.
func openConfiguredSpool(cfg config.ExporterConfig, retire func(collector.Envelope)) (*spool, error) {
	if !cfg.Spool.SpoolEnabled() {
		return nil, nil
	}
	sp, err := openSpool(spoolOptions{
		Dir:          cfg.Spool.Dir,
		MaxBytes:     cfg.Spool.MaxBytes,
		SegmentBytes: cfg.Spool.SegmentBytes,
		SyncInterval: cfg.Spool.SyncInterval,
		Retire:       retire,
	})
	if err == nil {
		return sp, nil
	}
	if cfg.Spool.SpoolRequired() {
		return nil, fmt.Errorf("opening exporter spool: %w", err)
	}
	log.Printf("exporter: no spool (%v) — envelopes will be dropped oldest-first during an outage and lost on restart; set exporter.spool.dir to a writable path to keep them", err)
	return nil, nil
}

// retiringExporter reports the fate of an envelope for the synchronous sinks,
// where "delivered" is simply "Export returned nil". The asynchronous path
// cannot use this because its Export returns as soon as the envelope is queued,
// which says nothing about whether it was sent.
type retiringExporter struct {
	inner  Exporter
	retire func(collector.Envelope)
}

func withRetire(inner Exporter, retire func(collector.Envelope)) Exporter {
	if retire == nil {
		return inner
	}
	return &retiringExporter{inner: inner, retire: retire}
}

func (r *retiringExporter) Export(e collector.Envelope) error {
	if err := r.inner.Export(e); err != nil {
		return err
	}
	r.retire(e)
	return nil
}

func (r *retiringExporter) Close() error { return r.inner.Close() }

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
	endpoint string
	// headers are sent on every POST — typically an ingestion key resolved
	// from the environment by the daemon. These were previously accepted in
	// config, documented as applying to this exporter, and then never read,
	// so any endpoint requiring authentication rejected every batch while the
	// config looked correct.
	headers       map[string]string
	client        *http.Client
	batchSize     int
	maxBatchBytes int
	flushInterval time.Duration
	maxRetries    int

	mu sync.Mutex
	// bufBytes is the running size estimate for buf, kept on append so the
	// flush decision is free. See batch.go for why the count limit alone is
	// not enough.
	buf      []collector.Envelope
	bufBytes int
	stopCh   chan struct{}
	flushWg  sync.WaitGroup
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
		headers:       cfg.Headers,
		client:        &http.Client{Timeout: 10 * time.Second},
		batchSize:     batchSize,
		maxBatchBytes: resolveMaxBatchBytes(cfg.MaxBatchBytes),
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
	h.bufBytes += envelopeBytes(e)
	// Either limit fills the batch. An envelope bigger than the cap on its own
	// flushes alone rather than being dropped — a record cannot be split.
	shouldFlush := len(h.buf) >= h.batchSize || h.bufBytes >= h.maxBatchBytes
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
	h.bufBytes = 0
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
		for k, v := range h.headers {
			req.Header.Set(k, v)
		}

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

// ResourceRefresher is implemented by exporters whose host attributes are not
// necessarily final when the process starts.
//
// It exists for IMDS: a boot can have the agent running before the network
// stack answers, and the instance identity discovered a few seconds later is
// still the right identity for everything sent afterwards. Without a way to
// publish it, a host that lost that race reported no instance attributes until
// somebody restarted the agent.
//
// Optional by design — stdout and file exporters have no resource to refresh —
// so callers type-assert rather than depending on every Exporter to implement
// it.
type ResourceRefresher interface {
	SetResourceAttributes(map[string]string)
}

// SetResourceAttributes forwards to the wrapped exporter when it supports it,
// so the wrapping does not hide the capability from the daemon.
func (r *retiringExporter) SetResourceAttributes(attrs map[string]string) {
	if inner, ok := r.inner.(ResourceRefresher); ok {
		inner.SetResourceAttributes(attrs)
	}
}
