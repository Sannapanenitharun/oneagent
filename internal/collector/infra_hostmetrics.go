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
// This exists because a backend's host-inventory view matches on the
// hostmetrics receiver's exact metric names, types and attributes, and
// populates only when it finds them — confirmed against a backend's
// documented data requirements, not guessed. Our own custom-named
// metrics, however correct, don't satisfy that match.
type InfraHostMetricsCollector struct {
	agentID  string
	interval time.Duration
	stop     chan struct{}
	gate     *sendGate
}

func NewInfraHostMetricsCollector(agentID string, interval time.Duration) *InfraHostMetricsCollector {
	return &InfraHostMetricsCollector{
		gate:     newSendGate("infra.hostmetrics"),
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
				h.sample(ctx, out)
			}
		}
	}()
	return nil
}

func (h *InfraHostMetricsCollector) Stop() error {
	close(h.stop)
	return nil
}

func (h *InfraHostMetricsCollector) sample(ctx context.Context, out chan<- Envelope) {
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
			h.gate.send(ctx, out, Envelope{
				Kind:      KindMetric,
				AgentID:   h.agentID,
				Source:    "system.cpu.time",
				Timestamp: now,
				Value:     seconds,
				Labels:    labels,
			})
		}
	}

	if states, err := readMemoryUsageStates(); err == nil {
		for state, bytes := range states {
			h.gate.send(ctx, out, Envelope{
				Kind:      KindMetric,
				AgentID:   h.agentID,
				Source:    "system.memory.usage",
				Timestamp: now,
				Value:     bytes,
				Labels:    map[string]string{"state": state},
			})
		}
	}

	if ifaces, err := readNetworkIOStates(); err == nil {
		for _, s := range ifaces {
			labels := map[string]string{"device": s.device, "direction": s.direction}
			if bootTimeUnix != "" {
				labels["_boot_time_unix"] = bootTimeUnix
			}
			h.gate.send(ctx, out, Envelope{
				Kind:      KindMetric,
				AgentID:   h.agentID,
				Source:    "system.network.io",
				Timestamp: now,
				Value:     s.bytes,
				Labels:    labels,
			})
		}
	}

	if pkts, err := readNetworkPacketStates(); err == nil {
		for _, s := range pkts {
			labels := map[string]string{"device": s.device, "direction": s.direction}
			if bootTimeUnix != "" {
				labels["_boot_time_unix"] = bootTimeUnix
			}
			h.gate.send(ctx, out, Envelope{
				Kind:      KindMetric,
				AgentID:   h.agentID,
				Source:    "system.network.packets",
				Timestamp: now,
				Value:     s.packets,
				Labels:    labels,
			})
		}
	}

	if errs, err := readNetworkErrorsAndDrops(); err == nil {
		for _, s := range errs {
			labels := map[string]string{"device": s.device, "direction": s.direction}
			if bootTimeUnix != "" {
				labels["_boot_time_unix"] = bootTimeUnix
			}
			h.gate.send(ctx, out, Envelope{Kind: KindMetric, AgentID: h.agentID, Source: "system.network.errors", Timestamp: now, Value: s.errors, Labels: labels})
			h.gate.send(ctx, out, Envelope{Kind: KindMetric, AgentID: h.agentID, Source: "system.network.dropped", Timestamp: now, Value: s.dropped, Labels: mapCopy(labels)})
		}
	}

	if conns, err := readNetworkConnectionStates(); err == nil {
		for state, count := range conns {
			h.gate.send(ctx, out, Envelope{
				Kind:      KindMetric,
				AgentID:   h.agentID,
				Source:    "system.network.connections",
				Timestamp: now,
				Value:     count,
				Labels:    map[string]string{"protocol": "tcp", "state": state},
			})
		}
	}

	if disks, err := readDiskIOStates(); err == nil {
		for _, d := range disks {
			readLabels := map[string]string{"device": d.device, "direction": "read"}
			writeLabels := map[string]string{"device": d.device, "direction": "write"}
			if bootTimeUnix != "" {
				readLabels["_boot_time_unix"] = bootTimeUnix
				writeLabels["_boot_time_unix"] = bootTimeUnix
			}
			h.gate.send(ctx, out, Envelope{Kind: KindMetric, AgentID: h.agentID, Source: "system.disk.io", Timestamp: now, Value: d.readBytes, Labels: readLabels})
			h.gate.send(ctx, out, Envelope{Kind: KindMetric, AgentID: h.agentID, Source: "system.disk.io", Timestamp: now, Value: d.writeBytes, Labels: writeLabels})
			h.gate.send(ctx, out, Envelope{Kind: KindMetric, AgentID: h.agentID, Source: "system.disk.operations", Timestamp: now, Value: d.readOps, Labels: mapCopy(readLabels)})
			h.gate.send(ctx, out, Envelope{Kind: KindMetric, AgentID: h.agentID, Source: "system.disk.operations", Timestamp: now, Value: d.writeOps, Labels: mapCopy(writeLabels)})
			h.gate.send(ctx, out, Envelope{Kind: KindMetric, AgentID: h.agentID, Source: "system.disk.operation_time", Timestamp: now, Value: d.readTimeSec, Labels: mapCopy(readLabels)})
			h.gate.send(ctx, out, Envelope{Kind: KindMetric, AgentID: h.agentID, Source: "system.disk.operation_time", Timestamp: now, Value: d.writeTimeSec, Labels: mapCopy(writeLabels)})
			// pending_operations is a point-in-time queue depth, not
			// cumulative — no direction split, no boot-time label needed.
			h.gate.send(ctx, out, Envelope{Kind: KindMetric, AgentID: h.agentID, Source: "system.disk.pending_operations", Timestamp: now, Value: d.pendingOps, Labels: map[string]string{"device": d.device}})
		}
	}

	if mounts, err := readFilesystemUsageStates(); err == nil {
		for _, m := range mounts {
			h.gate.send(ctx, out, Envelope{
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
			})
		}
	}

	if load, err := readLoadAverage(); err == nil {
		for window, val := range load {
			h.gate.send(ctx, out, Envelope{
				Kind:      KindMetric,
				AgentID:   h.agentID,
				Source:    "system.cpu.load_average." + window,
				Timestamp: now,
				Value:     val,
			})
		}
	}
}

// networkPacketSample is one (interface, direction) cumulative packet
// counter — same file, same per-interface layout as networkIOSample,
// just the packets column (index 1 for receive, index 9 for transmit)
// instead of bytes (index 0/8).
type networkPacketSample struct {
	device    string
	direction string
	packets   float64
}

func readNetworkPacketStates() ([]networkPacketSample, error) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var samples []networkPacketSample
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if lineNum <= 2 {
			continue
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
		rxPackets, errR := strconv.ParseFloat(fields[1], 64)
		txPackets, errT := strconv.ParseFloat(fields[9], 64)
		if errR == nil {
			samples = append(samples, networkPacketSample{device: device, direction: "receive", packets: rxPackets})
		}
		if errT == nil {
			samples = append(samples, networkPacketSample{device: device, direction: "transmit", packets: txPackets})
		}
	}
	return samples, nil
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
//
// One device is reported once, under its shortest mountpoint. A filesystem can
// appear in /proc/mounts many times — every bind mount of it is another line —
// and statfs answers for the whole filesystem regardless of which path you ask
// about, so those lines carry byte-for-byte identical numbers. Emitting all of
// them multiplies the series count and, worse, makes any sum over mountpoints
// count the same disk several times.
//
// This is not a hypothetical: the agent's own systemd unit causes it. PrivateTmp
// creates /tmp and /var/tmp, and each ReadWritePaths entry creates another bind
// mount, so a packaged agent saw its root filesystem five times while a dev run
// or a container — with none of that hardening — saw it once. That is why no
// test caught it.
//
// Shortest mountpoint wins because it is the real mount; the longer paths are
// the binds hanging off it. Deduplicating by device rather than by the reported
// numbers is deliberate — two filesystems that happen to be the same size are
// still two filesystems, and must not collapse into one.
func readFilesystemUsageStates() ([]filesystemUsageSample, error) {
	return readFilesystemUsageStatesFrom("/proc/mounts", syscall.Statfs)
}

// readFilesystemUsageStatesFrom is the testable body: the mounts file and the
// statfs call are both parameters. Reading /proc/mounts directly is what let
// the duplicate-bind-mount bug ship — the behaviour could only be observed on
// a host whose mount table already had the problem, which no test environment
// did.
func readFilesystemUsageStatesFrom(mountsPath string, statfs func(string, *syscall.Statfs_t) error) ([]filesystemUsageSample, error) {
	f, err := os.Open(mountsPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	type mount struct {
		mountpoint string
		fstype     string
		used, free float64
	}
	// Insertion order is kept separately: ranging a map would reorder the
	// output on every collection, which fragments series keys downstream.
	byDevice := make(map[string]mount)
	var order []string

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

		if prev, seen := byDevice[device]; seen && len(prev.mountpoint) <= len(mountpoint) {
			continue // already have this filesystem under an equal or shorter path
		}

		var stat syscall.Statfs_t
		if err := statfs(mountpoint, &stat); err != nil {
			continue // transient mounts, permission issues — skip rather than fail the whole sample
		}
		blockSize := float64(stat.Bsize)
		total := float64(stat.Blocks) * blockSize
		free := float64(stat.Bavail) * blockSize
		used := total - float64(stat.Bfree)*blockSize
		if used < 0 {
			used = 0
		}

		if _, seen := byDevice[device]; !seen {
			order = append(order, device)
		}
		byDevice[device] = mount{mountpoint: mountpoint, fstype: fstype, used: used, free: free}
	}

	samples := make([]filesystemUsageSample, 0, len(order)*2)
	for _, device := range order {
		m := byDevice[device]
		samples = append(samples,
			filesystemUsageSample{mountpoint: m.mountpoint, device: device, fstype: m.fstype, state: "used", bytes: m.used},
			filesystemUsageSample{mountpoint: m.mountpoint, device: device, fstype: m.fstype, state: "free", bytes: m.free},
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

// mapCopy returns a shallow copy of a label map — needed because Go maps
// are reference types, and several envelopes below reuse the "same"
// label set with one key changed; without copying, mutating one
// envelope's labels would retroactively corrupt another's.
func mapCopy(m map[string]string) map[string]string {
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

// networkErrDropSample is one (interface, direction) cumulative error or
// drop count.
type networkErrDropSample struct {
	device    string
	direction string
	errors    float64
	dropped   float64
}

// readNetworkErrorsAndDrops parses /proc/net/dev's errs/drop columns —
// same file and per-interface layout as readNetworkIOStates, just
// different column offsets. Kept as a separate function rather than
// merged into readNetworkIOStates to keep each function's return type
// simple and single-purpose.
func readNetworkErrorsAndDrops() ([]networkErrDropSample, error) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var samples []networkErrDropSample
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if lineNum <= 2 {
			continue
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
		rxErrs, _ := strconv.ParseFloat(fields[2], 64)
		rxDrop, _ := strconv.ParseFloat(fields[3], 64)
		txErrs, _ := strconv.ParseFloat(fields[10], 64)
		txDrop, _ := strconv.ParseFloat(fields[11], 64)
		samples = append(samples,
			networkErrDropSample{device: device, direction: "receive", errors: rxErrs, dropped: rxDrop},
			networkErrDropSample{device: device, direction: "transmit", errors: txErrs, dropped: txDrop},
		)
	}
	return samples, nil
}

// tcpStateNames maps /proc/net/tcp's hex "st" field to the standard
// Linux TCP state names (include/net/tcp_states.h), matching what OTel's
// system.network.connections metric expects as its "state" attribute.
var tcpStateNames = map[string]string{
	"01": "ESTABLISHED", "02": "SYN_SENT", "03": "SYN_RECV",
	"04": "FIN_WAIT1", "05": "FIN_WAIT2", "06": "TIME_WAIT",
	"07": "CLOSE", "08": "CLOSE_WAIT", "09": "LAST_ACK",
	"0A": "LISTEN", "0B": "CLOSING",
}

// readNetworkConnectionStates parses /proc/net/tcp and /proc/net/tcp6,
// returning a count of connections per TCP state. This is a point-in-time
// Gauge, not cumulative — connection counts go up and down constantly.
func readNetworkConnectionStates() (map[string]float64, error) {
	counts := make(map[string]float64, len(tcpStateNames))
	found := false
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(path)
		if err != nil {
			continue // tcp6 may not exist on IPv4-only hosts — not fatal
		}
		found = true
		scanner := bufio.NewScanner(f)
		lineNum := 0
		for scanner.Scan() {
			lineNum++
			if lineNum == 1 {
				continue // header line
			}
			fields := strings.Fields(scanner.Text())
			if len(fields) < 4 {
				continue
			}
			if name, ok := tcpStateNames[fields[3]]; ok {
				counts[name]++
			}
		}
		f.Close()
	}
	if !found {
		return nil, os.ErrNotExist
	}
	return counts, nil
}

// diskIOSample aggregates one block device's stats from /proc/diskstats.
type diskIOSample struct {
	device                    string
	readBytes, writeBytes     float64 // cumulative since boot
	readOps, writeOps         float64 // cumulative since boot
	readTimeSec, writeTimeSec float64 // cumulative since boot
	pendingOps                float64 // point-in-time queue depth
}

// virtualDiskPrefixes excludes loop devices, ram disks, zram (swap
// compression), and device-mapper internals — none of these represent
// real physical/block storage worth reporting on. Unlike the filesystem
// allowlist above, this is an EXCLUDE list, because real disk device
// naming varies too much across platforms/cloud providers (sda, vda,
// nvme0n1, xvda, hda...) to enumerate as an allowlist.
var virtualDiskPrefixes = []string{"loop", "ram", "zram", "dm-", "sr", "fd"}

func isVirtualDisk(device string) bool {
	for _, p := range virtualDiskPrefixes {
		if strings.HasPrefix(device, p) {
			return true
		}
	}
	return false
}

// readDiskIOStates parses /proc/diskstats. Field layout (1-indexed, this
// function uses 0-indexed slice positions after splitting):
// major minor device reads_completed reads_merged sectors_read
// time_reading writes_completed writes_merged sectors_written
// time_writing ios_in_progress time_ios weighted_time_ios [...discards]
// Sector size is always 512 bytes regardless of the device's actual
// block size — this is a longstanding Linux kernel convention, not
// something read from the device itself.
func readDiskIOStates() ([]diskIOSample, error) {
	f, err := os.Open("/proc/diskstats")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	const sectorBytes = 512.0
	var samples []diskIOSample
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 14 {
			continue
		}
		device := fields[2]
		if isVirtualDisk(device) {
			continue
		}

		readsCompleted, _ := strconv.ParseFloat(fields[3], 64)
		sectorsRead, _ := strconv.ParseFloat(fields[5], 64)
		timeReadingMs, _ := strconv.ParseFloat(fields[6], 64)
		writesCompleted, _ := strconv.ParseFloat(fields[7], 64)
		sectorsWritten, _ := strconv.ParseFloat(fields[9], 64)
		timeWritingMs, _ := strconv.ParseFloat(fields[10], 64)
		iosInProgress, _ := strconv.ParseFloat(fields[11], 64)

		samples = append(samples, diskIOSample{
			device:       device,
			readBytes:    sectorsRead * sectorBytes,
			writeBytes:   sectorsWritten * sectorBytes,
			readOps:      readsCompleted,
			writeOps:     writesCompleted,
			readTimeSec:  timeReadingMs / 1000.0,
			writeTimeSec: timeWritingMs / 1000.0,
			pendingOps:   iosInProgress,
		})
	}
	return samples, nil
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
