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

func TestReadNetworkPacketStates_RealProcNetDev(t *testing.T) {
	samples, err := readNetworkPacketStates()
	if err != nil {
		t.Fatalf("readNetworkPacketStates: %v", err)
	}
	if len(samples) == 0 {
		t.Fatal("no interfaces found")
	}
	foundLo := map[string]bool{}
	for _, s := range samples {
		if s.packets < 0 {
			t.Errorf("negative packets for %s/%s: %v", s.device, s.direction, s.packets)
		}
		if s.device == "lo" {
			foundLo[s.direction] = true
		}
	}
	if !foundLo["transmit"] || !foundLo["receive"] {
		t.Errorf("expected both directions for loopback, got: %+v", foundLo)
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

func TestReadNetworkErrorsAndDrops_RealProcNetDev(t *testing.T) {
	samples, err := readNetworkErrorsAndDrops()
	if err != nil {
		t.Fatalf("readNetworkErrorsAndDrops: %v", err)
	}
	if len(samples) == 0 {
		t.Fatal("no interfaces found")
	}
	foundLo := map[string]bool{}
	for _, s := range samples {
		if s.errors < 0 || s.dropped < 0 {
			t.Errorf("negative errors/dropped for %s/%s: errors=%v dropped=%v", s.device, s.direction, s.errors, s.dropped)
		}
		if s.device == "lo" {
			foundLo[s.direction] = true
		}
	}
	if !foundLo["transmit"] || !foundLo["receive"] {
		t.Errorf("expected both directions for loopback, got: %+v", foundLo)
	}
}

func TestReadNetworkConnectionStates_RealProcNetTCP(t *testing.T) {
	states, err := readNetworkConnectionStates()
	if err != nil {
		t.Fatalf("readNetworkConnectionStates: %v", err)
	}
	// Every value must be non-negative and every key must be a real,
	// recognized TCP state name — catches a parsing bug that would
	// otherwise silently produce garbage state labels.
	validStates := map[string]bool{}
	for _, name := range tcpStateNames {
		validStates[name] = true
	}
	for state, count := range states {
		if !validStates[state] {
			t.Errorf("unrecognized TCP state name in result: %q", state)
		}
		if count < 0 {
			t.Errorf("negative connection count for state %q: %v", state, count)
		}
	}
}

func TestReadDiskIOStates_RealProcDiskstats(t *testing.T) {
	samples, err := readDiskIOStates()
	if err != nil {
		t.Fatalf("readDiskIOStates: %v", err)
	}
	if len(samples) == 0 {
		t.Fatal("no real disk devices found — expected at least one non-virtual block device")
	}
	for _, s := range samples {
		if isVirtualDisk(s.device) {
			t.Errorf("virtual device %q leaked through the exclude filter", s.device)
		}
		if s.readBytes < 0 || s.writeBytes < 0 || s.readOps < 0 || s.writeOps < 0 ||
			s.readTimeSec < 0 || s.writeTimeSec < 0 || s.pendingOps < 0 {
			t.Errorf("negative value in disk sample for %s: %+v", s.device, s)
		}
	}
}

func TestIsVirtualDisk(t *testing.T) {
	cases := map[string]bool{
		"loop0": true, "loop12": true, "ram0": true, "zram0": true,
		"dm-0": true, "sr0": true,
		"vda": false, "sda": false, "nvme0n1": false, "xvda": false,
	}
	for device, want := range cases {
		if got := isVirtualDisk(device); got != want {
			t.Errorf("isVirtualDisk(%q) = %v, want %v", device, got, want)
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

	var cpuStates, memStates, netSamples, fsSamples, loadSamples, netErrSamples, netConnSamples, diskSamples int
	timeout := time.After(2 * time.Second)
	for cpuStates < 8 || memStates < 4 || netSamples < 1 || fsSamples < 1 || loadSamples < 1 ||
		netErrSamples < 1 || netConnSamples < 1 || diskSamples < 1 {
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
			case env.Source == "system.network.packets":
				netSamples++
				if env.Labels["device"] == "" || env.Labels["direction"] == "" {
					t.Errorf("system.network.packets missing device/direction label: %+v", env.Labels)
				}
			case env.Source == "system.network.errors" || env.Source == "system.network.dropped":
				netErrSamples++
				if env.Labels["device"] == "" || env.Labels["direction"] == "" {
					t.Errorf("%s missing device/direction label: %+v", env.Source, env.Labels)
				}
			case env.Source == "system.network.connections":
				netConnSamples++
				if env.Labels["protocol"] != "tcp" || env.Labels["state"] == "" {
					t.Errorf("system.network.connections missing protocol/state label: %+v", env.Labels)
				}
			case env.Source == "system.disk.io" || env.Source == "system.disk.operations" ||
				env.Source == "system.disk.operation_time" || env.Source == "system.disk.pending_operations":
				diskSamples++
				if env.Labels["device"] == "" {
					t.Errorf("%s missing device label: %+v", env.Source, env.Labels)
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
			t.Fatalf("timed out — got cpu=%d mem=%d net=%d fs=%d load=%d netErr=%d netConn=%d disk=%d (all need >=1, cpu>=8, mem>=4)",
				cpuStates, memStates, netSamples, fsSamples, loadSamples, netErrSamples, netConnSamples, diskSamples)
		}
	}
}
