package collector

import (
	"strings"
	"testing"
)

func mustFilter(t *testing.T, include, exclude InterfaceMatch) *InterfaceFilter {
	t.Helper()
	f, err := NewInterfaceFilter(include, exclude)
	if err != nil {
		t.Fatalf("NewInterfaceFilter: %v", err)
	}
	return f
}

// The default. All three agents this one is measured against collect every
// interface unless told otherwise, and so does an absent configuration block.
func TestInterfaceFilter_EmptyCollectsEverything(t *testing.T) {
	f := mustFilter(t, InterfaceMatch{}, InterfaceMatch{})
	for _, dev := range []string{"eth0", "lo", "veth1a2b3c", "br-78b709cc60b4", "docker0", "ens5"} {
		if !f.Allow(dev) {
			t.Errorf("Allow(%q) = false; an unconfigured filter must drop nothing", dev)
		}
	}
	// A nil filter is the same thing, reached through a collector that was
	// never given one.
	var nilF *InterfaceFilter
	if !nilF.Allow("eth0") {
		t.Error("a nil filter dropped an interface; it must collect everything")
	}
}

// The case this was built for: a container host where every veth is a worse
// copy of a per-container metric that already carries the container's name.
func TestInterfaceFilter_ExcludeRegexp(t *testing.T) {
	f := mustFilter(t, InterfaceMatch{}, InterfaceMatch{
		Interfaces: []string{"^veth", "^br-", "^docker"},
		MatchType:  "regexp",
	})
	dropped := []string{"veth1a2b3c", "vethb3f21a9", "br-78b709cc60b4", "docker0"}
	kept := []string{"eth0", "ens5", "eno1", "lo", "wlan0"}
	for _, dev := range dropped {
		if f.Allow(dev) {
			t.Errorf("Allow(%q) = true, want it excluded", dev)
		}
	}
	for _, dev := range kept {
		if !f.Allow(dev) {
			t.Errorf("Allow(%q) = false, want it kept", dev)
		}
	}
}

// Strict compares whole names. An interface whose name merely contains a rule
// must survive, or "eth0" would take "veth0" with it.
func TestInterfaceFilter_StrictIsExact(t *testing.T) {
	f := mustFilter(t, InterfaceMatch{}, InterfaceMatch{
		Interfaces: []string{"lo", "docker0"},
		MatchType:  "strict",
	})
	if f.Allow("lo") || f.Allow("docker0") {
		t.Error("a strictly named interface was not excluded")
	}
	for _, dev := range []string{"lo0", "vlo", "docker01", "eth0"} {
		if !f.Allow(dev) {
			t.Errorf("Allow(%q) = false; strict matching must compare whole names", dev)
		}
	}
}

// Include answers "only these", and is applied before exclude.
func TestInterfaceFilter_IncludeIsAnAllowlist(t *testing.T) {
	f := mustFilter(t, InterfaceMatch{
		Interfaces: []string{"eth0", "ens5"},
		MatchType:  "strict",
	}, InterfaceMatch{})
	if !f.Allow("eth0") || !f.Allow("ens5") {
		t.Error("an included interface was dropped")
	}
	for _, dev := range []string{"eth1", "veth1a2b3c", "lo"} {
		if f.Allow(dev) {
			t.Errorf("Allow(%q) = true; only included interfaces may pass", dev)
		}
	}
}

// Named by both, the removal wins — it is the more specific instruction.
func TestInterfaceFilter_ExcludeBeatsInclude(t *testing.T) {
	f := mustFilter(t,
		InterfaceMatch{Interfaces: []string{"^e"}, MatchType: "regexp"},
		InterfaceMatch{Interfaces: []string{"eth1"}, MatchType: "strict"},
	)
	if !f.Allow("eth0") {
		t.Error("eth0 matched include and no exclude, but was dropped")
	}
	if f.Allow("eth1") {
		t.Error("eth1 was excluded explicitly but the include rule kept it")
	}
}

// The failure this refuses to allow. A regex compared as a literal name matches
// nothing, so a filter written as ["^veth"] with strict matching would look
// configured and collect every veth on the host. Requiring match_type turns
// that into a startup error instead of a discovery weeks later.
func TestInterfaceFilter_MatchTypeIsRequired(t *testing.T) {
	_, err := NewInterfaceFilter(InterfaceMatch{}, InterfaceMatch{Interfaces: []string{"^veth"}})
	if err == nil {
		t.Fatal("interfaces with no match_type were accepted; the filter would silently do nothing")
	}
	if !strings.Contains(err.Error(), "match_type") {
		t.Errorf("error = %q, want it to name match_type", err)
	}
	// And it must say which pattern it is talking about, so the fix is obvious.
	if !strings.Contains(err.Error(), "^veth") {
		t.Errorf("error = %q, want it to quote the offending pattern", err)
	}
}

func TestInterfaceFilter_RejectsBadInput(t *testing.T) {
	if _, err := NewInterfaceFilter(InterfaceMatch{}, InterfaceMatch{
		Interfaces: []string{"[unclosed"}, MatchType: "regexp",
	}); err == nil {
		t.Error("an invalid regular expression was accepted")
	}
	if _, err := NewInterfaceFilter(InterfaceMatch{}, InterfaceMatch{
		Interfaces: []string{"eth0"}, MatchType: "fuzzy",
	}); err == nil {
		t.Error("an unknown match_type was accepted")
	}
	// A match_type with nothing to match is harmless: it selects nothing.
	if _, err := NewInterfaceFilter(InterfaceMatch{}, InterfaceMatch{MatchType: "regexp"}); err != nil {
		t.Errorf("an empty interface list was rejected: %v", err)
	}
}

// The startup line has to distinguish "collecting everything" from "filtering",
// because those look identical in a dashboard and have opposite fixes.
func TestInterfaceFilter_Describe(t *testing.T) {
	if got := mustFilter(t, InterfaceMatch{}, InterfaceMatch{}).Describe(); got != "every interface" {
		t.Errorf("Describe() = %q for an empty filter", got)
	}
	got := mustFilter(t, InterfaceMatch{}, InterfaceMatch{
		Interfaces: []string{"^veth", "^br-"}, MatchType: "regexp",
	}).Describe()
	if !strings.Contains(got, "2 exclude") {
		t.Errorf("Describe() = %q, want it to report the rule count", got)
	}
}
