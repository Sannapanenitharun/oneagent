package dashboard

import (
	"context"
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
}

// NewServer binds the listener immediately so a port conflict is reported
// at startup, next to the rest of the agent's initialization, rather than
// silently in a goroutine after Run has already reported success.
func NewServer(addr string, store *Store) (*Server, error) {
	if addr == "" {
		addr = "127.0.0.1:8088"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dashboard: listening on %s: %w", addr, err)
	}
	if !isLoopbackAddr(addr) {
		// Deliberately a warning, not an error: binding elsewhere is a
		// legitimate choice on a trusted network. But the dashboard has no
		// authentication, so exposing it off loopback publishes this host's
		// metrics, logs and traces to anything that can reach the port.
		log.Printf("dashboard: WARNING listening on %s, which is not loopback — this endpoint is unauthenticated", addr)
	}

	s := &Server{store: store, ln: ln, addr: addr}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/snapshot", s.handleSnapshot)
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
