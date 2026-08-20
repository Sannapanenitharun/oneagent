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
	"strings"
	"time"

	"github.com/agent-i/agent/internal/aggregate"
	"github.com/agent-i/agent/internal/collector"
	"github.com/agent-i/agent/internal/config"
	"github.com/agent-i/agent/internal/dashboard"
	"github.com/agent-i/agent/internal/ec2meta"
	"github.com/agent-i/agent/internal/exporter"
	"github.com/agent-i/agent/internal/spans"
	"github.com/agent-i/agent/internal/version"
)

// apiTokenEnv names the environment variable holding an optional bearer token
// for the agent's two listeners. It is a fixed name rather than one named by
// the config, so auth can be switched on by setting one variable in
// /etc/agent-i/env and restarting — no config edit, no redeploy, and nothing
// new on disk. The secret still never appears in the config file, which is the
// rule that matters.
//
// Unset means no authentication, which is the historical behaviour on both
// listeners and remains the default.
const apiTokenEnv = "AGENT_I_API_TOKEN"

type Daemon struct {
	cfg *config.Config
	// agentID is the resolved name this agent reports under, which is not
	// necessarily cfg.AgentID: an empty configured value is filled in from the
	// host at startup. Held here so a reload rebuilding the processors reuses
	// the resolved value instead of the empty one still sitting in the file.
	agentID    string
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

	// tailRegistry is nil unless a file-tailing collector is enabled. It is the
	// one piece of daemon-adjacent state deliberately not owned by the drain
	// loop: it already had its own lock because several tailer goroutines share
	// it, and the exporter's sender goroutine now reports into it too. The
	// daemon's own state stays lock-free.
	tailRegistry *collector.OffsetRegistry
	// journalCursors is nil unless journald collection is enabled. Same role
	// as tailRegistry for a source that has positions but no files: the
	// exporter commits into it as entries settle.
	journalCursors *collector.CursorStore

	// reloadCh carries a new configuration into the drain loop. Reload is
	// applied there rather than by the signal handler so that everything the
	// daemon owns continues to be touched from exactly one goroutine, which is
	// what lets the aggregator and span processor stay lock-free.
	reloadCh chan *config.Config
}

func New(cfg *config.Config) (*Daemon, error) {
	d := &Daemon{
		cfg:      cfg,
		reloadCh: make(chan *config.Config, 1),
	}

	// Latency is no longer stored as a sample, so there is no sample count to
	// cap. Said out loud rather than ignored silently: a setting that quietly
	// stopped doing anything is worse than one that was removed, because the
	// operator goes on believing it is in effect.
	if cfg.Aggregation.MaxSamples > 0 || cfg.Traces.Stats.MaxSamples > 0 {
		log.Printf("config: max_samples_per_context is set but no longer does anything — " +
			"percentiles now come from a bucketed histogram whose memory is bounded by its bucket " +
			"count, not by a sample cap. The setting is ignored and can be deleted.")
	}

	// Detected before anything is constructed, because three things need it:
	// the exporter attaches these attributes to every signal, the dashboard
	// displays them, and the agent id below is derived from them when the
	// config does not name one. Probing once and sharing the result keeps a
	// per-host startup cost from being paid repeatedly for a value that cannot
	// change while the process runs.
	hostAttrs := detectHostAttributes(cfg)
	d.agentID = resolveAgentID(cfg.AgentID, hostAttrs)

	var collectors []collector.Collector
	if cfg.Metrics.Enabled {
		collectors = append(collectors, collector.NewHostMetricsCollector(d.agentID, cfg.Interval, cfg.Metrics.Collect))
		// Additive, not a replacement — emits the standard OTel
		// hostmetrics names (system.cpu.time, system.memory.usage)
		// alongside our own host.cpu.used_pct/host.memory.used_pct.
		// Required for a backend's host-inventory view
		// > Hosts page to recognize this host at all (confirmed against
		// the OTel hostmetrics receiver spec — see infra_hostmetrics.go).
		collectors = append(collectors, collector.NewInfraHostMetricsCollector(d.agentID, cfg.Interval))
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
		d.tailRegistry = reg
	}

	if cfg.Logs.Enabled {
		lc, err := collector.NewLogTailCollector(d.agentID, cfg.Logs.Paths, tailOpts,
			collector.MultilineOptions{
				StartPattern: cfg.Logs.Multiline.StartPattern,
				MaxLines:     cfg.Logs.Multiline.MaxLines,
				Timeout:      cfg.Logs.Multiline.Timeout,
			},
			cfg.Logs.Multiline.Enabled,
		)
		if err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
		collectors = append(collectors, lc)
		if cfg.Logs.Multiline.Enabled {
			log.Printf("multiline log assembly enabled: records start at /%s/", cfg.Logs.Multiline.StartPattern)
		}
	}
	// The journal is not a file, so the tailer above cannot reach it. On a
	// systemd host this is where the OS's own logs are — sshd, the kernel,
	// unit failures — and without it those are simply not collected.
	if cfg.Journald.Enabled {
		jc := collector.NewJournaldCollector(d.agentID, collector.JournaldOptions{
			Units:          cfg.Journald.Units,
			ExcludeUnits:   cfg.Journald.ExcludeUnits,
			Priority:       cfg.Journald.Priority,
			Since:          cfg.Journald.Since,
			JournalctlPath: cfg.Journald.JournalctlPath,
			CursorPath:     cfg.Journald.CursorPath,
		})
		// The collector owns the store it reads from; the daemon needs the
		// same one to commit into as envelopes settle.
		d.journalCursors = jc.Cursors()
		collectors = append(collectors, jc)
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
		collectors = append(collectors, collector.NewAccessLogCollector(d.agentID, cfg.AccessLogs.Paths, format, fields, tailOpts))
	}
	if cfg.Traces.Enabled {
		// traces.auth_token_env wins when it is set and non-empty, so a host
		// that already configured a receiver-specific token keeps exactly the
		// token it had. AGENT_I_API_TOKEN is only a fallback, which is what
		// lets one variable cover both listeners without a config edit.
		traceToken := os.Getenv(cfg.Traces.AuthTokenEnv) // empty env name yields ""
		if traceToken == "" {
			traceToken = os.Getenv(apiTokenEnv)
		}
		rec := collector.NewOTLPReceiverCollector(
			d.agentID,
			cfg.Traces.ListenAddr,
			cfg.Traces.MaxRequestBytes,
			traceToken,
		)
		// Both default to true in config.Load, so the nil check there is what
		// decides this — by here they are always set.
		rec.AcceptSignals(*cfg.Traces.AcceptLogs, *cfg.Traces.AcceptMetrics)
		collectors = append(collectors, rec)
	}

	if len(collectors) == 0 {
		return nil, fmt.Errorf("config: no collectors enabled — set at least one of metrics.enabled, logs.enabled, access_logs.enabled, traces.enabled")
	}

	// Built after the registry so the exporter can report which lines are
	// settled. Constructing it here rather than first is the whole change:
	// the offset used to be written the moment a line was read, which meant a
	// line sitting in the export queue when the process died was recorded as
	// handled and never read again.
	expCfg := resolveExporterHeaders(cfg.Exporter)
	expCfg.ResourceAttributes = hostAttrs

	exp, err := exporter.New(expCfg, d.retire)
	if err != nil {
		return nil, fmt.Errorf("initializing exporter: %w", err)
	}
	d.collectors = collectors
	d.exp = exp

	if cfg.Aggregation.Enabled {
		d.agg = aggregate.New(d.agentID, aggregate.Config{
			Enabled:       true,
			Interval:      cfg.Aggregation.Interval,
			MaxContexts:   cfg.Aggregation.MaxContexts,
			KeepRawEvents: cfg.Aggregation.KeepRawEvents,
		})
		log.Printf("aggregation enabled: access log requests summarised every %s", d.agg.Interval())
	}

	sp := spans.New(d.agentID, spans.Config{
		StatsEnabled:    cfg.Traces.Stats.Enabled,
		SamplingEnabled: cfg.Traces.Sampling.Enabled,
		Rate:            cfg.Traces.Sampling.Rate,
		KeepErrors:      cfg.Traces.Sampling.KeepErrors != nil && *cfg.Traces.Sampling.KeepErrors,
		SlowThresholdMs: cfg.Traces.Sampling.SlowThresholdMs,
		Interval:        cfg.Traces.Stats.Interval,
		MaxContexts:     cfg.Traces.Stats.MaxContexts,
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
		d.dash = dashboard.NewStore(d.agentID, version.Version, cfg.Dashboard.Retain, cfg.Dashboard.MaxSeries)
		d.dash.SetHostAttributes(hostAttrs)
		// Constructed here rather than in Run so a port conflict is a
		// startup error the operator sees immediately, alongside every other
		// configuration failure, instead of a log line after the agent has
		// already reported itself healthy.
		srv, err := dashboard.NewServer(cfg.Dashboard.ListenAddr, d.dash, os.Getenv(apiTokenEnv))
		if err != nil {
			return nil, err
		}
		d.dashSrv = srv
		log.Printf("dashboard enabled: http://%s (retaining %s, max %d series)",
			srv.Addr(), cfg.Dashboard.Retain, cfg.Dashboard.MaxSeries)
	}

	return d, nil
}

// fallbackAgentID names an agent on a host that could not identify itself at
// all — not EC2, and no usable hostname. Reached only if os.Hostname fails,
// which on a working system it does not.
const fallbackAgentID = "unidentified-host"

// resolveAgentID decides what this agent calls itself.
//
// A configured value always wins, because it is an operator saying so
// explicitly. Everything after it covers the case this function exists for: a
// config installed unattended across a fleet. That file used to carry a
// hardcoded id, so every host installed from it reported under the same name
// and no backend could tell them apart — which is precisely the question an
// agent is deployed to answer.
//
// The fallbacks run most-meaningful-first. The Name tag is what people
// actually call an instance, but it is absent unless tags are exposed through
// IMDS, which is not the default. The instance id is always present on EC2 and
// always unique, if ugly. The hostname is all that is left off EC2, and is
// what someone filling the field in by hand would most likely have typed.
func resolveAgentID(configured string, hostAttrs map[string]string) string {
	if id := strings.TrimSpace(configured); id != "" {
		return id
	}
	if name := hostAttrs["host.name"]; name != "" {
		log.Printf("agent_id: not set in config — using the instance Name tag %q", name)
		return name
	}
	if id := hostAttrs["host.id"]; id != "" {
		log.Printf("agent_id: not set in config — using the EC2 instance id %s", id)
		return id
	}
	if h, err := os.Hostname(); err == nil {
		if h = strings.TrimSpace(h); h != "" {
			log.Printf("agent_id: not set in config — using the hostname %q", h)
			return h
		}
	}
	// A shared id is acceptable here and nowhere else: the host has told us
	// nothing to distinguish it by, and an agent running under a poor name
	// still reports, whereas one that refuses to start reports nothing.
	log.Printf("agent_id: not set in config and the host could not be identified — using %q", fallbackAgentID)
	return fallbackAgentID
}

// detectHostAttributes discovers resource attributes that describe the machine
// rather than the configuration — currently the EC2 instance identity.
//
// Failure is the expected outcome on anything that is not an EC2 instance, so
// it is reported at info level and the agent continues: telemetry without
// instance attributes is a smaller loss than an agent that refuses to start on
// a laptop. The probe is bounded by its own timeout so a blackholed link-local
// address cannot stall startup.
//
// The instance id, type and region are logged because they are what an operator
// checks when a host shows up unlabelled. The account id deliberately is not.
func detectHostAttributes(cfg *config.Config) map[string]string {
	if !cfg.EC2Metadata.DetectionEnabled() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.EC2Metadata.Timeout)
	defer cancel()

	md, err := ec2meta.NewDetector(cfg.EC2Metadata.Timeout).Detect(ctx)
	if err != nil {
		log.Printf("ec2 metadata: no instance identity available (%v) — continuing without EC2 attributes", err)
		return nil
	}
	log.Printf("ec2 metadata: instance %s (%s) in %s", md.InstanceID, md.InstanceType, md.Region)
	return md.ResourceAttributes()
}

// resolveExporterHeaders merges headers_env (env var references) into
// Headers before the exporter is constructed — same secrets-stay-out-of-
// YAML pattern the agent uses everywhere. Doesn't mutate the caller's
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
				// Absorbed is settled: the request has been counted into a
				// summary that will be exported. Without this, aggregated access
				// logs would never advance their file offset and every restart
				// would re-read them from the beginning.
				d.retire(env)
				continue
			}
			// Spans are always counted, then possibly dropped by sampling.
			if d.spans != nil && !d.spans.Process(env) {
				d.retire(env)
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

// retire records that an envelope has been accounted for, so a tailed file no
// longer needs to re-read the line it came from. Safe to call for any envelope:
// anything that did not come from a file carries no position and is ignored.
//
// Called from the drain loop and from the exporter's sender goroutine. The
// registry is the synchronisation point, which is why the daemon can keep
// owning its own state without a lock.
func (d *Daemon) retire(env collector.Envelope) {
	if d.tailRegistry != nil {
		collector.CommitTailOffset(d.tailRegistry, env)
	}
	if d.journalCursors != nil {
		collector.CommitJournalCursor(d.journalCursors, env)
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
	blocked := restartRequired(d.cfg, cfg)
	if len(blocked) > 0 {
		log.Printf("reload: ignoring changes to %v — these need a restart to take effect", blocked)
	}
	// Published unconditionally, including the empty case: a reload that
	// resolves everything must clear a set left by an earlier one, or the UI
	// keeps reporting a restart that is no longer owed. Logging is unchanged —
	// this is in addition to it, not instead of it.
	if d.dash != nil {
		d.dash.SetPendingRestart(blocked)
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
		d.agg = aggregate.New(d.agentID, aggregate.Config{
			Enabled:       true,
			Interval:      cfg.Aggregation.Interval,
			MaxContexts:   cfg.Aggregation.MaxContexts,
			KeepRawEvents: cfg.Aggregation.KeepRawEvents,
		})
	} else {
		d.agg = nil
	}

	sp := spans.New(d.agentID, spans.Config{
		StatsEnabled:    cfg.Traces.Stats.Enabled,
		SamplingEnabled: cfg.Traces.Sampling.Enabled,
		Rate:            cfg.Traces.Sampling.Rate,
		KeepErrors:      cfg.Traces.Sampling.KeepErrors != nil && *cfg.Traces.Sampling.KeepErrors,
		SlowThresholdMs: cfg.Traces.Sampling.SlowThresholdMs,
		Interval:        cfg.Traces.Stats.Interval,
		MaxContexts:     cfg.Traces.Stats.MaxContexts,
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
	// The assembler is built once, with a compiled pattern and per-file state
	// mid-record. Swapping it live would mean deciding what happens to records
	// already in progress, for a setting nobody changes at runtime.
	add("logs.multiline", old.Logs.Multiline != new.Logs.Multiline)
	add("access_logs.enabled", old.AccessLogs.Enabled != new.AccessLogs.Enabled)
	add("access_logs.format", old.AccessLogs.Format != new.AccessLogs.Format)
	add("tailing", old.Tailing != new.Tailing)
	add("traces.enabled", old.Traces.Enabled != new.Traces.Enabled)
	add("traces.listen_addr", old.Traces.ListenAddr != new.Traces.ListenAddr)
	add("traces.max_request_bytes", old.Traces.MaxRequestBytes != new.Traces.MaxRequestBytes)
	add("traces.auth_token_env", old.Traces.AuthTokenEnv != new.Traces.AuthTokenEnv)
	// Detection runs once at startup, so a change here cannot take effect on a
	// running agent — say so rather than letting it look applied.
	add("ec2_metadata", old.EC2Metadata.DetectionEnabled() != new.EC2Metadata.DetectionEnabled() ||
		old.EC2Metadata.Timeout != new.EC2Metadata.Timeout)
	add("exporter", old.Exporter.Type != new.Exporter.Type ||
		old.Exporter.Endpoint != new.Exporter.Endpoint ||
		old.Exporter.Path != new.Exporter.Path)

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
