package ingest

import (
	"compress/gzip"
	"crypto/subtle"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/agent-i/agent/internal/otlpwire"
)

// Handler serves the OTLP/HTTP ingest endpoints.
//
// The paths are OTLP's own — /v1/metrics, /v1/logs, /v1/traces — so any
// OTLP-native producer can send here, not only this project's agent. That is
// most of the value of having chosen OTLP as the wire format: the backend is
// not tied to the collector in front of it.
type Handler struct {
	writer  *Writer
	keys    map[string]string // key -> label, for logging which tenant sent what
	maxBody int64
	now     func() time.Time
}

// Options configure the handler.
type Options struct {
	// APIKeys maps an accepted key to a human label. Empty disables
	// authentication entirely, which is the right default for a local
	// docker-compose and the wrong one for anything reachable — the server
	// logs a warning in that case rather than silently accepting the world.
	APIKeys map[string]string
	// MaxBodyBytes caps a single export. Matches the agent's own receiver
	// default; a producer that exceeds it is misconfigured rather than busy,
	// since the agent batches to stay well under.
	MaxBodyBytes int64
}

const defaultMaxBody = 4 << 20

func NewHandler(w *Writer, opts Options) *Handler {
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = defaultMaxBody
	}
	return &Handler{
		writer:  w,
		keys:    opts.APIKeys,
		maxBody: opts.MaxBodyBytes,
		now:     time.Now,
	}
}

// Routes registers the ingest endpoints on mux.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/metrics", h.signal("metrics"))
	mux.HandleFunc("/v1/logs", h.signal("logs"))
	mux.HandleFunc("/v1/traces", h.signal("traces"))
}

func (h *Handler) signal(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !h.authorized(r) {
			// 401 with no detail. Saying which part of the credential was
			// wrong is a hint to whoever is guessing.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		body, err := h.read(r)
		if err != nil {
			// 413 is retryable by nobody and must not look like a transient
			// failure, or a producer will resend the same oversized payload
			// forever.
			status := http.StatusBadRequest
			if strings.Contains(err.Error(), "too large") {
				status = http.StatusRequestEntityTooLarge
			}
			http.Error(w, err.Error(), status)
			return
		}

		batch, err := h.decode(kind, r, body)
		if err != nil {
			// A malformed export fails identically on every retry, so this is
			// a 400 rather than a 500 — the distinction is what stops a
			// producer retrying a payload that can never succeed.
			log.Printf("ingest: rejecting %s export: %v", kind, err)
			http.Error(w, fmt.Sprintf("decoding %s: %v", kind, err), http.StatusBadRequest)
			return
		}

		h.writer.Add(batch)

		// 200 with an empty JSON object is what OTLP/HTTP defines as success
		// for a full export. It is returned once the rows are buffered, not
		// once they are in ClickHouse, and that is deliberate: the alternative
		// makes every producer's export latency a function of the database's
		// merge queue. The agent's own spool is what covers the gap, since it
		// keeps its copy until this returns.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}
}

// decode picks the wire format and produces rows.
//
// Both OTLP encodings are accepted, and both matter: this project's own agent
// exports JSON, while most third-party SDKs and collectors default to
// protobuf. They converge on the same otlpwire types, so the row builders and
// their tests never see the difference.
//
// The content type decides. OTLP defines application/json and
// application/x-protobuf; anything else is assumed to be protobuf, because
// that is the OTLP default and a producer that omits the header is far more
// likely to be sending protobuf than to be sending JSON without saying so.
func (h *Handler) decode(kind string, r *http.Request, body []byte) (Batch, error) {
	now := h.now()
	isJSON := strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "json")

	switch kind {
	case "metrics":
		req, err := decodeWith(isJSON, body, UnmarshalJSONMetrics, otlpwire.UnmarshalExportMetricsServiceRequest)
		if err != nil {
			return Batch{}, err
		}
		return Metrics(req, now), nil

	case "logs":
		req, err := decodeWith(isJSON, body, UnmarshalJSONLogs, otlpwire.UnmarshalExportLogsServiceRequest)
		if err != nil {
			return Batch{}, err
		}
		return Logs(req, now), nil

	case "traces":
		req, err := decodeWith(isJSON, body, UnmarshalJSONTraces, otlpwire.UnmarshalExportTraceServiceRequest)
		if err != nil {
			return Batch{}, err
		}
		return Spans(req, now), nil
	}
	return Batch{}, fmt.Errorf("unknown signal %q", kind)
}

// decodeWith picks one of the two decoders. Generic so the choice is made once
// rather than repeated per signal, where the two branches could drift.
func decodeWith[T any](isJSON bool, body []byte, fromJSON, fromProto func([]byte) (*T, error)) (*T, error) {
	if isJSON {
		return fromJSON(body)
	}
	return fromProto(body)
}

// read pulls the body, transparently decompressing gzip.
//
// The limit is applied to the DECOMPRESSED stream, not the request. A gzip
// bomb is small on the wire and enormous once expanded, so capping only what
// arrived would be capping the wrong number.
func (h *Handler) read(r *http.Request) ([]byte, error) {
	var src io.Reader = http.MaxBytesReader(nil, r.Body, h.maxBody*20)

	if strings.Contains(r.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(src)
		if err != nil {
			return nil, fmt.Errorf("bad gzip body: %w", err)
		}
		defer zr.Close()
		src = zr
	}

	body, err := io.ReadAll(io.LimitReader(src, h.maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("reading body: %w", err)
	}
	if int64(len(body)) > h.maxBody {
		return nil, fmt.Errorf("body too large (limit %d bytes)", h.maxBody)
	}
	return body, nil
}

// authorized checks the API key.
//
// Accepts either an Authorization: Bearer header or the x-api-key header that
// most telemetry backends use, because the agent's headers_env config can set
// whichever one a given deployment already standardised on.
func (h *Handler) authorized(r *http.Request) bool {
	if len(h.keys) == 0 {
		return true // authentication disabled; the server warns about this at startup
	}
	presented := strings.TrimSpace(r.Header.Get("x-api-key"))
	if presented == "" {
		if v := r.Header.Get("Authorization"); strings.HasPrefix(v, "Bearer ") {
			presented = strings.TrimSpace(strings.TrimPrefix(v, "Bearer "))
		}
	}
	if presented == "" {
		return false
	}
	// Constant-time comparison against every key. Comparing with == would
	// leak, through timing, how much of a guess was correct — and iterating
	// with an early return would leak which keys exist.
	ok := false
	for key := range h.keys {
		if subtle.ConstantTimeCompare([]byte(key), []byte(presented)) == 1 {
			ok = true
		}
	}
	return ok
}
