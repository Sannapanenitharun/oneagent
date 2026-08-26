// Command agent is the Agent-I telemetry collector daemon entrypoint.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/agent-i/agent/internal/config"
	"github.com/agent-i/agent/internal/daemon"
	"github.com/agent-i/agent/internal/version"
)

func main() {
	configPath := flag.String("config", "/etc/agent-i/agent.yaml", "path to agent config file")
	showVersion := flag.Bool("version", false, "print the build version and exit")
	flag.Parse()

	// Answered before the config is read, so this still works on a host
	// whose config is missing or malformed — which is exactly when you
	// most need to know what is installed.
	if *showVersion {
		fmt.Println(version.Version)
		return
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
