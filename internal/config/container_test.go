package config

import (
	"net"
	"os"
	"regexp"
	"testing"
)

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

// The shipped config must not name a routable address in its examples.
//
// It used to. The commented endpoint carried a real public IP, sitting one line
// under the type: field — which is precisely the line an operator uncomments
// when switching an agent off stdout. Uncommenting it without also editing it
// ships that host's metrics, logs and trace contents to a machine its owner has
// no relationship with, and nothing about the agent's behaviour would look
// wrong while it happened.
//
// Loopback, link-local (IMDS) and RFC1918 are fine: none of them leaves the
// operator's own network. Anything else is not an example, it is a destination.
func TestShippedConfig_HasNoRoutableExampleAddress(t *testing.T) {
	raw, err := os.ReadFile("../../configs/agent.yaml")
	if err != nil {
		t.Fatalf("reading shipped config: %v", err)
	}

	// Any dotted quad appearing in a URL.
	re := regexp.MustCompile(`https?://(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})`)
	for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
		ip := net.ParseIP(m[1])
		if ip == nil {
			t.Errorf("unparseable address in the shipped config: %q", m[1])
			continue
		}
		if ip.IsLoopback() || ip.IsUnspecified() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
			continue
		}
		t.Errorf("shipped config names the routable address %s — an operator who uncomments "+
			"that line without editing it sends this host's telemetry to a stranger. Use a "+
			"reserved documentation name such as backend.example.com instead.", m[1])
	}
}
