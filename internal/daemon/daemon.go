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
	"time"

	"github.com/agent-i/agent/internal/aggregate"
	"github.com/agent-i/agent/internal/collector"
	"github.com/agent-i/agent/internal/config"
	"github.com/agent-i/agent/internal/dashboard"
	"github.com/agent-i/agent/internal/exporter"
	"github.com/agent-i/agent/internal/spans"
	"github.com/agent-i/agent/internal/version"
)

type Daemon struct {
	cfg        *config.Config
	collectors []collector.Collector
	exp        exporter.Exporter
	// agg is nil unless aggregation is enabled. When set it sits between the
	// drain loop and the exporter, absorbing per-request events and emitting
	// interval summaries in their place.
	agg *aggregate.Aggregator
	// spans is nil unless trace stats or sampling are enabled. It counts every
	// span and decides which ones are forwarded.
	spans *spans.Processor

	// dash and dashSrv are nil unless the local dashboard is enabled. The
	// store taps the export path read-only: it observes what is being
	// exported and never alters or delays it.
	dash    *dashboard.Store
	dashSrv *dashboard.Server

	// reloadCh carries a new configuration into the drain loop. Reload is
	// applied there rather than by the signal handler so that everything the
	// daemon owns continues to be touched from exactly one goroutine, which is
	// what lets the aggregator and span processor stay lock-free.
	reloadCh chan *config.Config
}

func New(cfg *config.Config) (*Daemon, error) {
	exp, err := exporter.New(resolveExporterHeaders(cfg.Exporter))
	if err != nil {
		return nil, fmt.Errorf("initializing exporter: %w", err)
	}

	var collectors []collector.Collector
	if cfg.Metrics.Enabled {
		collectors = append(collectors, collector.NewHostMetricsCollector(cfg.AgentID, cfg.Interval, cfg.Metrics.Collect))
		// Additive, not a replacement — emits the standard OTel
		// hostmetrics names (system.cpu.time, system.memory.usage)
		// alongside our own host.cpu.used_pct/host.memory.used_pct.
		// Required specifically for SigNoz's Infrastructure Monitoring
		// > Hosts page to recognize this host at all (confirmed against
		// SigNoz's own docs — see infra_hostmetrics.go).
		collectors = append(collectors, collector.NewInfraHostMetricsCollector(cfg.AgentID, cfg.Interval))
	}
	// Every file-tailing collector shares one offset registry. Two registries
	// pointed at the same path would overwrite each other's offsets on every
	// flush, so this is constructed once here rather than per collector.
	var tailOpts collector.TailingOptions
	if cfg.Logs.Enabled || cfg.AccessLogs.Enabled {
		reg, err := collector.NewOffsetRegistry(cfg.Tailing.RegistryPath)
		if err != nil {
			return nil, fmt.Errorf("initializing offset registry: %w", err)
		}
		tailOpts = collector.TailingOptions{
			ScanInterval: cfg.Tailing.ScanInterval,
			PollInterval: cfg.Tailing.PollInterval,
			MaxLineBytes: cfg.Tailing.MaxLineBytes,
			Registry:     reg,
		}
	}

	if cfg.Logs.Enabled {
		collectors = append(collectors, collector.NewLogTailCollector(cfg.AgentID, cfg.Logs.Paths, tailOpts))
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
		collectors = append(collectors, collector.NewAccessLogCollector(cfg.AgentID, cfg.AccessLogs.Paths, format, fields, tailOpts))
	}
	if cfg.Traces.Enabled {
		collectors = append(collectors, collector.NewOTLPTraceReceiverCollector(
			cfg.AgentID,
			cfg.Traces.ListenAddr,
			cfg.Traces.MaxRequestBytes,
			os.Getenv(cfg.Traces.AuthTokenEnv), // empty env name yields "", i.e. no auth
		))
	}

	cloudCollectors, err := buildCloudCollectors(cfg)
	if err != nil {
		return nil, err
	}
	collectors = append(collectors, cloudCollectors...)

	if len(collectors) == 0 {
		return nil, fmt.Errorf("config: no collectors enabled — set at least one of metrics.enabled, logs.enabled, access_logs.enabled, traces.enabled, cloud.aws.enabled")
	}

	d := &Daemon{
		cfg:        cfg,
		collectors: collectors,
		exp:        exp,
		reloadCh:   make(chan *config.Config, 1),
	}

	if cfg.Aggregation.Enabled {
		d.agg = aggregate.New(cfg.AgentID, aggregate.Config{
			Enabled:       true,
			Interval:      cfg.Aggregation.Interval,
			MaxContexts:   cfg.Aggregation.MaxContexts,
			MaxSamples:    cfg.Aggregation.MaxSamples,
			KeepRawEvents: cfg.Aggregation.KeepRawEvents,
		})
		log.Printf("aggregation enabled: access log requests summarised every %s", d.agg.Interval())
	}

	sp := spans.New(cfg.AgentID, spans.Config{
		StatsEnabled:    cfg.Traces.Stats.Enabled,
		SamplingEnabled: cfg.Traces.Sampling.Enabled,
		Rate:            cfg.Traces.Sampling.Rate,
		KeepErrors:      cfg.Traces.Sampling.KeepErrors != nil && *cfg.Traces.Sampling.KeepErrors,
		SlowThresholdMs: cfg.Traces.Sampling.SlowThresholdMs,
		Interval:        cfg.Traces.Stats.Interval,
		MaxContexts:     cfg.Traces.Stats.MaxContexts,
		MaxSamples:      cfg.Traces.Stats.MaxSamples,
	})
	if sp.Enabled() {
		d.spans = sp
		if cfg.Traces.Stats.Enabled {
			log.Printf("trace stats enabled: RED metrics over 100%% of spans every %s", sp.Interval())
		}
		if cfg.Traces.Sampling.Enabled {
			log.Printf("trace sampling enabled: rate=%.3f keep_errors=%t slow_threshold_ms=%.0f",
				cfg.Traces.Sampling.Rate, *cfg.Traces.Sampling.KeepErrors, cfg.Traces.Sampling.SlowThresholdMs)
		}
	}

	if cfg.Dashboard.Enabled {
		d.dash = dashboard.NewStore(cfg.AgentID, version.Version, cfg.Dashboard.Retain, cfg.Dashboard.MaxSeries)
		// Constructed here rather than in Run so a port conflict is a
		// startup error the operator sees immediately, alongside every other
		// configuration failure, instead of a log line after the agent has
		// already reported itself healthy.
		srv, err := dashboard.NewServer(cfg.Dashboard.ListenAddr, d.dash)
		if err != nil {
			return nil, err
		}
		d.dashSrv = srv
		log.Printf("dashboard enabled: http://%s (retaining %s, max %d series)",
			srv.Addr(), cfg.Dashboard.Retain, cfg.Dashboard.MaxSeries)
	}

	return d, nil
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

	// Disabled along with their collectors — see the build tag at the top of
	// internal/collector/gcpmonitoring.go and azuremonitor.go, and the matching
	// commented structs in internal/config/config.go.
	//
	// if cfg.Cloud.GCP.Enabled {
	// 	keyBytes, err := os.ReadFile(cfg.Cloud.GCP.ServiceAccountKeyPath)
	// 	if err != nil {
	// 		return nil, fmt.Errorf("config: cloud.gcp.enabled is true but reading service_account_key_path failed: %w", err)
	// 	}
	// 	gm, err := collector.NewGCPMonitoringCollector(collector.GCPMonitoringConfig{
	// 		AgentID:           cfg.AgentID,
	// 		ProjectID:         cfg.Cloud.GCP.ProjectID,
	// 		MetricType:        cfg.Cloud.GCP.MetricType,
	// 		ServiceAccountKey: keyBytes,
	// 		Interval:          cfg.Interval,
	// 	})
	// 	if err != nil {
	// 		return nil, fmt.Errorf("config: initializing GCP monitoring collector: %w", err)
	// 	}
	// 	collectors = append(collectors, gm)
	// }
	//
	// if cfg.Cloud.Azure.Enabled {
	// 	clientSecret := os.Getenv(cfg.Cloud.Azure.ClientSecretEnv)
	// 	if clientSecret == "" {
	// 		return nil, fmt.Errorf("config: cloud.azure.enabled is true but %s is not set in the environment", cfg.Cloud.Azure.ClientSecretEnv)
	// 	}
	// 	collectors = append(collectors, collector.NewAzureMonitorCollector(collector.AzureMonitorConfig{
	// 		AgentID:        cfg.AgentID,
	// 		TenantID:       cfg.Cloud.Azure.TenantID,
	// 		ClientID:       cfg.Cloud.Azure.ClientID,
	// 		ClientSecret:   clientSecret,
	// 		SubscriptionID: cfg.Cloud.Azure.SubscriptionID,
	// 		ResourceID:     cfg.Cloud.Azure.ResourceID,
	// 		MetricName:     cfg.Cloud.Azure.MetricName,
	// 		Interval:       cfg.Interval,
	// 	}))
	// }

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

	if d.dashSrv != nil {
		go d.dashSrv.Serve()
	}

	defer func() {
		for _, c := range d.collectors {
			if err := c.Stop(); err != nil {
				log.Printf("error stopping collector %s: %v", c.Name(), err)
			}
		}
		if d.dashSrv != nil {
			if err := d.dashSrv.Close(); err != nil {
				log.Printf("error closing dashboard: %v", err)
			}
		}
		if err := d.exp.Close(); err != nil {
			log.Printf("error closing exporter: %v", err)
		}
	}()

	// aggTick fires the summary window when aggregation is enabled, and stays
	// nil otherwise — a nil channel blocks forever in a select, so the case is
	// simply never chosen.
	// Both tickers always exist, even when their component is disabled: that
	// way a reload can enable a component or change its interval with a
	// Reset, instead of needing to create a ticker that the select is already
	// blocked on. The handlers no-op when their component is nil.
	aggTicker := time.NewTicker(tickerInterval(d.aggInterval()))
	defer aggTicker.Stop()
	spanTicker := time.NewTicker(tickerInterval(d.spanInterval()))
	defer spanTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Flush the partial windows before shutting down, so the last
			// interval's data is reported rather than discarded.
			d.flushAggregated()
			d.flushSpans()
			// Envelopes already produced but not yet handed to the exporter
			// were simply abandoned here before, so a clean SIGTERM silently
			// discarded up to a full channel's worth of telemetry.
			d.drain(out)
			return nil
		case <-aggTicker.C:
			d.flushAggregated()
		case <-spanTicker.C:
			d.flushSpans()
		case newCfg := <-d.reloadCh:
			d.applyConfig(newCfg, aggTicker, spanTicker)
		case env := <-out:
			// Absorbed envelopes are replaced by the summaries emitted at the
			// next flush; everything else passes straight through.
			if d.agg != nil && d.agg.Add(env) {
				continue
			}
			// Spans are always counted, then possibly dropped by sampling.
			if d.spans != nil && !d.spans.Process(env) {
				continue
			}
			d.export(env)
		}
	}
}

func (d *Daemon) export(env collector.Envelope) {
	// Observed before export, so what the dashboard shows is exactly what
	// was handed to the exporter — including envelopes a failing or slow
	// backend never actually receives. That is the whole point: it answers
	// "is the agent collecting?" independently of "is delivery working?".
	if d.dash != nil {
		d.dash.Record(env)
	}
	if err := d.exp.Export(env); err != nil {
		// A single export failure shouldn't crash the daemon — log and keep
		// draining, or a flaky exporter endpoint takes down all telemetry
		// collection with it.
		log.Printf("export error (source=%s): %v", env.Source, err)
	}
}

func (d *Daemon) flushAggregated() {
	if d.agg == nil {
		return
	}
	for _, env := range d.agg.Flush(time.Now().UTC()) {
		d.export(env)
	}
}

func (d *Daemon) flushSpans() {
	if d.spans == nil {
		return
	}
	for _, env := range d.spans.Flush(time.Now().UTC()) {
		d.export(env)
	}
}

// Reload hands a new configuration to the running daemon. It is safe to call
// from any goroutine and never blocks: if a reload is already pending it is
// superseded, since only the newest configuration matters.
//
// A configuration that fails to parse must never reach here — the caller keeps
// running on the old one rather than taking the agent down, because a typo in
// a config file should not cost you telemetry from the host.
func (d *Daemon) Reload(cfg *config.Config) {
	select {
	case <-d.reloadCh:
	default:
	}
	select {
	case d.reloadCh <- cfg:
	default:
	}
}

func (d *Daemon) aggInterval() time.Duration {
	if d.agg == nil {
		return 0
	}
	return d.agg.Interval()
}

func (d *Daemon) spanInterval() time.Duration {
	if d.spans == nil {
		return 0
	}
	return d.spans.Interval()
}

// tickerInterval keeps a disabled component's ticker running at a slow, harmless
// rate rather than requiring a nil channel, so reload can simply Reset it.
func tickerInterval(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Minute
	}
	return d
}

// applyConfig swaps in a new configuration. It runs on the drain loop, so it
// can rebuild the processors without any synchronisation.
func (d *Daemon) applyConfig(cfg *config.Config, aggTicker, spanTicker *time.Ticker) {
	if blocked := restartRequired(d.cfg, cfg); len(blocked) > 0 {
		log.Printf("reload: ignoring changes to %v — these need a restart to take effect", blocked)
	}

	// Close out the current windows under the OLD settings before switching,
	// so a window is never summarised with a mix of two configurations.
	d.flushAggregated()
	d.flushSpans()

	// Log paths: hand the new globs to the tailers, which pick up newly
	// matching files immediately and drop ones that no longer match.
	for _, c := range d.collectors {
		switch t := c.(type) {
		case *collector.LogTailCollector:
			if !equalStrings(d.cfg.Logs.Paths, cfg.Logs.Paths) {
				t.SetPaths(cfg.Logs.Paths)
				log.Printf("reload: logs.paths updated to %v", cfg.Logs.Paths)
			}
		case *collector.AccessLogCollector:
			if !equalStrings(d.cfg.AccessLogs.Paths, cfg.AccessLogs.Paths) {
				t.SetPaths(cfg.AccessLogs.Paths)
				log.Printf("reload: access_logs.paths updated to %v", cfg.AccessLogs.Paths)
			}
		}
	}

	// Rebuild the processors. They hold only per-window state, which was just
	// flushed, so replacing them wholesale loses nothing.
	if cfg.Aggregation.Enabled {
		d.agg = aggregate.New(cfg.AgentID, aggregate.Config{
			Enabled:       true,
			Interval:      cfg.Aggregation.Interval,
			MaxContexts:   cfg.Aggregation.MaxContexts,
			MaxSamples:    cfg.Aggregation.MaxSamples,
			KeepRawEvents: cfg.Aggregation.KeepRawEvents,
		})
	} else {
		d.agg = nil
	}

	sp := spans.New(cfg.AgentID, spans.Config{
		StatsEnabled:    cfg.Traces.Stats.Enabled,
		SamplingEnabled: cfg.Traces.Sampling.Enabled,
		Rate:            cfg.Traces.Sampling.Rate,
		KeepErrors:      cfg.Traces.Sampling.KeepErrors != nil && *cfg.Traces.Sampling.KeepErrors,
		SlowThresholdMs: cfg.Traces.Sampling.SlowThresholdMs,
		Interval:        cfg.Traces.Stats.Interval,
		MaxContexts:     cfg.Traces.Stats.MaxContexts,
		MaxSamples:      cfg.Traces.Stats.MaxSamples,
	})
	if sp.Enabled() {
		d.spans = sp
	} else {
		d.spans = nil
	}

	aggTicker.Reset(tickerInterval(d.aggInterval()))
	spanTicker.Reset(tickerInterval(d.spanInterval()))

	d.cfg = cfg
	log.Printf("reload: applied (aggregation=%t trace_stats=%t trace_sampling=%t rate=%.3f)",
		cfg.Aggregation.Enabled, cfg.Traces.Stats.Enabled,
		cfg.Traces.Sampling.Enabled, cfg.Traces.Sampling.Rate)
}

// restartRequired lists settings that changed but cannot be applied to a
// running agent. Reporting them is the point: silently ignoring a changed
// listen address or exporter endpoint would leave someone convinced their
// edit had taken effect.
func restartRequired(old, new *config.Config) []string {
	var changed []string
	add := func(name string, differs bool) {
		if differs {
			changed = append(changed, name)
		}
	}

	add("agent_id", old.AgentID != new.AgentID)
	add("interval", old.Interval != new.Interval)
	add("metrics.enabled", old.Metrics.Enabled != new.Metrics.Enabled)
	add("metrics.collect", !equalStrings(old.Metrics.Collect, new.Metrics.Collect))
	add("logs.enabled", old.Logs.Enabled != new.Logs.Enabled)
	add("access_logs.enabled", old.AccessLogs.Enabled != new.AccessLogs.Enabled)
	add("access_logs.format", old.AccessLogs.Format != new.AccessLogs.Format)
	add("tailing", old.Tailing != new.Tailing)
	add("traces.enabled", old.Traces.Enabled != new.Traces.Enabled)
	add("traces.listen_addr", old.Traces.ListenAddr != new.Traces.ListenAddr)
	add("traces.max_request_bytes", old.Traces.MaxRequestBytes != new.Traces.MaxRequestBytes)
	add("traces.auth_token_env", old.Traces.AuthTokenEnv != new.Traces.AuthTokenEnv)
	add("exporter", old.Exporter.Type != new.Exporter.Type ||
		old.Exporter.Endpoint != new.Exporter.Endpoint ||
		old.Exporter.Path != new.Exporter.Path)
	add("cloud.aws", old.Cloud.AWS != new.Cloud.AWS)

	return changed
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// shutdownDrainTimeout bounds how long shutdown waits for buffered envelopes.
// The exporter has its own bounded drain after this; both are bounded so that
// a restart against an unreachable backend stays a restart.
const shutdownDrainTimeout = 3 * time.Second

func (d *Daemon) drain(out <-chan collector.Envelope) {
	deadline := time.After(shutdownDrainTimeout)
	drained := 0
	for {
		select {
		case env := <-out:
			if err := d.exp.Export(env); err != nil {
				log.Printf("export error during shutdown (source=%s): %v", env.Source, err)
			}
			drained++
		case <-deadline:
			log.Printf("shutdown: drain timed out, %d envelopes still buffered", len(out))
			return
		default:
			if drained > 0 {
				log.Printf("shutdown: drained %d buffered envelopes", drained)
			}
			return
		}
	}
}
