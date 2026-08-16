package dashboard

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// index.html is embedded rather than installed alongside the binary so the
// dashboard cannot break by being deployed without its assets — the agent
// is shipped as a single static binary and this keeps that true.
//
//go:embed index.html
var indexHTML []byte

// Server exposes the store over HTTP on the loopback interface.
type Server struct {
	store *Store
	srv   *http.Server
	ln    net.Listener
	addr  string
	// authToken is empty unless the operator sets one, and empty means no
	// authentication at all — the historical behaviour, unchanged.
	authToken string
}

// NewServer binds the listener immediately so a port conflict is reported
// at startup, next to the rest of the agent's initialization, rather than
// silently in a goroutine after Run has already reported success.
//
// authToken is optional. When empty the handlers are registered exactly as
// they always were, with no auth layer in the request path at all; when set,
// the data-bearing routes require "Authorization: Bearer <token>".
func NewServer(addr string, store *Store, authToken string) (*Server, error) {
	if addr == "" {
		addr = "127.0.0.1:8088"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dashboard: listening on %s: %w", addr, err)
	}
	if !isLoopbackAddr(addr) {
		// Deliberately a warning, not an error: binding elsewhere is a
		// legitimate choice on a trusted network. Off loopback the endpoint
		// publishes this host's metrics, logs and traces to anything that can
		// reach the port — unless a token is set, which is the one thing that
		// makes that binding defensible.
		if authToken == "" {
			log.Printf("dashboard: WARNING listening on %s, which is not loopback — this endpoint is unauthenticated", addr)
		} else {
			log.Printf("dashboard: listening on %s (not loopback) with bearer auth required", addr)
		}
	}

	s := &Server{store: store, ln: ln, addr: addr, authToken: authToken}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.guard(s.handleIndex))
	mux.HandleFunc("/api/snapshot", s.guard(s.handleSnapshot))
	// Deliberately NOT guarded. It returns the literal string "ok" and no host
	// data, and it is what liveness probes and the UI's own connection check
	// call — requiring a credential here would break monitoring to protect
	// nothing.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})

	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return s, nil
}

// Addr is the address actually bound, which differs from the requested one
// when the config asks for port 0 (tests do this to avoid port collisions).
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Serve blocks until the server is closed. Callers run it in a goroutine.
func (s *Server) Serve() {
	if err := s.srv.Serve(s.ln); err != nil && err != http.ErrServerClosed {
		log.Printf("dashboard: server stopped: %v", err)
	}
}

func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}

// guard applies the optional bearer check.
//
// When no token is configured it returns the handler itself — not a wrapper
// that always passes. That keeps the default path byte-for-byte what it was:
// no extra frame, no header read, nothing to get wrong on the request path
// every 5 seconds.
func (s *Server) guard(h http.HandlerFunc) http.HandlerFunc {
	if s.authToken == "" {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="agent-i"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// authorized mirrors the OTLP receiver's check so both listeners behave the
// same way. Constant-time comparison: a caller must not be able to recover the
// token by measuring how long a rejection takes.
func (s *Server) authorized(r *http.Request) bool {
	if s.authToken == "" {
		return true
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(h, prefix)), []byte(s.authToken)) == 1
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page reads only from this same origin and loads no remote assets;
	// the CSP states that so a stray external reference fails loudly during
	// development instead of silently working on a dev machine with internet
	// and failing on an air-gapped host.
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
	_, _ = w.Write(indexHTML)
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	snap := s.store.Snapshot()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(snap); err != nil {
		log.Printf("dashboard: encoding snapshot: %v", err)
	}
}

// isLoopbackAddr reports whether a listen address is bound to the local
// machine only. A bare port or an empty host means "all interfaces", which
// is not loopback.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
