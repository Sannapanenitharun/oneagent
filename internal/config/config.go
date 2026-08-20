// Package config loads the agent's YAML configuration. Kept dependency-light
// (no third-party libraries at all) so the binary stays small and auditable.
package config

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/agent-i/agent/internal/yamlmin"
)

// Config is the root configuration for the agent daemon.
type Config struct {
	// AgentID names this agent in every signal it emits. Optional: left empty,
	// the daemon derives one at startup from what the machine actually is —
	// see daemon.resolveAgentID. It is not defaulted here because Load must
	// stay a pure function of the file, and any useful default depends on the
	// host rather than the configuration.
	AgentID    string          `yaml:"agent_id"`
	Interval   time.Duration   `yaml:"interval"`
	Metrics    MetricsConfig   `yaml:"metrics"`
	Logs       LogsConfig      `yaml:"logs"`
	AccessLogs AccessLogConfig `yaml:"access_logs"`
	Tailing    TailingConfig   `yaml:"tailing"`
	Traces     TracesConfig    `yaml:"traces"`
	Exporter   ExporterConfig  `yaml:"exporter"`

	Aggregation AggregationConfig `yaml:"aggregation"`
	Dashboard   DashboardConfig   `yaml:"dashboard"`
	EC2Metadata EC2MetadataConfig `yaml:"ec2_metadata"`
}

// EC2MetadataConfig controls the Instance Metadata Service probe that labels
// telemetry with the EC2 instance it came from.
//
// This is host self-description, not a cloud API integration: IMDS is a
// link-local address reachable only from the instance itself, it answers only
// about that instance, and it needs no credentials and no IAM permission. There
// is nothing to configure beyond whether to ask.
type EC2MetadataConfig struct {
	// Enabled is a pointer so that an absent block means on. Detection is
	// harmless off EC2 — it fails fast and the agent carries on — and having
	// it default off would mean every EC2 user has to discover the setting
	// before their hosts are identifiable.
	Enabled *bool `yaml:"enabled"`
	// Timeout bounds the whole probe. Kept short because it runs at startup on
	// every host, and a non-AWS machine may blackhole the link-local address
	// rather than refusing.
	Timeout time.Duration `yaml:"timeout"`
}

// DetectionEnabled reports whether to probe, treating an unset value as true.
func (c EC2MetadataConfig) DetectionEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// DashboardConfig controls the agent's built-in local web view. It serves
// only from memory and sends nothing anywhere — it is a window onto what
// this agent is collecting right now, for answering "is it working?" on the
// host instead of waiting for data to surface in a remote backend.
type DashboardConfig struct {
	Enabled bool `yaml:"enabled"`
	// ListenAddr defaults to loopback deliberately. The endpoint has no
	// authentication and exposes this host's metrics, logs and trace
	// contents, so binding it to a reachable interface publishes all of
	// that; the agent logs a warning if you do.
	ListenAddr string `yaml:"listen_addr"`
	// Retain bounds how much history is held in memory. This is a debug
	// window, not storage — the real retention lives in whatever backend
	// the exporter ships to.
	Retain time.Duration `yaml:"retain"`
	// MaxSeries caps distinct (metric, label-set) combinations held. Once
	// reached, new series are refused rather than evicting existing ones,
	// and the dashboard says so instead of quietly showing a subset.
	MaxSeries int `yaml:"max_series"`
}

// AggregationConfig controls summarising per-request access log events into
// per-interval metrics instead of shipping one record per HTTP request.
//
// Defaults to disabled: enabling it changes what an existing deployment
// receives (metrics rather than individual request records), and that is a
// decision for the operator rather than a silent upgrade side effect.
type AggregationConfig struct {
	Enabled bool `yaml:"enabled"`
	// Interval is the summary window, independent of the collection interval.
	Interval time.Duration `yaml:"interval"`
	// MaxContexts caps distinct (method, path, status) combinations per
	// window; beyond it everything collapses into one visible overflow bucket.
	MaxContexts int `yaml:"max_contexts"`
	// MaxSamples caps retained latency samples per context, which is what
	// bounds memory while still giving usable percentiles.
	MaxSamples int `yaml:"max_samples_per_context"`
	// KeepRawEvents also forwards the original per-request envelopes. Turning
	// this on removes the volume benefit; it exists for the case where
	// per-request detail is genuinely required.
	KeepRawEvents bool `yaml:"keep_raw_events"`
}

// TailingConfig tunes file tailing for both logs and access_logs. It is shared
// rather than per-source because both collectors must use the same offset
// registry file — two registries writing the same path would clobber each
// other's offsets.
type TailingConfig struct {
	// ScanInterval is how often the configured globs are re-evaluated, which
	// is what picks up files created after startup and closes the gap after a
	// rotation the poll loop did not catch.
	ScanInterval time.Duration `yaml:"scan_interval"`
	// PollInterval is how often open files are read.
	PollInterval time.Duration `yaml:"poll_interval"`
	// MaxLineBytes caps a single line. A file with no newlines (a binary
	// accidentally matched by a glob) would otherwise buffer without bound.
	MaxLineBytes int `yaml:"max_line_bytes"`
	// RegistryPath stores per-file read offsets so a restart resumes where it
	// left off instead of skipping everything written while the agent was down.
	RegistryPath string `yaml:"registry_path"`
}

type MetricsConfig struct {
	Enabled bool `yaml:"enabled"`
	// Which host metrics to sample: cpu, memory, disk, network
	Collect []string `yaml:"collect"`
}

type LogsConfig struct {
	Enabled bool `yaml:"enabled"`
	// Paths to tail, e.g. /var/log/app/*.log
	Paths []string `yaml:"paths"`
	// Multiline joins continuation lines onto the record they belong to.
	Multiline MultilineConfig `yaml:"multiline"`
}

// MultilineConfig controls whether a log record may span several physical
// lines. Without it a Java stack trace or a Python traceback arrives as dozens
// of unrelated records with no way to reassemble them, which is the single most
// common complaint about naive log tailing.
type MultilineConfig struct {
	Enabled bool `yaml:"enabled"`

	// StartPattern is a regular expression matching the FIRST line of a record.
	// Anything that does not match is treated as a continuation of the record
	// before it. Anchor it with ^ — a timestamp or log level at the start of the
	// line is the usual choice, e.g. `^\d{4}-\d{2}-\d{2}` or `^(INFO|WARN|ERROR)`.
	//
	// Matching the start rather than the continuation is deliberate: what a
	// record begins with is predictable, whereas what a stack trace looks like
	// varies by language, framework and locale.
	StartPattern string `yaml:"start_pattern"`

	// MaxLines caps how many physical lines one record may absorb. A pattern
	// that never matches would otherwise let a single record grow without
	// bound. Reaching the cap emits what has accumulated, marked as truncated,
	// rather than discarding it.
	MaxLines int `yaml:"max_lines"`

	// Timeout is how long an unfinished record waits for more lines before
	// being emitted anyway. Without it the last record in a quiet file is held
	// forever, waiting for a successor that never comes — which is exactly the
	// stack trace you were looking for.
	Timeout time.Duration `yaml:"timeout"`
}

// AccessLogConfig configures parsing of incoming HTTP requests from an
// access log (nginx, Apache, or an app's own structured request log) —
// this is what answers "get API calls into this host" without requiring
// any code change in the monitored app itself.
type AccessLogConfig struct {
	Enabled bool     `yaml:"enabled"`
	Paths   []string `yaml:"paths"`
	Format  string   `yaml:"format"` // "combined" (nginx/Apache, default) or "json"

	// JSONFields is only used when format: json — lets the parser match
	// whatever field names the app's structured logger actually emits.
	// Any field left blank falls back to a sensible default (see
	// collector.JSONFieldMap.withDefaults).
	JSONFields struct {
		Method     string `yaml:"method"`
		Path       string `yaml:"path"`
		Status     string `yaml:"status"`
		DurationMs string `yaml:"duration_ms"`
		RemoteAddr string `yaml:"remote_addr"`
	} `yaml:"json_fields"`
}

type TracesConfig struct {
	Enabled bool `yaml:"enabled"`
	// ListenAddr is the address to listen on for OTLP span pushes from
	// instrumented apps. Default it to loopback: apps instrumented by
	// auto-instrument.sh always talk to localhost, and a receiver bound to
	// every interface accepts spans from anything that can reach the host.
	ListenAddr string `yaml:"listen_addr"`
	// MaxRequestBytes caps a single OTLP request body. Without it the protobuf
	// path reads an arbitrarily large body fully into memory before decoding.
	MaxRequestBytes int64 `yaml:"max_request_bytes"`
	// AuthTokenEnv names an environment variable holding a bearer token that
	// clients must present. Empty means no authentication, which is only
	// reasonable while ListenAddr is loopback.
	AuthTokenEnv string `yaml:"auth_token_env"`
	// AcceptLogs and AcceptMetrics control the other two OTLP signals on this
	// same listener. An SDK configured with OTEL_EXPORTER_OTLP_ENDPOINT sends
	// all three to one base URL, so accepting only traces silently 404s an
	// application's metrics and logs — which is why both default to true.
	//
	// They are pointers so an existing config that predates them is
	// distinguishable from one that sets them to false: a bare false is the
	// zero value of a bool and would silently disable both on upgrade. See
	// applyTraceSignalDefaults.
	AcceptLogs    *bool `yaml:"accept_logs"`
	AcceptMetrics *bool `yaml:"accept_metrics"`

	Sampling TraceSamplingConfig `yaml:"sampling"`
	Stats    TraceStatsConfig    `yaml:"stats"`
}

// TraceSamplingConfig controls how many spans are forwarded. Statistics (see
// TraceStatsConfig) are always computed over 100% of spans regardless of what
// is set here, so reducing volume does not make counts approximate.
type TraceSamplingConfig struct {
	Enabled bool `yaml:"enabled"`
	// Rate is the fraction of ordinary traces kept, 0.0-1.0. The decision is
	// made from the trace ID, so a trace is kept or dropped as a whole.
	Rate float64 `yaml:"rate"`
	// KeepErrors forwards error spans regardless of Rate. Pointer so that
	// leaving it unset means true — sampling errors at the same rate as
	// successes is almost never intended.
	KeepErrors *bool `yaml:"keep_errors"`
	// SlowThresholdMs forwards spans at least this slow regardless of Rate.
	// Zero disables the rule.
	SlowThresholdMs float64 `yaml:"slow_threshold_ms"`
}

// TraceStatsConfig controls RED metrics (count, errors, latency distribution)
// computed over every span before sampling.
type TraceStatsConfig struct {
	Enabled     bool          `yaml:"enabled"`
	Interval    time.Duration `yaml:"interval"`
	MaxContexts int           `yaml:"max_contexts"`
	MaxSamples  int           `yaml:"max_samples_per_context"`
}

type ExporterConfig struct {
	// "stdout" for local dev, "file" to append JSONL, "http" to push our
	// own JSON envelope format, "otlp_http" to push real OTLP (metrics/
	// traces/logs) to an OTLP-native backend
	Type     string `yaml:"type"`
	Path     string `yaml:"path"`
	Endpoint string `yaml:"endpoint"`

	// Headers are sent on every request from the http/otlp_http
	// exporters — e.g. an ingestion API key, whose header name is whatever
	// your backend documents.
	// SECURITY: put the actual
	// key in an environment variable and reference it via headers_env
	// below, not directly in this YAML file.
	Headers    map[string]string `yaml:"headers"`
	HeadersEnv map[string]string `yaml:"headers_env"` // header name -> env var name holding its value

	// ResourceAttributes are extra OTel resource attributes discovered at
	// runtime rather than configured — currently the EC2 instance identity.
	// Populated by the daemon before the exporter is built; never read from
	// YAML, which is why it is tagged "-".
	ResourceAttributes map[string]string `yaml:"-"`

	// HTTP exporter tuning — all optional, sensible defaults applied in
	// the exporter package if left zero.
	BatchSize     int           `yaml:"batch_size"`     // envelopes per POST before a size-triggered flush
	FlushInterval time.Duration `yaml:"flush_interval"` // max time a partial batch waits before flushing
	MaxRetries    int           `yaml:"max_retries"`    // retry attempts on 5xx/network error, not on 4xx
	// MaxBatchBytes caps the estimated encoded size of one POST. BatchSize
	// alone bounds the record COUNT, which says nothing about the bytes: 100
	// syslog lines and 100 joined stack traces are three orders of magnitude
	// apart. Without this an oversized request is rejected by the backend and
	// no retry can succeed, so the whole batch is lost. Defaults to 4 MiB —
	// the same limit this agent's own OTLP receiver enforces inbound.
	MaxBatchBytes int `yaml:"max_batch_bytes"`

	// QueueSize bounds the envelopes buffered between collection and delivery
	// for the network exporters. When it fills, the oldest are dropped and
	// counted rather than blocking collection — see exporter/async.go.
	QueueSize int `yaml:"queue_size"`
	// ShutdownTimeout bounds the final flush attempt on SIGTERM. An unbounded
	// flush against an unreachable backend turns a restart into an outage.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

// Load reads and validates a YAML config file. Fails loudly on malformed
// config rather than silently falling back to defaults — a mis-scoped agent
// silently no-op'ing on a customer host is worse than a crash on startup.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	var cfg Config
	if err := yamlmin.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	// agent_id is deliberately not required. It used to be, which meant the
	// shipped config had to carry a placeholder value for the agent to start at
	// all — and that placeholder was then the real, identical id of every host
	// installed from it. An empty value now means "work it out from the host",
	// which is both a better default and impossible to leave wrong by accident.
	if cfg.Interval <= 0 {
		cfg.Interval = 15 * time.Second
	}
	if cfg.Exporter.Type == "" {
		cfg.Exporter.Type = "stdout"
	}

	// Tailing defaults. These are applied here rather than at the point of use
	// so that every collector sees identical values and the effective config is
	// visible in one place.
	if cfg.EC2Metadata.Timeout <= 0 {
		cfg.EC2Metadata.Timeout = time.Second
	}

	if cfg.Tailing.ScanInterval <= 0 {
		cfg.Tailing.ScanInterval = 30 * time.Second
	}
	if cfg.Tailing.PollInterval <= 0 {
		cfg.Tailing.PollInterval = 500 * time.Millisecond
	}
	if cfg.Tailing.MaxLineBytes <= 0 {
		cfg.Tailing.MaxLineBytes = 256 * 1024
	}
	if cfg.Tailing.RegistryPath == "" {
		cfg.Tailing.RegistryPath = "/var/lib/agent-i/registry.json"
	}

	// Dashboard defaults. Enabled is NOT defaulted on: an existing
	// deployment that upgrades should not silently start listening on a new
	// port it never asked for. Fresh installs get it because the shipped
	// configs/agent.yaml turns it on explicitly.
	if cfg.Dashboard.ListenAddr == "" {
		cfg.Dashboard.ListenAddr = "127.0.0.1:8088"
	}
	if cfg.Dashboard.Retain <= 0 {
		cfg.Dashboard.Retain = 15 * time.Minute
	}
	if cfg.Dashboard.MaxSeries <= 0 {
		cfg.Dashboard.MaxSeries = 500
	}

	if cfg.Traces.ListenAddr == "" {
		cfg.Traces.ListenAddr = "127.0.0.1:4319"
	}
	if cfg.Traces.MaxRequestBytes <= 0 {
		cfg.Traces.MaxRequestBytes = 4 << 20 // 4 MiB
	}
	if cfg.Traces.Sampling.KeepErrors == nil {
		keep := true
		cfg.Traces.Sampling.KeepErrors = &keep
	}
	// Default on: an unset value means a config written before this listener
	// accepted anything but traces, and on those hosts the application's
	// metrics and logs were being 404'd. Turning them on is the fix, not a
	// behaviour change someone has to opt into.
	if cfg.Traces.AcceptLogs == nil {
		on := true
		cfg.Traces.AcceptLogs = &on
	}
	if cfg.Traces.AcceptMetrics == nil {
		on := true
		cfg.Traces.AcceptMetrics = &on
	}
	if cfg.Traces.Sampling.Enabled && cfg.Traces.Sampling.Rate <= 0 {
		// Enabling sampling without a rate almost certainly means "I forgot to
		// set one", not "drop every trace". Fail safe — keep everything — and
		// say so, rather than silently discarding all trace data.
		log.Printf("config: traces.sampling.enabled is true but rate is %v; defaulting to 1.0 (keep all). "+
			"Set traces.sampling.rate to a value below 1.0 to actually reduce span volume.",
			cfg.Traces.Sampling.Rate)
		cfg.Traces.Sampling.Rate = 1.0
	}
	if cfg.Traces.Sampling.Rate > 1 {
		cfg.Traces.Sampling.Rate = 1
	}

	return &cfg, nil
}
