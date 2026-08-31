package exporter

import (
	"testing"

	"github.com/agent-i/agent/internal/config"
)

func attrMap(r otlpResource) map[string]string {
	out := make(map[string]string, len(r.Attributes))
	for _, a := range r.Attributes {
		if a.Value.StringValue != nil {
			out[a.Key] = *a.Value.StringValue
		}
	}
	return out
}

// EC2 attributes discovered at startup must ride on the resource of every
// exported signal — that is what makes a backend show the host as a specific
// instance rather than an anonymous Linux box.
func TestResourceFor_CarriesDiscoveredAttributes(t *testing.T) {
	exp, err := newOTLPHTTPExporter(config.ExporterConfig{
		Endpoint: "http://127.0.0.1:1",
		ResourceAttributes: map[string]string{
			"cloud.provider":          "aws",
			"cloud.platform":          "aws_ec2",
			"cloud.account.id":        "123456789012",
			"cloud.region":            "us-east-1",
			"cloud.availability_zone": "us-east-1a",
			"host.id":                 "i-0123456789abcdef0",
			"host.type":               "t3.medium",
		},
	})
	if err != nil {
		t.Fatalf("constructing exporter: %v", err)
	}
	defer exp.Close()

	got := attrMap(exp.resourceFor("checkout"))

	for k, want := range map[string]string{
		"cloud.provider":          "aws",
		"cloud.platform":          "aws_ec2",
		"cloud.account.id":        "123456789012",
		"cloud.region":            "us-east-1",
		"cloud.availability_zone": "us-east-1a",
		"host.type":               "t3.medium",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	// The attributes that were always there must survive.
	if got["service.name"] != "checkout" {
		t.Errorf("service.name = %q", got["service.name"])
	}
	if got["telemetry.distro.name"] != "agent-i" {
		t.Errorf("telemetry.distro.name = %q", got["telemetry.distro.name"])
	}
}

// On EC2 the semantic conventions define host.id as the instance id, so a
// discovered value has to win over /etc/machine-id. Emitting both would be two
// conflicting claims about the same key.
func TestResourceFor_DiscoveredHostIDWins(t *testing.T) {
	exp, err := newOTLPHTTPExporter(config.ExporterConfig{
		Endpoint:           "http://127.0.0.1:1",
		ResourceAttributes: map[string]string{"host.id": "i-0123456789abcdef0"},
	})
	if err != nil {
		t.Fatalf("constructing exporter: %v", err)
	}
	defer exp.Close()

	res := exp.resourceFor("svc")
	seen := 0
	for _, a := range res.Attributes {
		if a.Key == "host.id" {
			seen++
			if a.Value.StringValue == nil || *a.Value.StringValue != "i-0123456789abcdef0" {
				t.Errorf("host.id = %v, want the instance id", a.Value.StringValue)
			}
		}
	}
	if seen != 1 {
		t.Errorf("host.id appears %d times, want exactly 1", seen)
	}
}

// Off EC2 nothing is discovered, and the resource must look exactly as it did
// before this feature existed.
func TestResourceFor_NoDiscoveryIsUnchanged(t *testing.T) {
	exp, err := newOTLPHTTPExporter(config.ExporterConfig{Endpoint: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("constructing exporter: %v", err)
	}
	defer exp.Close()

	got := attrMap(exp.resourceFor("svc"))
	for _, k := range []string{"cloud.provider", "cloud.region", "host.type", "cloud.account.id"} {
		if _, present := got[k]; present {
			t.Errorf("%s present without detection", k)
		}
	}
	if got["service.name"] != "svc" || got["os.type"] == "" {
		t.Errorf("baseline attributes disturbed: %v", got)
	}
}

// Resources are rebuilt per flush, so an unstable ordering would make otherwise
// identical payloads differ between batches.
func TestResourceFor_AttributeOrderIsStable(t *testing.T) {
	cfg := config.ExporterConfig{
		Endpoint: "http://127.0.0.1:1",
		ResourceAttributes: map[string]string{
			"cloud.provider": "aws", "cloud.region": "us-east-1",
			"host.type": "t3.medium", "cloud.account.id": "1", "host.id": "i-1",
		},
	}
	exp, err := newOTLPHTTPExporter(cfg)
	if err != nil {
		t.Fatalf("constructing exporter: %v", err)
	}
	defer exp.Close()

	first := exp.resourceFor("svc")
	for i := 0; i < 20; i++ {
		next := exp.resourceFor("svc")
		if len(next.Attributes) != len(first.Attributes) {
			t.Fatalf("attribute count changed between calls")
		}
		for j := range first.Attributes {
			if next.Attributes[j].Key != first.Attributes[j].Key {
				t.Fatalf("attribute order changed at %d: %q vs %q", j, next.Attributes[j].Key, first.Attributes[j].Key)
			}
		}
	}
}

// A resource must never carry the same attribute twice.
//
// Every other test here reads attributes through attrMap, which collapses a
// duplicate into one entry and so cannot see this class of fault at all. It
// went unnoticed that way: discovery began supplying host.name from the EC2
// Name tag while resourceFor still appended its own unconditionally, and the
// resource carried two host.name attributes with different values. OTLP does
// not say which one a backend keeps, so the host's name depended on the reader.
func TestResourceFor_NoDuplicateAttributes(t *testing.T) {
	cfg := config.ExporterConfig{
		Endpoint: "http://127.0.0.1:1",
		ResourceAttributes: map[string]string{
			"host.name":      "prod-web-01",
			"host.id":        "i-0123456789abcdef0",
			"cloud.provider": "aws",
		},
	}
	exp, err := newOTLPHTTPExporter(cfg)
	if err != nil {
		t.Fatalf("constructing exporter: %v", err)
	}
	defer exp.Close()

	counts := map[string]int{}
	for _, a := range exp.resourceFor("svc").Attributes {
		counts[a.Key]++
	}
	for key, n := range counts {
		if n > 1 {
			t.Errorf("attribute %q appears %d times in one resource", key, n)
		}
	}

	// And the discovered values are the ones that survived, not the agent's
	// own fallbacks — the whole point of detecting them.
	got := attrMap(exp.resourceFor("svc"))
	if got["host.name"] != "prod-web-01" {
		t.Errorf("host.name = %q, want the discovered Name tag", got["host.name"])
	}
	if got["host.id"] != "i-0123456789abcdef0" {
		t.Errorf("host.id = %q, want the discovered instance id", got["host.id"])
	}
}

// With nothing discovered the agent still has to name the host, or telemetry
// arrives with no host identity at all.
func TestResourceFor_HostNameFallsBackWithoutDiscovery(t *testing.T) {
	exp, err := newOTLPHTTPExporter(config.ExporterConfig{Endpoint: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("constructing exporter: %v", err)
	}
	defer exp.Close()

	if got := attrMap(exp.resourceFor("svc")); got["host.name"] == "" {
		t.Error("host.name absent with no discovered value to replace it")
	}
}
