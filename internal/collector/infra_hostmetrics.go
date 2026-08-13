package collector

import (
	"bufio"
	"context"
	"os"
	"strconv"
	"strings"
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
