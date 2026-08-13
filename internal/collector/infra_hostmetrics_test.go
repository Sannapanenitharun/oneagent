package collector

import (
	"context"
	"testing"
	"time"
)

func TestReadCPUTimeStates_RealProcStat(t *testing.T) {
	states, err := readCPUTimeStates()
	if err != nil {
		t.Fatalf("readCPUTimeStates: %v", err)
	}

	for _, want := range cpuTimeStateOrder {
		v, ok := states[want]
		if !ok {
			t.Errorf("missing expected state %q in result: %+v", want, states)
			continue
		}
		if v < 0 {
			t.Errorf("state %q has negative value %v — cumulative counters should never be negative", want, v)
		}
	}

	// idle time should be a substantial, non-trivial number of seconds on
	// any host that's been running for more than a few seconds — this
	// isn't a delta, it's cumulative since boot.
	if states["idle"] <= 0 {
		t.Errorf("idle = %v, expected a positive cumulative value", states["idle"])
	}
}

func TestReadMemoryUsageStates_RealProcMeminfo(t *testing.T) {
	states, err := readMemoryUsageStates()
	if err != nil {
		t.Fatalf("readMemoryUsageStates: %v", err)
	}

	for _, want := range []string{"used", "free", "buffered", "cached"} {
		v, ok := states[want]
		if !ok {
			t.Errorf("missing expected state %q in result: %+v", want, states)
			continue
		}
		if v < 0 {
			t.Errorf("state %q has negative value %v", want, v)
		}
	}

	// A real running Linux system always has SOME memory in use — this
	// catches a totally broken parse (e.g. reading the wrong field)
	// that would silently produce all-zero output.
	if states["used"] <= 0 {
		t.Errorf("used = %v, expected a positive value on a real running system", states["used"])
	}

	// Sanity bound: on this sandbox specifically, total memory is a few
	// GB — used+free+buffered+cached should be in that same rough order
	// of magnitude, not wildly off (e.g. not still in KB when bytes were
	// expected, which would be 1024x too small).
	sum := states["used"] + states["free"] + states["buffered"] + states["cached"]
	const oneGB = 1024 * 1024 * 1024
	if sum < oneGB/10 {
		t.Errorf("sum of memory states = %v bytes, suspiciously small — check the KB-to-bytes conversion", sum)
	}
}

// TestInfraHostMetricsCollector_EndToEnd runs the actual collector for
// real and confirms it emits both system.cpu.time (8 states) and
// system.memory.usage (4 states) envelopes with the correct Source names
// and Labels — the same shape SigNoz's Infrastructure page requires.
func TestInfraHostMetricsCollector_EndToEnd(t *testing.T) {
	coll := NewInfraHostMetricsCollector("test-agent", 200*time.Millisecond)
	out := make(chan Envelope, 50)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := coll.Start(ctx, out); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer coll.Stop()

	var cpuStates, memStates int
	timeout := time.After(2 * time.Second)
	for cpuStates < 8 || memStates < 4 {
		select {
		case env := <-out:
			if env.Kind != KindMetric {
				t.Errorf("Kind = %q, want metric", env.Kind)
			}
			switch env.Source {
			case "system.cpu.time":
				cpuStates++
				if env.Labels["cpu"] != "cpu-total" {
					t.Errorf("system.cpu.time missing/wrong cpu label: %+v", env.Labels)
				}
				if env.Labels["state"] == "" {
					t.Errorf("system.cpu.time missing state label: %+v", env.Labels)
				}
			case "system.memory.usage":
				memStates++
				if env.Labels["state"] == "" {
					t.Errorf("system.memory.usage missing state label: %+v", env.Labels)
				}
			default:
				t.Errorf("unexpected envelope source: %s", env.Source)
			}
		case <-timeout:
			t.Fatalf("timed out — got %d cpu states, %d memory states (want 8 and 4)", cpuStates, memStates)
		}
	}
}
