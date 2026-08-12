// Package daemon wires configured collectors to the exporter and runs the
// main event loop. This is the only place that knows about "all" the
// collectors — collector.go, metrics.go, logs.go, traces.go each stay
// ignorant of each other, so adding a ninth collector never means editing
// an eighth.
package daemon

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/oneagent/agent/internal/collector"
	"github.com/oneagent/agent/internal/config"
	"github.com/oneagent/agent/internal/exporter"
)

type Daemon struct {
	cfg        *config.Config
	collectors []collector.Collector
	exp        exporter.Exporter
}

func New(cfg *config.Config) (*Daemon, error) {
	exp, err := exporter.New(resolveExporterHeaders(cfg.Exporter))
	if err != nil {
		return nil, fmt.Errorf("initializing exporter: %w", err)
	}

	var collectors []collector.Collector
	if cfg.Metrics.Enabled {
		collectors = append(collectors, collector.NewHostMetricsCollector(cfg.AgentID, cfg.Interval, cfg.Metrics.Collect))
	}
	if cfg.Logs.Enabled {
		collectors = append(collectors, collector.NewLogTailCollector(cfg.AgentID, cfg.Logs.Paths))
	}
	if cfg.AccessLogs.Enabled {
		format := collector.FormatCombined
		if cfg.AccessLogs.Format == "json" {
			format = collector.FormatJSON
		}
		fields := collector.JSONFieldMap{
			Method:     cfg.AccessLogs.JSONFields.Method,
			Path:       cfg.AccessLogs.JSONFields.Path,
			Status:     cfg.AccessLogs.JSONFields.Status,
			DurationMs: cfg.AccessLogs.JSONFields.DurationMs,
			RemoteAddr: cfg.AccessLogs.JSONFields.RemoteAddr,
		}
		collectors = append(collectors, collector.NewAccessLogCollector(cfg.AgentID, cfg.AccessLogs.Paths, format, fields))
	}
	if cfg.Traces.Enabled {
		collectors = append(collectors, collector.NewOTLPTraceReceiverCollector(cfg.AgentID, cfg.Traces.ListenAddr))
	}

	cloudCollectors, err := buildCloudCollectors(cfg)
	if err != nil {
		return nil, err
	}
	collectors = append(collectors, cloudCollectors...)

	if len(collectors) == 0 {
		return nil, fmt.Errorf("config: no collectors enabled — set at least one of metrics.enabled, logs.enabled, access_logs.enabled, traces.enabled, cloud.{aws,gcp,azure}.enabled")
	}

	return &Daemon{cfg: cfg, collectors: collectors, exp: exp}, nil
}

// resolveExporterHeaders merges headers_env (env var references) into
// Headers before the exporter is constructed — same secrets-stay-out-of-
// YAML pattern as buildCloudCollectors below. Doesn't mutate the caller's
// config struct.
func resolveExporterHeaders(cfg config.ExporterConfig) config.ExporterConfig {
	if len(cfg.HeadersEnv) == 0 {
		return cfg
	}
	merged := make(map[string]string, len(cfg.Headers)+len(cfg.HeadersEnv))
	for k, v := range cfg.Headers {
		merged[k] = v
	}
	for headerName, envVar := range cfg.HeadersEnv {
		merged[headerName] = os.Getenv(envVar)
	}
	cfg.Headers = merged
	return cfg
}

// resolving secrets from the environment variables named in config (see
// the SECURITY note on config.CloudConfig — the YAML file itself never
// holds a credential value, only the name of where to find one).
func buildCloudCollectors(cfg *config.Config) ([]collector.Collector, error) {
	var collectors []collector.Collector

	if cfg.Cloud.AWS.Enabled {
		accessKey := os.Getenv(cfg.Cloud.AWS.AccessKeyIDEnv)
		secretKey := os.Getenv(cfg.Cloud.AWS.SecretAccessKeyEnv)
		if accessKey == "" || secretKey == "" {
			return nil, fmt.Errorf("config: cloud.aws.enabled is true but %s / %s are not set in the environment",
				cfg.Cloud.AWS.AccessKeyIDEnv, cfg.Cloud.AWS.SecretAccessKeyEnv)
		}
		collectors = append(collectors, collector.NewCloudWatchCollector(collector.CloudWatchConfig{
			AgentID:         cfg.AgentID,
			Region:          cfg.Cloud.AWS.Region,
			Namespace:       cfg.Cloud.AWS.Namespace,
			MetricName:      cfg.Cloud.AWS.MetricName,
			Statistic:       cfg.Cloud.AWS.Statistic,
			DimensionName:   cfg.Cloud.AWS.DimensionName,
			DimensionValue:  cfg.Cloud.AWS.DimensionValue,
			Interval:        cfg.Interval,
			AccessKeyID:     accessKey,
			SecretAccessKey: secretKey,
			SessionToken:    os.Getenv(cfg.Cloud.AWS.SessionTokenEnv),
		}))
	}

	if cfg.Cloud.GCP.Enabled {
		keyBytes, err := os.ReadFile(cfg.Cloud.GCP.ServiceAccountKeyPath)
		if err != nil {
			return nil, fmt.Errorf("config: cloud.gcp.enabled is true but reading service_account_key_path failed: %w", err)
		}
		gm, err := collector.NewGCPMonitoringCollector(collector.GCPMonitoringConfig{
			AgentID:           cfg.AgentID,
			ProjectID:         cfg.Cloud.GCP.ProjectID,
			MetricType:        cfg.Cloud.GCP.MetricType,
			ServiceAccountKey: keyBytes,
			Interval:          cfg.Interval,
		})
		if err != nil {
			return nil, fmt.Errorf("config: initializing GCP monitoring collector: %w", err)
		}
		collectors = append(collectors, gm)
	}

	if cfg.Cloud.Azure.Enabled {
		clientSecret := os.Getenv(cfg.Cloud.Azure.ClientSecretEnv)
		if clientSecret == "" {
			return nil, fmt.Errorf("config: cloud.azure.enabled is true but %s is not set in the environment", cfg.Cloud.Azure.ClientSecretEnv)
		}
		collectors = append(collectors, collector.NewAzureMonitorCollector(collector.AzureMonitorConfig{
			AgentID:        cfg.AgentID,
			TenantID:       cfg.Cloud.Azure.TenantID,
			ClientID:       cfg.Cloud.Azure.ClientID,
			ClientSecret:   clientSecret,
			SubscriptionID: cfg.Cloud.Azure.SubscriptionID,
			ResourceID:     cfg.Cloud.Azure.ResourceID,
			MetricName:     cfg.Cloud.Azure.MetricName,
			Interval:       cfg.Interval,
		}))
	}

	return collectors, nil
}

// Run starts all collectors and drains their output into the exporter
// until ctx is cancelled. Blocks until shutdown is complete.
func (d *Daemon) Run(ctx context.Context) error {
	out := make(chan collector.Envelope, 256) // buffered: absorbs bursts (e.g. a batch of pushed spans) without blocking collectors

	for _, c := range d.collectors {
		if err := c.Start(ctx, out); err != nil {
			return fmt.Errorf("starting collector %s: %w", c.Name(), err)
		}
		log.Printf("started collector: %s", c.Name())
	}

	defer func() {
		for _, c := range d.collectors {
			if err := c.Stop(); err != nil {
				log.Printf("error stopping collector %s: %v", c.Name(), err)
			}
		}
		if err := d.exp.Close(); err != nil {
			log.Printf("error closing exporter: %v", err)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case env := <-out:
			if err := d.exp.Export(env); err != nil {
				// A single export failure shouldn't crash the daemon —
				// log and keep draining, or a flaky exporter endpoint
				// takes down all telemetry collection with it.
				log.Printf("export error (source=%s): %v", env.Source, err)
			}
		}
	}
}
