package collector

import (
	"bufio"
	"context"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// InfraHostMetricsCollector emits metrics using the EXACT names, types, and
// attributes the standard OpenTelemetry hostmetrics receiver produces —
// specifically system.cpu.time and system.memory.usage. This is a
// separate collector from HostMetricsCollector (which emits our own
// simpler host.cpu.used_pct / host.memory.used_pct) rather than a
// replacement, for two reasons: (1) it doesn't disturb existing behavior
// or dashboards built on the original metrics, and (2) the data models
// are genuinely different — system.cpu.time is a cumulative counter
// since boot, not a point-in-time percentage, so it isn't a drop-in
// substitute.
//
// This exists specifically because SigNoz's Infrastructure Monitoring
// > Hosts page requires this exact schema to populate at all — confirmed
// against SigNoz's own docs (signoz.io/docs/infrastructure-monitoring/
// reference/telemetry-data-requirements/), not guessed. Our own
// custom-named metrics, however correct, don't satisfy it.
type InfraHostMetricsCollector struct {
	agentID  string
	interval time.Duration
	stop     chan struct{}
}

func NewInfraHostMetricsCollector(agentID string, interval time.Duration) *InfraHostMetricsCollector {
	return &InfraHostMetricsCollector{
		agentID:  agentID,
		interval: interval,
		stop:     make(chan struct{}),
	}
}

func (h *InfraHostMetricsCollector) Name() string { return "infra.hostmetrics" }

func (h *InfraHostMetricsCollector) Start(ctx context.Context, out chan<- Envelope) error {
	ticker := time.NewTicker(h.interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-h.stop:
				return
			case <-ticker.C:
				h.sample(out)
			}
		}
	}()
	return nil
}

func (h *InfraHostMetricsCollector) Stop() error {
	close(h.stop)
	return nil
}

func (h *InfraHostMetricsCollector) sample(out chan<- Envelope) {
	now := time.Now().UTC()
	bootTimeUnix := readBootTimeUnix() // "" if unavailable — handled downstream

	if states, err := readCPUTimeStates(); err == nil {
		for state, seconds := range states {
			labels := map[string]string{"state": state, "cpu": "cpu-total"}
			if bootTimeUnix != "" {
				// Consumed by the OTLP exporter to set startTimeUnixNano
				// on this metric's Sum data point (OTel requires a
				// cumulative counter to declare when it started
				// accumulating) — not a real CPU-state attribute, so the
				// exporter strips it back out before sending.
				labels["_boot_time_unix"] = bootTimeUnix
			}
			out <- Envelope{
				Kind:      KindMetric,
				AgentID:   h.agentID,
				Source:    "system.cpu.time",
				Timestamp: now,
				Value:     seconds,
				Labels:    labels,
			}
		}
	}

	if states, err := readMemoryUsageStates(); err == nil {
		for state, bytes := range states {
			out <- Envelope{
				Kind:      KindMetric,
				AgentID:   h.agentID,
				Source:    "system.memory.usage",
				Timestamp: now,
				Value:     bytes,
				Labels:    map[string]string{"state": state},
			}
		}
	}

	if ifaces, err := readNetworkIOStates(); err == nil {
		for _, s := range ifaces {
			labels := map[string]string{"device": s.device, "direction": s.direction}
			if bootTimeUnix != "" {
				labels["_boot_time_unix"] = bootTimeUnix
			}
			out <- Envelope{
				Kind:      KindMetric,
				AgentID:   h.agentID,
				Source:    "system.network.io",
				Timestamp: now,
				Value:     s.bytes,
				Labels:    labels,
			}
		}
	}

	if mounts, err := readFilesystemUsageStates(); err == nil {
		for _, m := range mounts {
			out <- Envelope{
				Kind:      KindMetric,
				AgentID:   h.agentID,
				Source:    "system.filesystem.usage",
				Timestamp: now,
				Value:     m.bytes,
				Labels: map[string]string{
					"state":      m.state,
					"mountpoint": m.mountpoint,
					"device":     m.device,
					"type":       m.fstype,
				},
			}
		}
	}

	if load, err := readLoadAverage(); err == nil {
		for window, val := range load {
			out <- Envelope{
				Kind:      KindMetric,
				AgentID:   h.agentID,
				Source:    "system.cpu.load_average." + window,
				Timestamp: now,
				Value:     val,
			}
		}
	}
}

// networkIOSample is one (interface, direction) cumulative byte counter.
type networkIOSample struct {
	device    string
	direction string // "transmit" or "receive"
	bytes     float64
}

// readNetworkIOStates parses /proc/net/dev, returning CUMULATIVE bytes
// transmitted/received per interface since boot — same Sum/cumulative
// model as CPU time, for the same reason: these counters only ever
// increase.
//
// /proc/net/dev's column layout after the interface name is 8 receive
// fields followed by 8 transmit fields; bytes is the first field in each
// group (index 0 and index 8).
func readNetworkIOStates() ([]networkIOSample, error) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var samples []networkIOSample
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if lineNum <= 2 {
			continue // two header lines
		}
		line := scanner.Text()
		colonIdx := strings.Index(line, ":")
		if colonIdx < 0 {
			continue
		}
		device := strings.TrimSpace(line[:colonIdx])
		fields := strings.Fields(line[colonIdx+1:])
		if len(fields) < 16 {
			continue
		}
		rxBytes, errR := strconv.ParseFloat(fields[0], 64)
		txBytes, errT := strconv.ParseFloat(fields[8], 64)
		if errR == nil {
			samples = append(samples, networkIOSample{device: device, direction: "receive", bytes: rxBytes})
		}
		if errT == nil {
			samples = append(samples, networkIOSample{device: device, direction: "transmit", bytes: txBytes})
		}
	}
	return samples, nil
}

// filesystemUsageSample is one (mountpoint, state) byte value.
type filesystemUsageSample struct {
	mountpoint string
	device     string
	fstype     string
	state      string // "used" or "free"
	bytes      float64
}

// realFilesystemTypes is an allowlist of filesystem types we consider
// "real" storage worth reporting on — deliberately excludes virtual/
// pseudo filesystems (proc, sysfs, tmpfs, cgroup, overlay, network/FUSE
// mounts, etc.) that would otherwise produce noisy, meaningless disk
// usage entries. This is an allowlist rather than an exclude-list
// because the set of virtual filesystem types is large and grows over
// time; a real disk's fstype is one of a small, stable set.
var realFilesystemTypes = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true,
	"xfs": true, "btrfs": true, "zfs": true,
	"vfat": true, "ntfs": true, "exfat": true, "apfs": true,
}

// readFilesystemUsageStates parses /proc/mounts for real filesystems and
// calls statfs(2) on each mountpoint to get used/free bytes.
func readFilesystemUsageStates() ([]filesystemUsageSample, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var samples []filesystemUsageSample
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		device, mountpoint, fstype := fields[0], fields[1], fields[2]
		if !realFilesystemTypes[fstype] {
			continue
		}

		var stat syscall.Statfs_t
		if err := syscall.Statfs(mountpoint, &stat); err != nil {
			continue // transient mounts, permission issues — skip rather than fail the whole sample
		}
		blockSize := float64(stat.Bsize)
		total := float64(stat.Blocks) * blockSize
		free := float64(stat.Bavail) * blockSize
		used := total - float64(stat.Bfree)*blockSize
		if used < 0 {
			used = 0
		}

		samples = append(samples,
			filesystemUsageSample{mountpoint: mountpoint, device: device, fstype: fstype, state: "used", bytes: used},
			filesystemUsageSample{mountpoint: mountpoint, device: device, fstype: fstype, state: "free", bytes: free},
		)
	}
	return samples, nil
}

// readLoadAverage parses /proc/loadavg's first three fields — 1/5/15
// minute load averages — a simple, non-cumulative Gauge, unlike
// everything else read from /proc/stat in this file.
func readLoadAverage() (map[string]float64, error) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(b))
	if len(fields) < 3 {
		return nil, os.ErrInvalid
	}
	result := make(map[string]float64, 3)
	for i, window := range []string{"1m", "5m", "15m"} {
		v, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			continue
		}
		result[window] = v
	}
	return result, nil
}

// readBootTimeUnix reads the "btime" line from /proc/stat — seconds
// since the Unix epoch when the system booted. This is what makes
// system.cpu.time's cumulative counters meaningful: they've been
// accumulating since exactly this moment, which OTel requires a Sum
// metric to declare via startTimeUnixNano. Returns "" (not an error) on
// any failure, since this is a supplementary field — losing it shouldn't
// stop the actual CPU metrics from being collected.
func readBootTimeUnix() string {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[0] == "btime" {
			return fields[1]
		}
	}
	return ""
}

// cpuTimeStateOrder is the field order in /proc/stat's aggregate "cpu"
// line. Some kernels append guest/guest_nice columns beyond these eight;
// we only read the standard eight, which is what the OTel hostmetrics
// receiver itself reports by default.
var cpuTimeStateOrder = []string{"user", "nice", "system", "idle", "iowait", "irq", "softirq", "steal"}

// readCPUTimeStates reads /proc/stat's aggregate cpu line and returns
// CUMULATIVE seconds spent in each state since boot — not a delta, not a
// percentage. This maps directly onto OTel's Sum/cumulative model: /proc/
// stat's jiffie counters themselves only ever increase, exactly like a
// monotonic OTel counter is defined to behave.
func readCPUTimeStates() (map[string]float64, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return nil, os.ErrInvalid
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 1+len(cpuTimeStateOrder) || fields[0] != "cpu" {
		return nil, os.ErrInvalid
	}

	const userHZ = 100.0 // jiffies per second — 100 on virtually all Linux distros (getconf CLK_TCK)
	states := make(map[string]float64, len(cpuTimeStateOrder))
	for i, name := range cpuTimeStateOrder {
		jiffies, err := strconv.ParseFloat(fields[i+1], 64)
		if err != nil {
			continue
		}
		states[name] = jiffies / userHZ
	}
	return states, nil
}

// readMemoryUsageStates reads /proc/meminfo and returns a state
// breakdown in bytes, matching the OTel hostmetrics receiver's
// system.memory.usage states (used/free/buffered/cached). Values are
// converted from /proc/meminfo's native KB to bytes.
func readMemoryUsageStates() (map[string]float64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	raw := map[string]float64{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		v, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		raw[key] = v
	}

	total, free, buffers, cached := raw["MemTotal"], raw["MemFree"], raw["Buffers"], raw["Cached"]
	if total == 0 {
		return nil, os.ErrInvalid
	}
	used := total - free - buffers - cached
	if used < 0 {
		used = 0 // guard against an unusual /proc/meminfo snapshot producing a negative value
	}

	const kbToBytes = 1024.0
	return map[string]float64{
		"used":     used * kbToBytes,
		"free":     free * kbToBytes,
		"buffered": buffers * kbToBytes,
		"cached":   cached * kbToBytes,
	}, nil
}
