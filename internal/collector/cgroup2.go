package collector

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
)

// This file reads container resource usage out of the cgroup v2 unified
// hierarchy. It is the source of every container.* metric except the network
// ones, which cgroups cannot answer — see containerNetworkIO.
//
// Reading cgroups rather than asking the Docker daemon for stats is the choice
// Datadog makes, and the reason is worth stating: /containers/<id>/stats is a
// streaming endpoint that costs the daemon a goroutine and a 1-second sample
// window per container, while the cgroup files are a handful of reads with no
// daemon involvement at all. It also decouples the numbers from the runtime —
// the same code answers for containerd or Podman, whose sockets speak a
// different protocol but whose containers live in the same hierarchy.
//
// Only v2 is implemented. cgroup v1 splits every controller into its own mount
// with a layout that differs between the systemd and cgroupfs drivers, and
// getting that wrong yields plausible numbers for the wrong container. Rather
// than guess, a v1 host is detected and reported — see cgroupMode.

// cgroupModeKind distinguishes the hierarchy this host is running.
type cgroupModeKind int

const (
	cgroupUnavailable cgroupModeKind = iota // no /sys/fs/cgroup at all
	cgroupV1                                // legacy split hierarchy — not supported
	cgroupV2                                // unified hierarchy
)

func (k cgroupModeKind) String() string {
	switch k {
	case cgroupV1:
		return "v1 (legacy)"
	case cgroupV2:
		return "v2 (unified)"
	default:
		return "unavailable"
	}
}

// cgroupRoot is where the host's cgroup filesystem is readable from here.
func cgroupRoot() string { return hostPath("/sys/fs/cgroup") }

// cgroupMode reports which hierarchy is mounted.
//
// The marker is cgroup.controllers, which exists only at the root of a v2
// mount. Testing for it is more reliable than checking the mount type, because
// a container sees /sys/fs/cgroup as a bind mount of whatever the host had.
func cgroupMode() cgroupModeKind {
	root := cgroupRoot()
	if _, err := os.Stat(path.Join(root, "cgroup.controllers")); err == nil {
		return cgroupV2
	}
	// A v1 host has per-controller directories under the same root.
	if _, err := os.Stat(path.Join(root, "memory")); err == nil {
		return cgroupV1
	}
	return cgroupUnavailable
}

// cgroupStats is one container's resource usage as the kernel reports it.
//
// Counters are cumulative since the container started, which is what the OTLP
// exporter wants: a Sum it can hand to the backend with a start time, rather
// than a rate this agent computed and can no longer re-aggregate. The one
// exception is CPU utilisation, derived in containers.go from two samples,
// because there is no counter form of "percent busy".
//
// Every field is paired with a have* flag rather than relying on zero, because
// zero is a legitimate reading. A container that has done no block I/O reports
// rbytes=0, and that is a fact worth emitting; a container whose io.stat is
// unreadable is a different situation and must not be reported as zero traffic.
type cgroupStats struct {
	CPUUsageNanos       uint64
	CPUUserNanos        uint64
	CPUSystemNanos      uint64
	CPUThrottledNanos   uint64
	CPUThrottledPeriods uint64
	HaveCPU             bool

	MemoryCurrent      uint64
	MemoryFile         uint64
	MemoryInactiveFile uint64
	// MemoryLimit is zero when the container is unconstrained ("max"). Callers
	// must treat zero as "no limit", not as a limit of zero bytes — emitting a
	// limit of 0 would make every unconstrained container look 100% full.
	MemoryLimit uint64
	HaveMemory  bool

	IOReadBytes  uint64
	IOWriteBytes uint64
	HaveIO       bool

	PIDsCurrent uint64
	HavePIDs    bool
}

// MemoryWorkingSet is the number `docker stats` shows, and it is not
// memory.current.
//
// memory.current includes the page cache, so a container that has merely read
// a large file appears to be using all of it — and it will be reclaimed under
// pressure without the container noticing. Subtracting inactive_file is the
// same correction the kubelet and cAdvisor apply, and it is what makes the
// percentage comparable to the limit that would actually trigger an OOM kill.
func (s cgroupStats) MemoryWorkingSet() uint64 {
	if s.MemoryInactiveFile >= s.MemoryCurrent {
		return 0
	}
	return s.MemoryCurrent - s.MemoryInactiveFile
}

// readCgroupStats collects everything readable under one container's cgroup
// directory.
//
// Individual controllers are allowed to be missing. A kernel built without the
// io controller, or a delegated cgroup where pids is not enabled, should cost
// you those metrics and nothing else — so each section sets its own have flag
// and errors are not propagated. The one hard failure is a directory that does
// not exist, which means the caller resolved the wrong path and should hear
// about it.
func readCgroupStats(dir string) (cgroupStats, error) {
	var s cgroupStats

	if fi, err := os.Stat(dir); err != nil {
		return s, err
	} else if !fi.IsDir() {
		return s, fmt.Errorf("cgroup path %s is not a directory", dir)
	}

	if kv, err := readCgroupKeyValues(path.Join(dir, "cpu.stat")); err == nil {
		// The kernel reports microseconds; OTel's container.cpu.usage.* are
		// nanoseconds. Converting here keeps the unit conversion next to the
		// only place that knows what unit the file is in.
		s.CPUUsageNanos = kv["usage_usec"] * 1000
		s.CPUUserNanos = kv["user_usec"] * 1000
		s.CPUSystemNanos = kv["system_usec"] * 1000
		s.CPUThrottledNanos = kv["throttled_usec"] * 1000
		s.CPUThrottledPeriods = kv["nr_throttled"]
		s.HaveCPU = true
	}

	if v, err := readCgroupUint(path.Join(dir, "memory.current")); err == nil {
		s.MemoryCurrent = v
		s.HaveMemory = true
		// memory.max holds the literal string "max" when unconstrained, which
		// is why this cannot use readCgroupUint directly.
		if lim, ok := readCgroupLimit(path.Join(dir, "memory.max")); ok {
			s.MemoryLimit = lim
		}
		if kv, err := readCgroupKeyValues(path.Join(dir, "memory.stat")); err == nil {
			s.MemoryFile = kv["file"]
			s.MemoryInactiveFile = kv["inactive_file"]
		}
	}

	if r, w, err := readCgroupIOStat(path.Join(dir, "io.stat")); err == nil {
		s.IOReadBytes, s.IOWriteBytes = r, w
		s.HaveIO = true
	}

	if v, err := readCgroupUint(path.Join(dir, "pids.current")); err == nil {
		s.PIDsCurrent = v
		s.HavePIDs = true
	}

	return s, nil
}

// readCgroupKeyValues parses the "key value" line format shared by cpu.stat and
// memory.stat. Unparseable lines are skipped: memory.stat gained new keys over
// several kernel releases and will gain more, and one unrecognised line must
// not cost the caller the whole file.
func readCgroupKeyValues(p string) (map[string]uint64, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := make(map[string]uint64, 16)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		out[fields[0]] = v
	}
	return out, sc.Err()
}

// readCgroupUint reads a file holding a single integer.
func readCgroupUint(p string) (uint64, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

// readCgroupLimit reads a limit file, which holds either an integer or the
// word "max". It reports ok=false for "max" and for anything unreadable, so an
// absent limit and an unlimited one are handled identically — which is correct,
// since neither gives a denominator to compute a percentage against.
func readCgroupLimit(p string) (uint64, bool) {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(b))
	if s == "max" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// readCgroupIOStat sums per-device byte counters out of io.stat.
//
//	8:0 rbytes=1024 wbytes=2048 rios=10 wios=20 dbytes=0 dios=0
//
// Summing across devices matches Datadog's container.io.read/write and OTel's
// io_service_bytes_recursive, both of which are whole-container figures. A
// per-device breakdown would need the major:minor numbers resolved to names to
// be readable, and those names live on the host — not something a containerised
// agent can rely on seeing.
//
// An empty io.stat is normal and not an error: a container that has issued no
// I/O has no device lines at all, and reporting zero bytes for it is the
// truthful answer.
func readCgroupIOStat(p string) (read, write uint64, err error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		for _, field := range strings.Fields(sc.Text()) {
			k, v, ok := strings.Cut(field, "=")
			if !ok {
				continue // the leading "8:0" device field
			}
			n, convErr := strconv.ParseUint(v, 10, 64)
			if convErr != nil {
				continue
			}
			switch k {
			case "rbytes":
				read += n
			case "wbytes":
				write += n
			}
		}
	}
	return read, write, sc.Err()
}

// readCgroupFirstPID returns any process id inside this cgroup.
//
// It exists to reach the container's network namespace: cgroup v2 has no
// network controller, so the only way to a container's byte counters is
// /proc/<pid>/net/dev for a process that lives in it. Any pid in the cgroup
// will do, since they share the namespace.
//
// Reading cgroup.procs is deliberately preferred over asking the Docker API
// for State.Pid. The API answer costs one HTTP round trip per container per
// interval — an N+1 against the daemon on a busy host — where this is a single
// file read, and it keeps the metrics path working when the socket is not
// mounted at all.
func readCgroupFirstPID(dir string) (int, error) {
	f, err := os.Open(path.Join(dir, "cgroup.procs"))
	if err != nil {
		return 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		if err != nil {
			continue
		}
		return pid, nil
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	// A container that is stopping can empty its cgroup between the listing
	// and this read. Not an error worth logging every interval.
	return 0, errors.New("cgroup has no processes")
}

// cgroupDirCandidates lists where a container's cgroup is likely to be, in the
// order they should be tried.
//
// The layout depends on the runtime's cgroup driver, which is not something the
// agent can be told and should not have to be. The systemd driver — the default
// on every distribution shipping systemd, which is to say almost all of them —
// produces the .scope form; the cgroupfs driver produces the plain nested form.
// The kubepods entries cover a Docker runtime under Kubernetes, where the
// container sits inside a QoS-class slice.
//
// Trying candidates before walking matters because the walk is the expensive
// path and this list resolves the overwhelming majority of hosts with a single
// stat.
func cgroupDirCandidates(root, id string) []string {
	// Docker first and in full, because it is by far the most common and every
	// stat that misses costs a syscall before the walk begins. The other
	// runtimes contribute their systemd-driver form only; their cgroupfs and
	// Kubernetes layouts nest too variably to enumerate, and the depth-limited
	// walk in resolveCgroupDir is what covers those.
	out := []string{
		path.Join(root, "system.slice", "docker-"+id+".scope"),
		path.Join(root, "docker", id),
		path.Join(root, "system.slice", "docker.service", "docker-"+id+".scope"),
		path.Join(root, "kubepods.slice", "docker-"+id+".scope"),
	}
	for _, p := range containerCgroupPrefixes {
		if p.prefix == "docker-" {
			continue
		}
		out = append(out, path.Join(root, "system.slice", p.prefix+id+".scope"))
	}
	return out
}

// resolveCgroupDir finds the cgroup directory for a container id.
//
// It tries the known layouts first and falls back to a depth-limited search.
// The search exists because cgroup paths are ultimately arbitrary — a container
// started inside a systemd user slice, or by a runtime configured with a
// cgroup-parent, lands somewhere none of the candidates predict — and returning
// no metrics for those would be a silent hole rather than a visible one.
//
// Callers are expected to cache the result for the container's lifetime: a
// cgroup directory does not move, so the walk is paid once per container, not
// once per interval.
func resolveCgroupDir(root, id string) (string, error) {
	for _, c := range cgroupDirCandidates(root, id) {
		if fi, err := os.Stat(c); err == nil && fi.IsDir() {
			return c, nil
		}
	}
	if dir := searchCgroupDir(root, id, 0); dir != "" {
		return dir, nil
	}
	return "", fmt.Errorf("no cgroup directory found for container %s under %s", shortID(id), root)
}

// cgroupSearchMaxDepth bounds the fallback walk. Four levels reaches
// <root>/<slice>/<sub-slice>/<scope>, which covers every nesting the runtimes
// actually produce; deeper would mostly be walking individual container
// subtrees looking for something that is not there.
const cgroupSearchMaxDepth = 4

// searchCgroupDir looks for a directory whose name contains the container id.
//
// Matching on "contains" rather than equality is what makes one function handle
// every naming convention at once: docker-<id>.scope, cri-containerd-<id>.scope
// and a bare <id> all contain it, and a 64-hex-character id is long enough that
// an accidental match is not a real concern.
func searchCgroupDir(dir, id string, depth int) string {
	if depth > cgroupSearchMaxDepth {
		return ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if strings.Contains(e.Name(), id) {
			return path.Join(dir, e.Name())
		}
	}
	// Breadth first: the match is far more likely to be a sibling than a
	// descendant, and descending early would walk a whole container subtree
	// before checking the slice next to it.
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if found := searchCgroupDir(path.Join(dir, e.Name()), id, depth+1); found != "" {
			return found
		}
	}
	return ""
}

// shortID trims a container id to the 12 characters Docker itself displays.
// Full ids are 64 hex characters, which is unreadable in a label and adds
// nothing — the prefix is already unique on any real host.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
