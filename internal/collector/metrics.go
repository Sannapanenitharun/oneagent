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
		if pct, err := readCPUPercent(); err == nil {
			h.gate.send(ctx, out, Envelope{
				Kind: KindMetric, AgentID: h.agentID, Source: "host.cpu.used_pct",
				Timestamp: now, Value: pct,
			})
		}
	}
}

// readMemory returns (used, total) in KB from /proc/meminfo.
func readMemory() (used, total float64, err error) {
	f, err := os.Open("/proc/meminfo")
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

// readCPUPercent does a short (100ms) two-sample delta against
// /proc/stat's aggregate cpu line to compute instantaneous utilization.
func readCPUPercent() (float64, error) {
	s1, err := readCPUStat()
	if err != nil {
		return 0, err
	}
	time.Sleep(100 * time.Millisecond)
	s2, err := readCPUStat()
	if err != nil {
		return 0, err
	}

	idleDelta := s2.idle - s1.idle
	totalDelta := s2.total - s1.total
	if totalDelta <= 0 {
		return 0, nil
	}
	return (1 - idleDelta/totalDelta) * 100, nil
}

type cpuStat struct{ idle, total float64 }

func readCPUStat() (cpuStat, error) {
	f, err := os.Open("/proc/stat")
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
