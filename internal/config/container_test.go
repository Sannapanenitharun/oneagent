package config

import "testing"

// The container image bakes deploy/agent-container.yaml in at build time, so a
// typo in it does not surface until someone runs the image and the agent exits
// on startup. This parses it the same way the agent does and asserts the
// handful of settings that are the reason the file exists at all.
func TestLoad_ContainerConfigParses(t *testing.T) {
	cfg, err := Load("../../deploy/agent-container.yaml")
	if err != nil {
		t.Fatalf("loading deploy/agent-container.yaml: %v", err)
	}

	if !cfg.Containers.Enabled {
		t.Error("containers.enabled is false in the image that exists to collect containers")
	}
	if !cfg.Containers.CollectLogs() {
		t.Error("container log collection is off")
	}
	if cfg.Containers.Logs.Root != "/var/lib/docker/containers" {
		t.Errorf("containers.logs.root = %q; it must be the HOST path, which the agent "+
			"prefixes with AGENT_I_HOST_ROOT — pre-prefixing it here would double the prefix",
			cfg.Containers.Logs.Root)
	}

	// Loopback would be unreachable from an application container on the same
	// network AND unpublishable with -p, since docker forwards to the
	// container's interface. Both listeners have to bind 0.0.0.0 inside the
	// container for the image to be usable at all.
	if cfg.Traces.ListenAddr != "0.0.0.0:4318" {
		t.Errorf("traces.listen_addr = %q, want 0.0.0.0:4318 — an app container "+
			"cannot reach a receiver bound to loopback", cfg.Traces.ListenAddr)
	}
	if cfg.Dashboard.ListenAddr != "0.0.0.0:8088" {
		t.Errorf("dashboard.listen_addr = %q, want 0.0.0.0:8088", cfg.Dashboard.ListenAddr)
	}

	// journalctl is not in the image. Enabling it would restart-loop the reader
	// against a binary that does not exist.
	if cfg.Journald.Enabled {
		t.Error("journald.enabled is true but the image has no journalctl")
	}

	// State has to land on the declared volume or a recreate loses every read
	// offset and the agent re-sends or skips log files.
	if cfg.Tailing.RegistryPath != "/var/lib/agent-i/registry.json" {
		t.Errorf("tailing.registry_path = %q, want it under the /var/lib/agent-i volume",
			cfg.Tailing.RegistryPath)
	}
	if cfg.Exporter.Spool.Dir != "/var/lib/agent-i/spool" {
		t.Errorf("exporter.spool.dir = %q, want it under the /var/lib/agent-i volume",
			cfg.Exporter.Spool.Dir)
	}
}
