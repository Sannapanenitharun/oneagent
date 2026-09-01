package collector

import (
	"fmt"
	"regexp"
	"strings"
)

// Deciding which network interfaces are worth measuring.
//
// Two designs were considered, taken from the two agents this one is measured
// against. Datadog filters at the agent: excluded_interfaces and
// excluded_interface_re, both empty by default, so an operator names what to
// drop and nothing leaves the host. Dynatrace collects everything and enforces
// a cardinality limit at the backend, rejecting data points that carry a
// dimension tuple it has not seen before once a per-metric ceiling is reached.
//
// This agent filters at the agent, for three reasons.
//
// The backend half already exists here and does not need a second layer: the
// store caps a snapshot fairly across metric names and reports what it refused,
// so a truncated view is visible rather than silent. Adding an ingest-side
// tuple limit would put the same policy at a second layer, in a component that
// is currently stateless per request and would have to grow a thirty-day
// sliding window to do it.
//
// Filtering at the source removes the cost everywhere at once — export
// bandwidth, stored rows, and the snapshot budget — rather than only at the
// last of the three.
//
// And "reject tuples not seen before" has a failure mode this agent would hit.
// It is first-come-first-served: on a host that churns containers, the veth
// interfaces that happened to appear first would hold the budget permanently
// and lock out a real interface added later. An explicit exclusion cannot do
// that, because it names what to drop rather than racing for what to keep.
//
// The shape is OpenTelemetry's hostmetrics receiver — include, exclude and
// match_type — because this agent already emits system.network.* under those
// semantic conventions, and an operator who knows one should not have to learn
// another.

// InterfaceMatch selects interfaces by name.
type InterfaceMatch struct {
	Interfaces []string
	// MatchType is "strict" or "regexp". There is deliberately no default: a
	// regex evaluated as a literal name matches nothing, so a filter written as
	// ["^veth"] against strict matching would silently collect everything it
	// was meant to drop. Requiring the choice makes that a startup error
	// instead of a discovery weeks later.
	MatchType string
}

func (m InterfaceMatch) empty() bool { return len(m.Interfaces) == 0 }

// InterfaceFilter decides whether one interface is collected.
//
// The zero value collects everything, which is what an absent configuration
// block means and what all three of Datadog, Dynatrace and the OTel collector
// do by default. Nothing is dropped unless an operator said to drop it.
type InterfaceFilter struct {
	include []*regexp.Regexp
	exclude []*regexp.Regexp
	// Strict sets are kept separate rather than being compiled into anchored
	// patterns, so an interface name containing regex metacharacters cannot
	// change what a strict rule matches.
	includeExact map[string]bool
	excludeExact map[string]bool
}

// NewInterfaceFilter compiles include and exclude rules.
func NewInterfaceFilter(include, exclude InterfaceMatch) (*InterfaceFilter, error) {
	f := &InterfaceFilter{}
	var err error
	if f.include, f.includeExact, err = compileMatch("include", include); err != nil {
		return nil, err
	}
	if f.exclude, f.excludeExact, err = compileMatch("exclude", exclude); err != nil {
		return nil, err
	}
	return f, nil
}

func compileMatch(which string, m InterfaceMatch) ([]*regexp.Regexp, map[string]bool, error) {
	if m.empty() {
		// A match_type with no interfaces is harmless — it selects nothing —
		// so it is not an error. The reverse is.
		return nil, nil, nil
	}
	switch m.MatchType {
	case "strict":
		exact := make(map[string]bool, len(m.Interfaces))
		for _, s := range m.Interfaces {
			exact[s] = true
		}
		return nil, exact, nil
	case "regexp":
		out := make([]*regexp.Regexp, 0, len(m.Interfaces))
		for _, s := range m.Interfaces {
			re, err := regexp.Compile(s)
			if err != nil {
				return nil, nil, fmt.Errorf("metrics.network.%s: %q is not a valid regular expression: %w", which, s, err)
			}
			out = append(out, re)
		}
		return out, nil, nil
	case "":
		return nil, nil, fmt.Errorf("metrics.network.%s lists interfaces but sets no match_type; "+
			"it must be \"strict\" (exact names) or \"regexp\" (patterns). Without it a pattern "+
			"such as %q would be compared as a literal interface name, match nothing, and the "+
			"filter would silently do nothing", which, m.Interfaces[0])
	default:
		return nil, nil, fmt.Errorf("metrics.network.%s: match_type is %q; it must be \"strict\" or \"regexp\"",
			which, m.MatchType)
	}
}

// Allow reports whether an interface's metrics should be collected.
//
// Include is applied first and exclude second, matching the OTel receiver: an
// include list answers "only these", and an exclude list then removes from
// whatever survived. An interface named by both is excluded, because the more
// specific instruction is the one that removes.
func (f *InterfaceFilter) Allow(device string) bool {
	if f == nil {
		return true
	}
	if len(f.include) > 0 || len(f.includeExact) > 0 {
		if !f.matches(device, f.include, f.includeExact) {
			return false
		}
	}
	if f.matches(device, f.exclude, f.excludeExact) {
		return false
	}
	return true
}

func (f *InterfaceFilter) matches(device string, res []*regexp.Regexp, exact map[string]bool) bool {
	if exact[device] {
		return true
	}
	for _, re := range res {
		if re.MatchString(device) {
			return true
		}
	}
	return false
}

// Describe renders the active rules for the startup log, so an operator can see
// what the agent decided to measure without reading the config back.
func (f *InterfaceFilter) Describe() string {
	if f == nil || (len(f.include) == 0 && len(f.includeExact) == 0 && len(f.exclude) == 0 && len(f.excludeExact) == 0) {
		return "every interface"
	}
	var parts []string
	if n := len(f.include) + len(f.includeExact); n > 0 {
		parts = append(parts, fmt.Sprintf("%d include rule(s)", n))
	}
	if n := len(f.exclude) + len(f.excludeExact); n > 0 {
		parts = append(parts, fmt.Sprintf("%d exclude rule(s)", n))
	}
	return strings.Join(parts, ", ")
}
