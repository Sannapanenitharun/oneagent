// Command server is the central backend: it receives OTLP from every agent,
// stores it in ClickHouse, and answers the dashboard's queries.
//
// One binary serving both roles rather than two. They will want to scale
// differently eventually — ingest with the fleet, queries with the number of
// people looking — but splitting them now would mean two deployments, two
// configs and a shared schema to keep in step, for a separation nothing is yet
// asking for. The packages are already separate, so the split stays cheap.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agent-i/agent/internal/ingest"
	"github.com/agent-i/agent/internal/store"
	"github.com/agent-i/agent/internal/version"
)

func main() {
	var (
		listen     = flag.String("listen", envOr("AGENTI_LISTEN", "0.0.0.0:4318"), "address to serve OTLP ingest and the query API on")
		chEndpoint = flag.String("clickhouse", envOr("AGENTI_CLICKHOUSE", "http://127.0.0.1:8123"), "ClickHouse HTTP endpoint")
		chDatabase = flag.String("database", envOr("AGENTI_CLICKHOUSE_DB", "agenti"), "ClickHouse database")
		chUser     = flag.String("user", envOr("AGENTI_CLICKHOUSE_USER", "default"), "ClickHouse user")
		batchRows  = flag.Int("batch-rows", envIntOr("AGENTI_BATCH_ROWS", 10000), "rows buffered before a flush")
		batchWait  = flag.Duration("batch-interval", 2*time.Second, "maximum time rows wait before a flush")
		maxSeries  = flag.Int("max-series", envIntOr("AGENTI_MAX_SERIES", 0), "distinct series one host's snapshot may carry (0 uses the default of 400)")
		showVer    = flag.Bool("version", false, "print the build version and exit")
	)
	flag.Parse()

	if *showVer {
		log.SetFlags(0)
		log.Println(version.Version)
		return
	}

	// Secrets come from the environment, never from a flag: a flag is visible
	// in ps output to every user on the box. Same rule the agent applies to
	// its exporter headers.
	chPassword := os.Getenv("AGENTI_CLICKHOUSE_PASSWORD")
	apiKeys := parseAPIKeys(os.Getenv("AGENTI_API_KEYS"))

	db, err := store.New(store.Config{
		Endpoint: *chEndpoint,
		Database: *chDatabase,
		User:     *chUser,
		Password: chPassword,

		MaxSeriesPerHost: *maxSeries,
	})
	if err != nil {
		log.Fatalf("clickhouse: %v", err)
	}

	// Wait for the database rather than exiting. Under docker-compose or
	// Kubernetes this process routinely starts before ClickHouse is accepting
	// connections, and a crash loop that resolves itself is noise in the logs
	// of every deployment that ever restarts both together.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := waitForDB(ctx, db); err != nil {
		log.Fatalf("clickhouse at %s: %v", *chEndpoint, err)
	}
	if err := db.Migrate(ctx); err != nil {
		log.Fatalf("clickhouse: applying schema: %v", err)
	}
	log.Printf("clickhouse: connected to %s, schema up to date", *chEndpoint)
	log.Printf("api: snapshots carry at most %d series per host", db.MaxSeriesPerHost())

	writer := ingest.NewWriter(db, *batchRows, *batchWait)
	handler := ingest.NewHandler(writer, ingest.Options{APIKeys: apiKeys})

	if len(apiKeys) == 0 {
		// Loud, because the default is the permissive one. It is the right
		// default for a laptop and the wrong one for anything with a route to
		// it, and the difference is invisible from the outside.
		log.Printf("WARNING: no API keys configured (AGENTI_API_KEYS is unset) — this server accepts telemetry from anyone who can reach %s", *listen)
	} else {
		log.Printf("ingest: %d API key(s) configured", len(apiKeys))
	}

	mux := http.NewServeMux()
	handler.Routes(mux)
	registerQueryAPI(mux, db)

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		// Reports the dependency, not just the process. A health check that
		// only proves this binary is running would keep a server in a load
		// balancer while every write it accepts is failing.
		if err := db.Ping(r.Context()); err != nil {
			http.Error(w, "clickhouse unreachable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:    *listen,
		Handler: mux,
		// An agent that opens a connection and sends nothing must not hold a
		// slot indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Printf("server %s listening on %s (OTLP ingest at /v1/{metrics,logs,traces}, API at /api/)", version.Version, *listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")

	// Stop accepting first, then flush. The other order would drop whatever
	// arrived during the flush.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	writer.Close(shutdownCtx)
	log.Println("stopped")
}

// waitForDB retries until ClickHouse answers or the context ends.
func waitForDB(ctx context.Context, db *store.Client) error {
	const attemptTimeout = 3 * time.Second
	var lastErr error
	for attempt := 0; ; attempt++ {
		probe, cancel := context.WithTimeout(ctx, attemptTimeout)
		err := db.Ping(probe)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt == 0 {
			log.Printf("clickhouse: not ready yet (%v) — retrying", err)
		}
		select {
		case <-ctx.Done():
			return lastErr
		case <-time.After(2 * time.Second):
		}
	}
}

// registerQueryAPI mounts the endpoints the dashboard reads.
func registerQueryAPI(mux *http.ServeMux, db *store.Client) {
	mux.HandleFunc("/api/hosts", func(w http.ResponseWriter, r *http.Request) {
		hosts, err := db.Hosts(r.Context(), durationParam(r, "window", 10*time.Minute))
		if err != nil {
			apiError(w, err)
			return
		}
		writeJSON(w, map[string]any{"hosts": hosts, "now": time.Now().UTC().UnixMilli()})
	})

	mux.HandleFunc("/api/series", func(w http.ResponseWriter, r *http.Request) {
		host := r.URL.Query().Get("host")
		name := r.URL.Query().Get("name")
		if host == "" || name == "" {
			http.Error(w, "host and name are required", http.StatusBadRequest)
			return
		}
		points, err := db.Series(r.Context(), host, name,
			durationParam(r, "window", time.Hour), durationParam(r, "step", 15*time.Second))
		if err != nil {
			apiError(w, err)
			return
		}
		writeJSON(w, map[string]any{"host": host, "name": name, "points": points})
	})

	// One host's whole window, in the agent's own payload shape.
	//
	// The same shape on purpose: the dashboard's logs view, trace waterfall,
	// flame graph, service map and every percentile in front of them are
	// written against it, and a second shape would mean all of that existing
	// twice. A host the browser has no route to renders through exactly the
	// code that renders one it does.
	mux.HandleFunc("/api/snapshot", func(w http.ResponseWriter, r *http.Request) {
		host := r.URL.Query().Get("host")
		if host == "" {
			http.Error(w, "host is required", http.StatusBadRequest)
			return
		}
		snap, err := db.Snapshot(r.Context(), host, durationParam(r, "window", 15*time.Minute))
		if err != nil {
			apiError(w, err)
			return
		}
		store.SortSeries(snap.Series)
		writeJSON(w, snap)
	})

	mux.HandleFunc("/api/metrics/names", func(w http.ResponseWriter, r *http.Request) {
		host := r.URL.Query().Get("host")
		if host == "" {
			http.Error(w, "host is required", http.StatusBadRequest)
			return
		}
		names, err := db.MetricNames(r.Context(), host, durationParam(r, "window", time.Hour))
		if err != nil {
			apiError(w, err)
			return
		}
		writeJSON(w, map[string]any{"host": host, "names": names})
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("api: writing response: %v", err)
	}
}

// apiError reports a query failure as a 500 with the database's own message.
// The alternative — a bare "internal error" — turns every schema or type
// problem into an unactionable page.
func apiError(w http.ResponseWriter, err error) {
	log.Printf("api: %v", err)
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// durationParam reads a window like "15m" or a bare number of seconds, because
// both are things people put in a URL by hand.
func durationParam(r *http.Request, key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return fallback
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(raw); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return fallback
}

// parseAPIKeys reads "label:key,label:key" or a bare comma-separated list.
func parseAPIKeys(spec string) map[string]string {
	out := map[string]string{}
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		label, key := "", entry
		if i := strings.Index(entry, ":"); i > 0 {
			label, key = entry[:i], strings.TrimSpace(entry[i+1:])
		}
		if key != "" {
			out[key] = label
		}
	}
	return out
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envIntOr(key string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}
