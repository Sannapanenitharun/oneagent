package collector

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// HostMetricsCollector polls basic host metrics on a fixed interval.
// Deliberately reads /proc directly instead of pulling in a system-stats
// library: this is Linux-only by design (matches the daemon deployment
// target) and keeps the binary's dependency surface — and therefore its
// supply-chain attack surface — minimal.
type HostMetricsCollector struct {
	agentID  string
	interval time.Duration
	metrics  map[string]bool // which of cpu/memory/disk are enabled
	stop     chan struct{}
	gate     *sendGate

	// prevCPU is the previous /proc/stat reading. CPU utilisation is a
	// difference between two points in time, and holding the previous one lets
	// that difference span the whole interval — see sampleCPU. Touched only by
	// the collector's own goroutine, so no lock.
	prevCPU     cpuStat
	havePrevCPU bool
}

func NewHostMetricsCollector(agentID string, interval time.Duration, enabled []string) *HostMetricsCollector {
	m := make(map[string]bool, len(enabled))
	for _, e := range enabled {
		m[e] = true
	}
	return &HostMetricsCollector{
		gate:     newSendGate("host.metrics"),
		agentID:  agentID,
		interval: interval,
		metrics:  m,
		stop:     make(chan struct{}),
	}
}

func (h *HostMetricsCollector) Name() string { return "host.metrics" }

func (h *HostMetricsCollector) Start(ctx context.Context, out chan<- Envelope) error {
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
				h.sample(ctx, out)
			}
		}
	}()
	return nil
}

func (h *HostMetricsCollector) Stop() error {
	close(h.stop)
	return nil
}

func (h *HostMetricsCollector) sample(ctx context.Context, out chan<- Envelope) {
	now := time.Now().UTC()

	if h.metrics["memory"] {
		if used, total, err := readMemory(); err == nil && total > 0 {
			h.gate.send(ctx, out, Envelope{
				Kind: KindMetric, AgentID: h.agentID, Source: "host.memory.used_pct",
				Timestamp: now, Value: (used / total) * 100,
			})
		}
	}

	if h.metrics["cpu"] {
		if pct, ok := h.sampleCPU(); ok {
			h.gate.send(ctx, out, Envelope{
				Kind: KindMetric, AgentID: h.agentID, Source: "host.cpu.used_pct",
				Timestamp: now, Value: pct,
			})
		}
	}
}

// sampleCPU returns utilisation across the whole interval since the previous
// sample, and whether a value is available yet.
//
// It used to take two readings 100ms apart and report the difference between
// them. That measured 100ms out of every interval — at the default 15s, 0.67%
// of elapsed time — so a workload that spiked between samples was invisible and
// the number was noisy even when nothing was wrong. It also parked the
// collector's goroutine in a sleep on every tick.
//
// Differencing against the previous tick instead covers 100% of the elapsed
// time, which is both the accurate answer and the one consistent with
// system.cpu.time, the cumulative counter this collector's sibling already
// emits from the same /proc/stat fields.
//
// The cost is that the very first tick after startup has nothing to difference
// against and reports nothing. That is the honest result: no time has elapsed
// under observation yet, and inventing a 100ms stand-in for it was the old
// behaviour's whole problem.
func (h *HostMetricsCollector) sampleCPU() (float64, bool) {
	cur, err := readCPUStat()
	if err != nil {
		return 0, false
	}
	return h.deltaCPU(cur)
}

// deltaCPU is sampleCPU's arithmetic, split out so it can be exercised without
// a /proc/stat to read.
func (h *HostMetricsCollector) deltaCPU(cur cpuStat) (float64, bool) {
	prev := h.prevCPU
	had := h.havePrevCPU
	h.prevCPU = cur
	h.havePrevCPU = true

	if !had {
		return 0, false
	}

	idleDelta := cur.idle - prev.idle
	totalDelta := cur.total - prev.total
	if totalDelta <= 0 {
		// Counters did not move: either two reads landed inside the same clock
		// tick, or the file went backwards after a suspend. Either way there is
		// no interval to report on.
		return 0, false
	}
	pct := (1 - idleDelta/totalDelta) * 100
	// Clamp rather than emit an impossible reading. Deltas can go slightly out
	// of range across a suspend/resume or a CPU hotplug event.
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct, true
}

// readMemory returns (used, total) in KB from /proc/meminfo.
func readMemory() (used, total float64, err error) {
	f, err := os.Open(hostPath("/proc/meminfo"))
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	vals := map[string]float64{}
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
		vals[key] = v
	}
	total = vals["MemTotal"]
	available := vals["MemAvailable"]
	if total == 0 {
		return 0, 0, fmt.Errorf("MemTotal not found")
	}
	used = total - available
	return used, total, nil
}

type cpuStat struct{ idle, total float64 }

func readCPUStat() (cpuStat, error) {
	f, err := os.Open(hostPath("/proc/stat"))
	if err != nil {
		return cpuStat{}, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return cpuStat{}, fmt.Errorf("empty /proc/stat")
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuStat{}, fmt.Errorf("unexpected /proc/stat format")
	}

	var total float64
	var idle float64
	for i, f := range fields[1:] {
		v, err := strconv.ParseFloat(f, 64)
		if err != nil {
			continue
		}
		total += v
		if i == 3 { // idle is the 4th value (index 3)
			idle = v
		}
	}
	return cpuStat{idle: idle, total: total}, nil
}
