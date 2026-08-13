package collector

import (
	"context"
	"strings"
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
func TestReadNetworkIOStates_RealProcNetDev(t *testing.T) {
	samples, err := readNetworkIOStates()
	if err != nil {
		t.Fatalf("readNetworkIOStates: %v", err)
	}
	if len(samples) == 0 {
		t.Fatal("no network interfaces found — expected at least loopback")
	}

	foundLoDirections := map[string]bool{}
	for _, s := range samples {
		if s.bytes < 0 {
			t.Errorf("negative byte count for %s/%s: %v", s.device, s.direction, s.bytes)
		}
		if s.direction != "transmit" && s.direction != "receive" {
			t.Errorf("unexpected direction %q for device %s", s.direction, s.device)
		}
		if s.device == "lo" {
			foundLoDirections[s.direction] = true
		}
	}
	if !foundLoDirections["transmit"] || !foundLoDirections["receive"] {
		t.Errorf("expected both transmit and receive for loopback interface, got: %+v", foundLoDirections)
	}
}

func TestReadFilesystemUsageStates_RealProcMounts(t *testing.T) {
	samples, err := readFilesystemUsageStates()
	if err != nil {
		t.Fatalf("readFilesystemUsageStates: %v", err)
	}
	if len(samples) == 0 {
		t.Fatal("no real filesystems found — expected at least the root filesystem")
	}

	foundRoot := false
	statesByMount := map[string]map[string]float64{}
	for _, s := range samples {
		if s.bytes < 0 {
			t.Errorf("negative bytes for %s state=%s: %v", s.mountpoint, s.state, s.bytes)
		}
		if s.mountpoint == "/" {
			foundRoot = true
		}
		if statesByMount[s.mountpoint] == nil {
			statesByMount[s.mountpoint] = map[string]float64{}
		}
		statesByMount[s.mountpoint][s.state] = s.bytes
	}
	if !foundRoot {
		t.Errorf("expected to find the root filesystem (/), got mountpoints: %+v", statesByMount)
	}
	// Sanity: used+free for root should be a non-trivial number of bytes
	// (a real disk, not an empty/zeroed stub).
	rootTotal := statesByMount["/"]["used"] + statesByMount["/"]["free"]
	const oneGB = 1024 * 1024 * 1024
	if rootTotal < oneGB {
		t.Errorf("root filesystem used+free = %v bytes, suspiciously small for a real disk", rootTotal)
	}
}

func TestReadLoadAverage_RealProcLoadavg(t *testing.T) {
	load, err := readLoadAverage()
	if err != nil {
		t.Fatalf("readLoadAverage: %v", err)
	}
	for _, window := range []string{"1m", "5m", "15m"} {
		v, ok := load[window]
		if !ok {
			t.Errorf("missing %s load average", window)
			continue
		}
		if v < 0 {
			t.Errorf("%s load average is negative: %v", window, v)
		}
	}
}

func TestInfraHostMetricsCollector_EndToEnd(t *testing.T) {
	coll := NewInfraHostMetricsCollector("test-agent", 200*time.Millisecond)
	out := make(chan Envelope, 200)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := coll.Start(ctx, out); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer coll.Stop()

	var cpuStates, memStates, netSamples, fsSamples, loadSamples int
	timeout := time.After(2 * time.Second)
	for cpuStates < 8 || memStates < 4 || netSamples < 1 || fsSamples < 1 || loadSamples < 1 {
		select {
		case env := <-out:
			if env.Kind != KindMetric {
				t.Errorf("Kind = %q, want metric", env.Kind)
			}
			switch {
			case env.Source == "system.cpu.time":
				cpuStates++
				if env.Labels["cpu"] != "cpu-total" {
					t.Errorf("system.cpu.time missing/wrong cpu label: %+v", env.Labels)
				}
				if env.Labels["state"] == "" {
					t.Errorf("system.cpu.time missing state label: %+v", env.Labels)
				}
			case env.Source == "system.memory.usage":
				memStates++
				if env.Labels["state"] == "" {
					t.Errorf("system.memory.usage missing state label: %+v", env.Labels)
				}
			case env.Source == "system.network.io":
				netSamples++
				if env.Labels["device"] == "" || env.Labels["direction"] == "" {
					t.Errorf("system.network.io missing device/direction label: %+v", env.Labels)
				}
			case env.Source == "system.filesystem.usage":
				fsSamples++
				if env.Labels["mountpoint"] == "" || env.Labels["state"] == "" {
					t.Errorf("system.filesystem.usage missing mountpoint/state label: %+v", env.Labels)
				}
			case strings.HasPrefix(env.Source, "system.cpu.load_average."):
				loadSamples++
			default:
				t.Errorf("unexpected envelope source: %s", env.Source)
			}
		case <-timeout:
			t.Fatalf("timed out — got cpu=%d mem=%d net=%d fs=%d load=%d (want >=8, >=4, >=1, >=1, >=1)",
				cpuStates, memStates, netSamples, fsSamples, loadSamples)
		}
	}
}
