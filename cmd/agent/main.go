// Command agent is the OneAgent telemetry collector daemon entrypoint.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/oneagent/agent/internal/config"
	"github.com/oneagent/agent/internal/daemon"
)

func main() {
	configPath := flag.String("config", "/etc/oneagent-agent/agent.yaml", "path to agent config file")
	flag.Parse()

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

	log.Printf("oneagent-agent starting (agent_id=%s, interval=%s)", cfg.AgentID, cfg.Interval)
	if err := d.Run(ctx); err != nil {
		log.Fatalf("daemon error: %v", err)
	}
	log.Println("oneagent-agent shut down cleanly")
	os.Exit(0)
}
