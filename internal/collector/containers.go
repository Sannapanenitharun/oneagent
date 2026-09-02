package collector

import (
	"bufio"
	"context"
	"io"
	"log"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// ContainerCollector emits per-container resource metrics on a Docker host.
//
// The division of labour follows the one Datadog settled on, and it is the
// reason this works at all when parts of the environment are missing: the
// cgroup filesystem supplies every number, and the Docker socket supplies only
// names. A host that mounts the socket but not the cgroups produces nothing; a
// host that mounts the cgroups but not the socket produces complete metrics
// labelled with short container ids. The second is a usable degradation, which
// is why the socket is optional.
//
// Metric names follow the OpenTelemetry container semantic conventions — the
// same set the OTel Collector's dockerstats receiver emits — rather than
// Datadog's container.* names. The two overlap textually and disagree on units
// (Datadog's container.cpu.usage is nanoseconds of CPU; OTel's is too, but its
// memory figures differ in what they include), and since everything downstream
// of this agent speaks OTLP, matching the OTel contract is what makes the data
// mean the same thing to a backend as data from any other collector.
type ContainerCollector struct {
	agentID string
	opts    ContainerOptions
	docker  *dockerClient
	gate    *sendGate
	stop    chan struct{}

	// Everything below is touched only by the goroutine Start launches, in
	// sample(). No mutex, matching how the rest of this package works: state
	// belongs to one goroutine rather than being shared under a lock.
	cgroupDirs map[string]string
	prevCPU    map[string]cpuTick
	mode       cgroupModeKind
	selfID     string
	// warned remembers one-shot diagnostics so a permanent condition — no
	// socket mounted, cgroup v1 host — is stated once rather than every
	// interval for the life of the process.
	warned map[string]bool
}

// ContainerOptions configures container collection.
type ContainerOptions struct {
	Interval       time.Duration
	DockerEndpoint string
	// ExcludeImages and ExcludeNames drop containers whose image or name
	// contains any of these substrings. Substring rather than exact match
	// because image references carry registries and tags that an operator
	// should not have to spell out to exclude a component.
	ExcludeImages []string
	ExcludeNames  []string
	// AppLabel names the Docker label whose value identifies the application
	// running in a container. Empty disables the mapping entirely, which is
	// the default and the previous behaviour.
	AppLabel string
}

// cpuTick is the previous CPU reading for one container, kept so utilisation
// can be derived. Two samples are required because the kernel exposes CPU as a
// cumulative counter and there is no counter form of "percent busy".
type cpuTick struct {
	usageNanos uint64
	at         time.Time
}

// Container metric names, OTel container semantic conventions.
const (
	metricContainerCPUTotal    = "container.cpu.usage.total"
	metricContainerCPUUser     = "container.cpu.usage.usermode"
	metricContainerCPUKernel   = "container.cpu.usage.kernelmode"
	metricContainerCPUThrottle = "container.cpu.throttling_data.throttled_time"
	metricContainerCPUUtil     = "container.cpu.utilization"
	metricContainerMemUsage    = "container.memory.usage.total"
	metricContainerMemLimit    = "container.memory.usage.limit"
	metricContainerMemPercent  = "container.memory.percent"
	metricContainerMemFile     = "container.memory.file"
	metricContainerBlockIO     = "container.blockio.io_service_bytes_recursive"
	metricContainerNetRx       = "container.network.io.usage.rx_bytes"
	metricContainerNetTx       = "container.network.io.usage.tx_bytes"
	metricContainerPIDs        = "container.pids.count"
)

func NewContainerCollector(agentID string, opts ContainerOptions) *ContainerCollector {
	if opts.Interval <= 0 {
		opts.Interval = 15 * time.Second
	}
	return &ContainerCollector{
		agentID:    agentID,
		opts:       opts,
		docker:     newDockerClient(opts.DockerEndpoint),
		gate:       newSendGate("containers"),
		stop:       make(chan struct{}),
		cgroupDirs: map[string]string{},
		prevCPU:    map[string]cpuTick{},
		warned:     map[string]bool{},
	}
}

func (c *ContainerCollector) Name() string { return "containers" }

func (c *ContainerCollector) Start(ctx context.Context, out chan<- Envelope) error {
	c.mode = cgroupMode()
	c.selfID = readSelfContainerID()

	// Said at startup rather than discovered from an empty dashboard. A
	// containerised agent that was not given the cgroup mount looks exactly
	// like a host with no containers, and those need different fixes.
	switch c.mode {
	case cgroupV2:
		log.Printf("containers: reading cgroup v2 under %s", cgroupRoot())
	case cgroupV1:
		log.Printf("containers: %s is cgroup v1 — container metrics are not collected. "+
			"Only the unified (v2) hierarchy is supported; v1 splits each controller into a "+
			"separate mount whose layout differs by cgroup driver, and reading the wrong one "+
			"reports another container's usage. Boot with systemd.unified_cgroup_hierarchy=1 "+
			"to enable collection.", cgroupRoot())
	default:
		log.Printf("containers: no cgroup filesystem at %s — container metrics are not collected. "+
			"If the agent is running in a container, mount the host's cgroups and set %s "+
			"(-v /sys/fs/cgroup:/host/sys/fs/cgroup:ro -e %s=/host).",
			cgroupRoot(), HostRootEnv, HostRootEnv)
	}

	if err := c.docker.Available(ctx); err != nil {
		log.Printf("containers: %v — metrics will still be collected from cgroups, "+
			"but containers will be labelled by id rather than name. Mount the socket "+
			"(-v /var/run/docker.sock:/var/run/docker.sock:ro) to get names and images.", err)
	}

	ticker := time.NewTicker(c.opts.Interval)
	go func() {
		defer ticker.Stop()
		defer c.docker.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case <-c.stop:
				return
			case <-ticker.C:
				c.sample(ctx, out)
			}
		}
	}()
	return nil
}

func (c *ContainerCollector) Stop() error {
	close(c.stop)
	return nil
}

// warnOnce logs a message the first time a given key is seen.
func (c *ContainerCollector) warnOnce(key, format string, args ...any) {
	if c.warned[key] {
		return
	}
	c.warned[key] = true
	log.Printf(format, args...)
}

func (c *ContainerCollector) sample(ctx context.Context, out chan<- Envelope) {
	if c.mode != cgroupV2 {
		return
	}
	now := time.Now().UTC()
	root := cgroupRoot()

	containers := c.discover(ctx, root)
	// See resolveSelfContainerID. For metrics this only costs a spurious series
	// rather than starting a feedback loop, but the agent's own container is
	// excluded for a reason — its usage correlates with how much telemetry is
	// being collected — and that reason does not depend on the namespace it was
	// started in.
	c.selfID = resolveSelfContainerID(c.selfID, containers)
	c.reportAppMapping(containers)
	seen := make(map[string]bool, len(containers))

	for _, ct := range containers {
		if c.skip(ct) {
			continue
		}
		seen[ct.ID] = true

		dir, ok := c.cgroupDirs[ct.ID]
		if !ok {
			resolved, err := resolveCgroupDir(root, ct.ID)
			if err != nil {
				// Expected while a container is starting or stopping, so it is
				// not worth a line per interval per container.
				c.warnOnce(cgroupWarnPrefix+ct.ID, "containers: %v", err)
				continue
			}
			dir = resolved
			c.cgroupDirs[ct.ID] = dir
		}

		stats, err := readCgroupStats(dir)
		if err != nil {
			// The directory went away — the container exited between the
			// listing and this read. Drop the cached path so a container that
			// reuses the id resolves afresh.
			delete(c.cgroupDirs, ct.ID)
			continue
		}
		c.emit(ctx, out, now, ct, dir, stats)
	}

	// Forget containers that are gone. Without this these maps grow for the
	// life of the process on any host that starts short-lived containers,
	// which is most CI runners and every cron-in-a-container setup.
	for id := range c.cgroupDirs {
		if !seen[id] {
			delete(c.cgroupDirs, id)
			delete(c.prevCPU, id)
		}
	}
	for id := range c.prevCPU {
		if !seen[id] {
			delete(c.prevCPU, id)
		}
	}
	// The warn set too. Its per-container entries are keyed by id, so a host
	// that churns containers whose cgroups never resolve — the exact case that
	// key exists for — would otherwise accumulate one entry per container
	// forever. The process-wide keys have no prefix and are left alone.
	for key := range c.warned {
		if id, ok := strings.CutPrefix(key, cgroupWarnPrefix); ok && !seen[id] {
			delete(c.warned, key)
		}
	}
}

// cgroupWarnPrefix namespaces the per-container entries in the warn set so they
// can be pruned without touching the process-wide ones.
const cgroupWarnPrefix = "cgroup:"

// discover lists the containers to report on, preferring the Docker daemon and
// falling back to the cgroup tree.
//
// The fallback is what makes the socket optional. It costs the names, the image
// and the container's start time, so the counters it produces are emitted
// without a start timestamp — an honest "I do not know when this began" rather
// than a guess that would make the first delta after an agent restart look like
// a spike.
func (c *ContainerCollector) discover(ctx context.Context, root string) []dockerContainer {
	listed, err := c.docker.Containers(ctx)
	if err == nil {
		return listed
	}
	c.warnOnce("docker-list", "containers: %v — falling back to cgroup discovery, "+
		"so containers are identified by id only", err)
	return discoverFromCgroups(root)
}

// skip applies the configured exclusions and always drops the agent's own
// container.
//
// Excluding self is not merely tidiness. The agent's container is the one
// container whose resource usage is guaranteed to correlate with how much
// telemetry is being collected, so including it puts a feedback signal into
// the data — and on a host with nothing else running, it makes the dashboard
// look busy when it is idle.
func (c *ContainerCollector) skip(ct dockerContainer) bool {
	if c.selfID != "" && ct.ID == c.selfID {
		return true
	}
	name := ct.Name()
	for _, pat := range c.opts.ExcludeNames {
		if pat != "" && strings.Contains(name, pat) {
			return true
		}
	}
	for _, pat := range c.opts.ExcludeImages {
		if pat != "" && strings.Contains(ct.Image, pat) {
			return true
		}
	}
	return false
}

func (c *ContainerCollector) emit(ctx context.Context, out chan<- Envelope, now time.Time, ct dockerContainer, dir string, s cgroupStats) {
	base := containerLabels(ct, c.opts.AppLabel)

	// Container counters start when the container starts, not when the host
	// boots. The exporter reads this internal label to fill OTLP's required
	// start_time_unix_nano on a Sum; without it the field is omitted, which is
	// correct-but-lossy rather than wrong. Created is zero under cgroup-only
	// discovery, and then the label is simply absent.
	startUnix := ""
	if ct.Created > 0 {
		startUnix = strconv.FormatInt(ct.Created, 10)
	}

	send := func(name string, value float64, extra map[string]string, cumulative bool) {
		labels := make(map[string]string, len(base)+len(extra)+1)
		for k, v := range base {
			labels[k] = v
		}
		for k, v := range extra {
			labels[k] = v
		}
		if cumulative && startUnix != "" {
			labels["_boot_time_unix"] = startUnix
		}
		c.gate.send(ctx, out, Envelope{
			Kind:      KindMetric,
			AgentID:   c.agentID,
			Source:    name,
			Timestamp: now,
			Value:     value,
			Labels:    labels,
		})
	}

	if s.HaveCPU {
		send(metricContainerCPUTotal, float64(s.CPUUsageNanos), nil, true)
		send(metricContainerCPUUser, float64(s.CPUUserNanos), nil, true)
		send(metricContainerCPUKernel, float64(s.CPUSystemNanos), nil, true)
		send(metricContainerCPUThrottle, float64(s.CPUThrottledNanos), nil, true)

		// Utilisation is expressed the way `docker stats` expresses it: percent
		// of one CPU, so a container using two cores fully reads 200%. That is
		// deliberately not normalised by core count, because the number an
		// operator compares this against is the one in docker stats, and
		// silently disagreeing with it by a factor of nCPU is worse than
		// needing a sentence of explanation.
		if prev, ok := c.prevCPU[ct.ID]; ok {
			elapsed := now.Sub(prev.at).Seconds()
			// A counter that went backwards means the container restarted
			// under the same id, which docker does not do — but a cgroup path
			// reused after an id collision would. Skip rather than emit a
			// negative or absurd rate.
			if elapsed > 0 && s.CPUUsageNanos >= prev.usageNanos {
				delta := float64(s.CPUUsageNanos - prev.usageNanos)
				send(metricContainerCPUUtil, (delta/1e9)/elapsed*100, nil, false)
			}
		}
		c.prevCPU[ct.ID] = cpuTick{usageNanos: s.CPUUsageNanos, at: now}
	}

	if s.HaveMemory {
		working := s.MemoryWorkingSet()
		send(metricContainerMemUsage, float64(working), nil, false)
		send(metricContainerMemFile, float64(s.MemoryFile), nil, false)
		// Only emitted when a limit is actually set. An unconstrained container
		// has no denominator, and reporting its limit as the host's total would
		// invent a constraint that does not exist.
		if s.MemoryLimit > 0 {
			send(metricContainerMemLimit, float64(s.MemoryLimit), nil, false)
			send(metricContainerMemPercent, float64(working)/float64(s.MemoryLimit)*100, nil, false)
		}
	}

	if s.HaveIO {
		send(metricContainerBlockIO, float64(s.IOReadBytes), map[string]string{"operation": "read"}, true)
		send(metricContainerBlockIO, float64(s.IOWriteBytes), map[string]string{"operation": "write"}, true)
	}

	if s.HavePIDs {
		send(metricContainerPIDs, float64(s.PIDsCurrent), nil, false)
	}

	// Network is not a cgroup v2 controller, so it comes from the container's
	// network namespace instead — see containerNetworkIO.
	if rx, tx, ok := containerNetworkIO(dir); ok {
		send(metricContainerNetRx, float64(rx), nil, true)
		send(metricContainerNetTx, float64(tx), nil, true)
	}
}

// maxAppNameBytes bounds a value read from a container label before it becomes
// service.name.
//
// service is a LowCardinality column in the logs table and a series label on
// every container metric, so a label templated with something per-request would
// be expensive in one place and permanent in the other. Sixty-four bytes is
// longer than any application name and shorter than the identifiers that get
// pasted into labels by accident.
const maxAppNameBytes = 64

// appNameFrom reads the application identity out of a container's own labels.
//
// Deliberately a pure function with no memory of what it has seen. The obvious
// alternative — count distinct values and refuse past a cap — needs state, and
// this is called from three collectors, one of which runs a goroutine per
// container. Shared mutable state there would need a lock, which this package
// does not have and should not gain for a validation. A per-collector cap would
// not be a cap on anything meaningful, since the three would drift.
//
// The bound that actually holds is structural rather than counted: every
// container metric already carries container.id, so grouping those same series
// by application cannot create a series that did not already exist. What is
// left to guard against is one pathological value, and length plus a character
// check does that without remembering anything.
//
// A rejected value yields "", and the container falls back to the host's
// identity exactly as an unlabelled one does. The collector says how many
// containers resolved at startup, which is where a mapping that matches nothing
// becomes visible.
func appNameFrom(ct dockerContainer, key string) string {
	if key == "" || ct.Labels == nil {
		return ""
	}
	v := strings.TrimSpace(ct.Labels[key])
	if v == "" || len(v) > maxAppNameBytes {
		return ""
	}
	for _, r := range v {
		// Control characters and internal whitespace: neither belongs in a
		// service name, and both survive into a database column and a
		// dashboard filter if they are let through.
		if r < 0x20 || r == 0x7f || unicode.IsSpace(r) {
			return ""
		}
	}
	return v
}

// containerLabels builds the identifying attributes carried by every metric and
// every log line from one container.
//
// container.id is the short form. The full 64-character id is unique but
// unreadable, and it would be the highest-cardinality label in the whole
// dataset — every restart of every container creating a permanent new series.
// Twelve characters is what docker itself displays and is unique on any real
// host.
//
// appLabel is the Docker label key that names the application; when a container
// carries it, the value becomes service.name. That single assignment is what
// makes container telemetry attributable: the exporter already preserves a
// per-envelope service.name and groups resources by it — it was written so that
// spans from an instrumented app would not be re-labelled with the agent's own
// identity — but nothing the agent generates itself has ever set the label, so
// every container on a host has reported under one service until now.
func containerLabels(ct dockerContainer, appLabel string) map[string]string {
	runtime := ct.Runtime
	if runtime == "" {
		// Discovered from a cgroup name that named no runtime. Reported as
		// unknown rather than assumed to be docker: the label is there so an
		// operator can tell a containerd container from a Podman one, and a
		// value that is sometimes a guess cannot do that job.
		runtime = "unknown"
	}
	labels := map[string]string{
		"container.id":      shortID(ct.ID),
		"container.runtime": runtime,
	}
	if name := ct.Name(); name != "" {
		labels["container.name"] = name
	}
	if ct.Image != "" {
		labels["container.image.name"] = ct.Image
	}
	if app := appNameFrom(ct, appLabel); app != "" {
		labels["service.name"] = app
	}
	return labels
}

// containerNetworkIO reads a container's byte counters from its network
// namespace.
//
// cgroup v2 has no network controller, which is why this is the one metric
// family that cannot come from the cgroup files. The route in is a process
// inside the container: /proc/<pid>/net/dev is rendered from the reader's
// network namespace, so reading it for a pid in the container yields that
// container's interfaces.
//
// This requires the host's process namespace — `--pid host` on the agent
// container — because otherwise the pids listed in cgroup.procs are not
// resolvable under /proc. When they are not, this reports ok=false and the
// network metrics are simply absent, which is the intended degradation.
func containerNetworkIO(cgroupDir string) (rx, tx uint64, ok bool) {
	pid, err := readCgroupFirstPID(cgroupDir)
	if err != nil {
		return 0, 0, false
	}
	f, err := os.Open(hostPath(path.Join("/proc", strconv.Itoa(pid), "net", "dev")))
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		if line <= 2 {
			continue // two header rows
		}
		text := sc.Text()
		colon := strings.Index(text, ":")
		if colon < 0 {
			continue
		}
		device := strings.TrimSpace(text[:colon])
		// Loopback is excluded for the same reason the host metrics exclude it:
		// a container talking to itself is not network traffic, and including
		// it makes an idle container with a health check look busy.
		if device == "lo" {
			continue
		}
		fields := strings.Fields(text[colon+1:])
		if len(fields) < 16 {
			continue
		}
		if v, err := strconv.ParseUint(fields[0], 10, 64); err == nil {
			rx += v
		}
		if v, err := strconv.ParseUint(fields[8], 10, 64); err == nil {
			tx += v
		}
		ok = true
	}
	return rx, tx, ok
}

// discoverFromCgroups enumerates containers by looking at the cgroup tree,
// for when the Docker socket is unavailable.
//
// It searches the same layouts resolveCgroupDir knows about, in reverse: rather
// than asking "where is this id", it asks "what ids are here". Only the id is
// recoverable this way, so the returned records carry no name, image or
// creation time — dockerContainer.Name falls back to the short id.
func discoverFromCgroups(root string) []dockerContainer {
	var out []dockerContainer
	seen := map[string]bool{}

	// Walked rather than read from a fixed list of parents, because the
	// runtimes that are not Docker nest far more deeply. A containerd container
	// under Kubernetes lives at
	//
	//	kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod<uid>.slice/cri-containerd-<id>.scope
	//
	// which is four levels down and cannot be enumerated by naming its parent.
	// The depth bound is the same one resolveCgroupDir uses, and for the same
	// reason: it covers every layout in practice while keeping a pathological
	// tree from being walked forever.
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if depth > cgroupSearchMaxDepth {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			id, runtime := containerIDFromCgroupName(e.Name())
			if id != "" {
				if seen[id] {
					continue
				}
				seen[id] = true
				if runtime == "" {
					// A bare id under .../docker/ is Docker's cgroupfs layout.
					// The parent is the only evidence available, and it is
					// good evidence.
					runtime = runtimeFromCgroupParent(dir)
				}
				out = append(out, dockerContainer{ID: id, State: "running", Runtime: runtime})
				// A container's own cgroup has no container children; not
				// descending saves walking every container's subtree.
				continue
			}
			walk(path.Join(dir, e.Name()), depth+1)
		}
	}
	walk(root, 0)
	return out
}

// runtimeFromCgroupParent infers a runtime from the directory a bare container
// id was found in, for the cgroupfs drivers that carry no prefix.
func runtimeFromCgroupParent(dir string) string {
	switch base := path.Base(dir); {
	case base == "docker" || strings.HasPrefix(base, "docker."):
		return "docker"
	case strings.HasPrefix(base, "containerd"):
		return "containerd"
	case strings.HasPrefix(base, "crio"):
		return "cri-o"
	case strings.Contains(base, "libpod"):
		return "podman"
	default:
		return ""
	}
}

// containerCgroupPrefixes maps a cgroup directory-name prefix to the runtime
// that created it, in the order they must be tried.
//
// Every OCI runtime names its cgroup after the container id and distinguishes
// itself with a prefix. That is the whole of the difference between them as far
// as this agent is concerned: the CPU, memory, I/O and PID numbers come from
// the same cgroup v2 files whatever created them, so supporting a new runtime
// is a naming question, not a collection one.
//
// Order matters. "cri-containerd-" must be tested before "containerd-", or the
// longer prefix never matches and every containerd container is reported with
// "cri-" left on the front of its id.
var containerCgroupPrefixes = []struct{ prefix, runtime string }{
	{"docker-", "docker"},
	{"cri-containerd-", "containerd"},
	{"containerd-", "containerd"},
	{"crio-", "cri-o"},
	{"libpod-", "podman"},
}

// containerIDFromCgroupName extracts a container id and its runtime from a
// cgroup directory name, returning "" when the name is not a container's.
//
// The accepted shapes are "<prefix><id>.scope" for the systemd driver and a
// bare "<id>" for cgroupfs. Requiring 64 hex characters is what keeps ordinary
// slices — user.slice, system.slice, init.scope — from being mistaken for
// containers.
//
// A bare id carries no evidence of which runtime made it, so the runtime is
// returned empty rather than assumed. Guessing "docker" there would have been
// right on most hosts and quietly wrong on the rest, and a mislabelled runtime
// is worse than an absent one: it is a fact the operator cannot tell is made
// up.
func containerIDFromCgroupName(name string) (id, runtime string) {
	name = strings.TrimSuffix(name, ".scope")
	for _, p := range containerCgroupPrefixes {
		if rest, ok := strings.CutPrefix(name, p.prefix); ok {
			if len(rest) == 64 && isHex(rest) {
				return rest, p.runtime
			}
		}
	}
	if len(name) == 64 && isHex(name) {
		return name, ""
	}
	return "", ""
}

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// readSelfContainerID returns the id of the container this agent is running in,
// or "" when it is running on the host.
//
// Under cgroup v2 /proc/self/cgroup has a single line whose third field is the
// path of this process's cgroup, and for a container that path ends in the
// container's own directory — the same names containerIDFromCgroupName parses.
// This is read once at startup: a process does not change containers.
func readSelfContainerID() string {
	// Deliberately NOT hostPath: this asks about the agent's own process, and
	// under a bind-mounted host /proc the answer would be about pid 1 on the
	// host instead.
	f, err := os.Open("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	defer f.Close()
	return parseSelfCgroup(f)
}

// resolveSelfContainerID identifies the container the agent is running in,
// using the daemon's own list to confirm a guess the cgroup path could not make.
//
// This exists because /proc/self/cgroup only names the container when the agent
// shares the host's cgroup namespace. Without --cgroupns host it reads "0::/",
// the agent cannot recognise itself, and it collects its own container's
// output — which for the log collectors is a feedback loop, not merely noise:
// every envelope the agent writes to stdout is read back, wrapped in a new
// envelope, and written again, with the escaping doubling on each pass. It
// saturates the pipeline within seconds and starves every other container.
//
// Docker names a container's UTS hostname after its own short id, so the
// hostname is the second signal. It is never trusted on its own: it is matched
// against the ids the daemon just reported, so the only way to get a false
// positive is for the hostname to be the short id of a container that is
// genuinely running — which is the thing being detected.
//
// The two signals cover different deployments and neither is redundant. With
// --cgroupns host the cgroup path answers and the hostname does not (--uts host
// makes it the host's name). Without it, the reverse.
func resolveSelfContainerID(current string, listed []dockerContainer) string {
	if current != "" {
		return current
	}
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return resolveSelfContainerIDWithHost(h, current, listed)
}

// resolveSelfContainerIDWithHost is the testable body of the above. The
// hostname is a parameter because the interesting behaviour is which hostnames
// may be matched against container ids and which must not, and the test
// process's own hostname is whatever the runner gave it.
func resolveSelfContainerIDWithHost(hostname, current string, listed []dockerContainer) string {
	if current != "" {
		return current
	}
	// A short id is exactly twelve hex characters. Anything else is an ordinary
	// hostname and must not be prefix-matched against container ids — a host
	// called "web01" would otherwise claim any container whose id starts with
	// those five characters.
	if len(hostname) != 12 || !isHex(hostname) {
		return ""
	}
	for _, ct := range listed {
		if strings.HasPrefix(ct.ID, hostname) {
			return ct.ID
		}
	}
	return ""
}

// parseSelfCgroup is the testable body of readSelfContainerID.
//
// Split out because the interesting behaviour is which cgroup paths count as a
// container and which do not, and that cannot be exercised through a hardcoded
// /proc/self/cgroup — the test process is whatever the test runner put it in.
func parseSelfCgroup(r io.Reader) string {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) < 3 {
			continue
		}
		for _, seg := range strings.Split(fields[2], "/") {
			// The runtime is irrelevant here — the question is only which
			// container this process is in, and the agent's own container is
			// excluded the same way whatever created it.
			if id, _ := containerIDFromCgroupName(seg); id != "" {
				return id
			}
		}
	}
	return ""
}

// reportAppMapping states once how many containers the app label resolved.
//
// Configuring a label key that no container carries is a silent failure of
// exactly the kind this codebase keeps running into: telemetry continues, every
// container keeps reporting under the host's identity, and the only symptom is
// a cost report where one application accounts for everything. A count is
// enough to catch it — "0 of 21" and "19 of 21" need different fixes, and both
// are obvious the moment the number is written down.
//
// warnOnce keys on the resolved count, so a genuine change — a deployment that
// labels the remaining containers — is reported again, while a steady state
// stays quiet.
func (c *ContainerCollector) reportAppMapping(containers []dockerContainer) {
	if c.opts.AppLabel == "" {
		return
	}
	resolved, total := 0, 0
	for _, ct := range containers {
		if c.skip(ct) {
			continue
		}
		total++
		if appNameFrom(ct, c.opts.AppLabel) != "" {
			resolved++
		}
	}
	if total == 0 {
		return
	}
	key := "applabel:" + strconv.Itoa(resolved) + "/" + strconv.Itoa(total)
	switch {
	case resolved == 0:
		c.warnOnce(key, "containers: label %q matched none of %d containers — their metrics and logs "+
			"will report under this host's identity, not per application. Set it with "+
			"`docker run --label %s=<app>` or a compose `labels:` block.",
			c.opts.AppLabel, total, c.opts.AppLabel)
	case resolved < total:
		c.warnOnce(key, "containers: label %q resolved %d of %d containers; the remaining %d report "+
			"under this host's identity", c.opts.AppLabel, resolved, total, total-resolved)
	default:
		c.warnOnce(key, "containers: label %q resolved all %d containers", c.opts.AppLabel, total)
	}
}
