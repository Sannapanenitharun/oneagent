package daemon

import (
	"os"
	"testing"
)

// A configured id is an operator saying so explicitly, and must win over
// anything discovered about the host.
func TestResolveAgentID_ConfiguredWins(t *testing.T) {
	attrs := map[string]string{"host.name": "prod-web-01", "host.id": "i-0123456789abcdef0"}
	if got := resolveAgentID("chosen-by-hand", attrs); got != "chosen-by-hand" {
		t.Errorf("resolveAgentID = %q, want the configured value", got)
	}
}

// The shipped config carries an empty agent_id, and a hand-edited one can
// easily end up holding only whitespace. Both mean "not set".
func TestResolveAgentID_BlankConfigFallsThrough(t *testing.T) {
	attrs := map[string]string{"host.id": "i-0123456789abcdef0"}
	for _, configured := range []string{"", "   ", "\t\n"} {
		if got := resolveAgentID(configured, attrs); got != "i-0123456789abcdef0" {
			t.Errorf("resolveAgentID(%q) = %q, want the instance id", configured, got)
		}
	}
}

// The Name tag is what people actually call an instance, so it outranks the id.
func TestResolveAgentID_PrefersNameTagOverInstanceID(t *testing.T) {
	attrs := map[string]string{"host.name": "prod-web-01", "host.id": "i-0123456789abcdef0"}
	if got := resolveAgentID("", attrs); got != "prod-web-01" {
		t.Errorf("resolveAgentID = %q, want the Name tag", got)
	}
}

// Tags are not exposed through IMDS by default, so this is the common EC2 case:
// no name, but an instance id that is always present and always unique.
func TestResolveAgentID_InstanceIDWhenNoNameTag(t *testing.T) {
	attrs := map[string]string{
		"cloud.provider": "aws",
		"host.id":        "i-0123456789abcdef0",
		"host.type":      "t3.medium",
	}
	if got := resolveAgentID("", attrs); got != "i-0123456789abcdef0" {
		t.Errorf("resolveAgentID = %q, want the instance id", got)
	}
}

// Off EC2 nothing is detected and the hostname is all that is left.
func TestResolveAgentID_HostnameOffEC2(t *testing.T) {
	want, err := os.Hostname()
	if err != nil {
		t.Skipf("no hostname available: %v", err)
	}
	if got := resolveAgentID("", nil); got != want {
		t.Errorf("resolveAgentID = %q, want the hostname %q", got, want)
	}
}

// The one thing that must never happen: every host in a fleet reporting under
// the same id, which is what a hardcoded default in the shipped config did.
// Two hosts differing in any identifying attribute must resolve differently.
func TestResolveAgentID_DistinguishesHosts(t *testing.T) {
	a := resolveAgentID("", map[string]string{"host.id": "i-aaaaaaaaaaaaaaaaa"})
	b := resolveAgentID("", map[string]string{"host.id": "i-bbbbbbbbbbbbbbbbb"})
	if a == b {
		t.Errorf("two instances resolved to the same id %q", a)
	}
}

// Whatever happens, the result is usable: an empty agent id would label every
// signal the agent emits with nothing at all.
func TestResolveAgentID_NeverEmpty(t *testing.T) {
	cases := []map[string]string{nil, {}, {"host.name": ""}, {"host.id": ""}}
	for _, attrs := range cases {
		if got := resolveAgentID("", attrs); got == "" {
			t.Errorf("resolveAgentID(%v) returned an empty id", attrs)
		}
	}
}
