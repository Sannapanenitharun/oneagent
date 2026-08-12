// Package config loads the agent's YAML configuration. Kept dependency-light
// (one third-party lib: yaml.v3) so the binary stays small and auditable.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration for the agent daemon.
type Config struct {
	AgentID    string          `yaml:"agent_id"`
	Interval   time.Duration   `yaml:"interval"`
	Metrics    MetricsConfig   `yaml:"metrics"`
	Logs       LogsConfig      `yaml:"logs"`
	AccessLogs AccessLogConfig `yaml:"access_logs"`
	Traces     TracesConfig    `yaml:"traces"`
	Cloud      CloudConfig     `yaml:"cloud"`
	Exporter   ExporterConfig  `yaml:"exporter"`
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
	// Address to listen on for OTLP-compatible span pushes from instrumented apps
	ListenAddr string `yaml:"listen_addr"`
}

// CloudConfig configures the pull-based cloud provider metric adapters.
// SECURITY: secrets (access keys, client secrets, service account keys)
// are never held directly in this struct or the YAML file — every secret
// field below is the NAME of an environment variable to read at startup,
// not the secret itself. A config file is often committed, backed up, or
// shared more casually than the process environment; keeping secrets out
// of it is a cheap, meaningful reduction in exposure surface.
type CloudConfig struct {
	AWS   AWSCloudConfig   `yaml:"aws"`
	GCP   GCPCloudConfig   `yaml:"gcp"`
	Azure AzureCloudConfig `yaml:"azure"`
}

type AWSCloudConfig struct {
	Enabled            bool   `yaml:"enabled"`
	Region             string `yaml:"region"`
	Namespace          string `yaml:"namespace"`   // e.g. "AWS/EC2"
	MetricName         string `yaml:"metric_name"` // e.g. "CPUUtilization"
	Statistic          string `yaml:"statistic"`   // "Average" (default), "Maximum", "Sum", "Minimum"
	DimensionName      string `yaml:"dimension_name"`
	DimensionValue     string `yaml:"dimension_value"`
	AccessKeyIDEnv     string `yaml:"access_key_id_env"`     // env var holding the AWS access key ID
	SecretAccessKeyEnv string `yaml:"secret_access_key_env"` // env var holding the AWS secret access key
	SessionTokenEnv    string `yaml:"session_token_env"`     // optional: env var for a temporary/STS session token
}

type GCPCloudConfig struct {
	Enabled               bool   `yaml:"enabled"`
	ProjectID             string `yaml:"project_id"`
	MetricType            string `yaml:"metric_type"`              // e.g. "compute.googleapis.com/instance/cpu/utilization"
	ServiceAccountKeyPath string `yaml:"service_account_key_path"` // path to the downloaded JSON key file (the file's permissions, not this config, protect the key)
}

type AzureCloudConfig struct {
	Enabled         bool   `yaml:"enabled"`
	TenantID        string `yaml:"tenant_id"`
	ClientID        string `yaml:"client_id"`
	SubscriptionID  string `yaml:"subscription_id"`
	ResourceID      string `yaml:"resource_id"`       // full ARM resource ID to poll metrics for
	MetricName      string `yaml:"metric_name"`       // e.g. "Percentage CPU"
	ClientSecretEnv string `yaml:"client_secret_env"` // env var holding the AAD app's client secret
}

type ExporterConfig struct {
	// "stdout" for local dev, "file" to append JSONL, "http" to push to a collector backend
	Type     string `yaml:"type"`
	Path     string `yaml:"path"`
	Endpoint string `yaml:"endpoint"`

	// HTTP exporter tuning — all optional, sensible defaults applied in
	// the exporter package if left zero.
	BatchSize     int           `yaml:"batch_size"`     // envelopes per POST before a size-triggered flush
	FlushInterval time.Duration `yaml:"flush_interval"` // max time a partial batch waits before flushing
	MaxRetries    int           `yaml:"max_retries"`    // retry attempts on 5xx/network error, not on 4xx
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
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	if cfg.AgentID == "" {
		return nil, fmt.Errorf("config: agent_id is required")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 15 * time.Second
	}
	if cfg.Exporter.Type == "" {
		cfg.Exporter.Type = "stdout"
	}

	return &cfg, nil
}
