// Command agent is the Agent-I telemetry collector daemon entrypoint.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/agent-i/agent/internal/collector"
	"github.com/agent-i/agent/internal/config"
	"github.com/agent-i/agent/internal/daemon"
	"github.com/agent-i/agent/internal/version"
)

func main() {
	configPath := flag.String("config", "/etc/agent-i/agent.yaml", "path to agent config file")
	showVersion := flag.Bool("version", false, "print the build version and exit")
	checkOnly := flag.Bool("check", false, "validate the config file, report what it enables, and exit")
	flag.Parse()

	// Answered before the config is read, so this still works on a host
	// whose config is missing or malformed — which is exactly when you
	// most need to know what is installed.
	if *showVersion {
		fmt.Println(version.Version)
		return
	}

	if *checkOnly {
		os.Exit(check(*configPath))
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	d, err := daemon.New(cfg)
	if err != nil {
		log.Fatalf("startup error: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// SIGHUP re-reads the config file. Settings that can be applied to a
	// running agent (log paths, aggregation, trace sampling and stats) take
	// effect immediately; the daemon logs anything that needs a restart.
	//
	// A config that fails to parse is reported and discarded — the agent keeps
	// running on the configuration it already has. Exiting here would mean a
	// typo during a routine edit costs you all telemetry from the host, which
	// is a far worse outcome than running briefly on stale settings.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			newCfg, err := config.Load(*configPath)
			if err != nil {
				log.Printf("reload: %v — keeping the current configuration", err)
				continue
			}
			log.Printf("reload: re-read %s", *configPath)
			d.Reload(newCfg)
		}
	}()

	// d.AgentID(), not cfg.AgentID: the configured value is empty on any host
	// that names itself from its hostname or its EC2 Name tag, which is the
	// default and therefore the common case.
	log.Printf("agent-i starting (version=%s, agent_id=%s, interval=%s)", version.Version, d.AgentID(), cfg.Interval)
	if err := d.Run(ctx); err != nil {
		log.Fatalf("daemon error: %v", err)
	}
	log.Println("agent-i shut down cleanly")
	os.Exit(0)
}

// check validates a configuration file and reports what it would run, without
// starting anything. Returns the process exit code.
//
// Deliberately does NOT construct the daemon. daemon.New binds the dashboard
// listener, and the moment this runs is during an upgrade, with the current
// agent still up — so a full construction would fail with "address in use" on
// a perfectly healthy host. A check that cries wolf during the one operation
// it exists to protect is worse than no check at all.
//
// What it does cover is the failure that actually took a host down: a config
// file the running parser accepts and a newer, stricter one rejects. v2.1.0
// added duplicate-key detection and crash-looped an EC2 instance 72 times on a
// file v2.0.3 had been reading for weeks. Run before the restart, this turns
// that into a refusal with a line number.
func check(path string) int {
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config INVALID: %s\n  %v\n", path, err)
		return 1
	}

	// Compiled exactly as the daemon compiles it. This is the one new way
	// v2.2.0 can reject a file that earlier versions parsed without complaint,
	// because they did not read the block at all.
	if _, err := collector.NewInterfaceFilter(
		collector.InterfaceMatch{
			Interfaces: cfg.Metrics.Network.Include.Interfaces,
			MatchType:  cfg.Metrics.Network.Include.MatchType,
		},
		collector.InterfaceMatch{
			Interfaces: cfg.Metrics.Network.Exclude.Interfaces,
			MatchType:  cfg.Metrics.Network.Exclude.MatchType,
		},
	); err != nil {
		fmt.Fprintf(os.Stderr, "config INVALID: %s\n  metrics.network: %v\n", path, err)
		return 1
	}

	enabled := []string{}
	add := func(on bool, name string) {
		if on {
			enabled = append(enabled, name)
		}
	}
	add(cfg.Metrics.Enabled, "metrics")
	add(cfg.Logs.Enabled, "logs")
	add(cfg.Journald.Enabled, "journald")
	add(cfg.Containers.Enabled, "containers")
	add(cfg.AccessLogs.Enabled, "access_logs")
	add(cfg.Processes.Enabled, "processes")
	add(cfg.Traces.Enabled, "traces")

	fmt.Printf("config OK: %s\n", path)
	fmt.Printf("  version    %s\n", version.Version)
	if len(enabled) == 0 {
		// Not an error here — daemon.New is what refuses to start — but saying
		// so is the difference between a silent no-op host and an obvious one.
		fmt.Printf("  collectors none enabled — the agent will refuse to start\n")
	} else {
		fmt.Printf("  collectors %s\n", strings.Join(enabled, " "))
	}

	// Attribution, when it is configured. An app label that names a key no
	// container carries is the silent failure this reports: telemetry keeps
	// flowing, every container keeps reporting under the host's identity, and
	// nothing looks wrong. The collector says how many containers actually
	// resolved once it is running; this says whether it will try at all.
	if cfg.Containers.Enabled {
		if cfg.Containers.AppLabel != "" {
			fmt.Printf("  app label  %s → service.name\n", cfg.Containers.AppLabel)
		} else {
			fmt.Printf("  app label  unset — every container reports as the host\n")
		}
	}
	if len(cfg.ResourceAttributes) > 0 {
		keys := make([]string, 0, len(cfg.ResourceAttributes))
		for k := range cfg.ResourceAttributes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+cfg.ResourceAttributes[k])
		}
		fmt.Printf("  attributes %s\n", strings.Join(parts, " "))
	}

	// Printed last and unconditionally, because this is the line that would
	// have ended a multi-day investigation in seconds. A typo in the exporter
	// key ("oexporter:") meant the whole block was never read and the type
	// silently defaulted to stdout: the agent looked healthy, collected
	// everything, and threw all of it away.
	switch cfg.Exporter.Type {
	case "stdout", "":
		fmt.Printf("  exporter   stdout — telemetry is written to the log and DISCARDED\n")
	case "file":
		fmt.Printf("  exporter   file → %s\n", cfg.Exporter.Path)
	default:
		fmt.Printf("  exporter   %s → %s\n", cfg.Exporter.Type, cfg.Exporter.Endpoint)
	}
	return 0
}
