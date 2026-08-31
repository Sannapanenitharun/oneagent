package collector

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withHostRoot points the package's host-path resolution at a fixture tree for
// the duration of one test. hostRoot is set once at process start and read-only
// thereafter, so this is the only place it is ever assigned.
func withHostRoot(t *testing.T, root string) {
	t.Helper()
	prev := hostRoot
	hostRoot = normalizeHostRoot(root)
	t.Cleanup(func() { hostRoot = prev })
}

func TestHostPath(t *testing.T) {
	tests := []struct {
		name, root, in, want string
	}{
		{"unset means read this machine directly", "", "/proc/stat", "/proc/stat"},
		{"a mounted host root is prefixed", "/host", "/proc/stat", "/host/proc/stat"},
		// A container that bind-mounts the host root at / has not moved
		// anything, and joining "/" onto every path would allocate for nothing.
		{"slash is not a prefix", "/", "/proc/stat", "/proc/stat"},
		{"trailing slashes are cleaned", "/host/", "/sys/fs/cgroup", "/host/sys/fs/cgroup"},
		{"whitespace from a shell export is trimmed", "  /host  ", "/proc/meminfo", "/host/proc/meminfo"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withHostRoot(t, tc.root)
			if got := hostPath(tc.in); got != tc.want {
				t.Errorf("hostPath(%q) with root %q = %q, want %q", tc.in, tc.root, got, tc.want)
			}
		})
	}
}

func TestContainerIDFromCgroupName(t *testing.T) {
	tests := []struct {
		in, wantID, wantRuntime string
	}{
		{"docker-" + testContainerID + ".scope", testContainerID, "docker"},
		// Every other OCI runtime names its cgroup the same way and differs
		// only in the prefix, which is why supporting them is a naming change
		// rather than a collection one: the numbers come from the same files.
		{"cri-containerd-" + testContainerID + ".scope", testContainerID, "containerd"},
		{"containerd-" + testContainerID + ".scope", testContainerID, "containerd"},
		{"crio-" + testContainerID + ".scope", testContainerID, "cri-o"},
		{"libpod-" + testContainerID + ".scope", testContainerID, "podman"},
		// cgroupfs driver: a bare id names no runtime, and one is not invented.
		{testContainerID, testContainerID, ""},
		// Ordinary slices share the directory with containers and must not be
		// mistaken for them — the length and hex check is what separates them.
		{"system.slice", "", ""},
		{"user-1000.slice", "", ""},
		{"init.scope", "", ""},
		{"docker-short.scope", "", ""},
		{"crio-short.scope", "", ""},
		{strings.Repeat("z", 64), "", ""},
	}
	for _, tc := range tests {
		id, runtime := containerIDFromCgroupName(tc.in)
		if id != tc.wantID || runtime != tc.wantRuntime {
			t.Errorf("containerIDFromCgroupName(%q) = (%q, %q), want (%q, %q)",
				tc.in, id, runtime, tc.wantID, tc.wantRuntime)
		}
	}
}

// "cri-containerd-" is a longer prefix that starts with a shorter one. Testing
// the short prefix first would leave "cri-" on the front of every containerd
// id, producing a 68-character string that then fails the hex check — so the
// container would vanish rather than be misnamed, which is the harder failure
// to notice.
func TestContainerIDFromCgroupName_LongestPrefixWins(t *testing.T) {
	id, runtime := containerIDFromCgroupName("cri-containerd-" + testContainerID + ".scope")
	if id != testContainerID {
		t.Errorf("id = %q, want the bare 64-hex id", id)
	}
	if runtime != "containerd" {
		t.Errorf("runtime = %q, want containerd", runtime)
	}
}

// A container discovered from a cgroup that names no runtime is reported as
// unknown. Labelling it "docker" would be right on most hosts and silently
// wrong on the rest, and the label exists precisely so an operator can tell the
// runtimes apart.
func TestContainerLabels_RuntimeIsNeverGuessed(t *testing.T) {
	if got := containerLabels(dockerContainer{ID: testContainerID})["container.runtime"]; got != "unknown" {
		t.Errorf("container.runtime = %q, want unknown for a container with no runtime evidence", got)
	}
	for _, rt := range []string{"docker", "containerd", "cri-o", "podman"} {
		got := containerLabels(dockerContainer{ID: testContainerID, Runtime: rt})["container.runtime"]
		if got != rt {
			t.Errorf("container.runtime = %q, want %q", got, rt)
		}
	}
}

// The cgroupfs drivers carry no prefix, so the directory a bare id sits in is
// the only evidence of what made it.
func TestRuntimeFromCgroupParent(t *testing.T) {
	tests := []struct{ dir, want string }{
		{"/sys/fs/cgroup/docker", "docker"},
		{"/sys/fs/cgroup/system.slice/docker.service", "docker"},
		{"/sys/fs/cgroup/system.slice/containerd.service", "containerd"},
		{"/sys/fs/cgroup/crio", "cri-o"},
		{"/sys/fs/cgroup/machine.slice/libpod_parent", "podman"},
		{"/sys/fs/cgroup/system.slice", ""},
		{"/sys/fs/cgroup", ""},
	}
	for _, tc := range tests {
		if got := runtimeFromCgroupParent(tc.dir); got != tc.want {
			t.Errorf("runtimeFromCgroupParent(%q) = %q, want %q", tc.dir, got, tc.want)
		}
	}
}

func TestContainerIDFromLogPath(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"/var/lib/docker/containers/" + testContainerID + "/" + testContainerID + "-json.log", testContainerID},
		// The directory is used rather than the filename precisely so a rotated
		// file keeps resolving to the same container.
		{"/var/lib/docker/containers/" + testContainerID + "/" + testContainerID + "-json.log.1", testContainerID},
		{"/host/var/lib/docker/containers/" + testContainerID + "/x-json.log", testContainerID},
		{"/var/log/syslog", ""},
		{"/var/lib/docker/containers/not-an-id/x-json.log", ""},
	}
	for _, tc := range tests {
		if got := containerIDFromLogPath(tc.in); got != tc.want {
			t.Errorf("containerIDFromLogPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDockerContainerName(t *testing.T) {
	// The Engine prefixes names with a slash, which is an artefact of its
	// naming scheme and not something anyone wants to see on a chart.
	if got := (dockerContainer{Names: []string{"/api-server"}}).Name(); got != "api-server" {
		t.Errorf("Name() = %q, want %q", got, "api-server")
	}
	// A container started without a name still has to carry something
	// identifying, or its metrics are unattributable.
	if got := (dockerContainer{ID: testContainerID}).Name(); got != testContainerID[:12] {
		t.Errorf("Name() = %q, want the short id %q", got, testContainerID[:12])
	}
}

// buildContainerFixture lays down a cgroup tree with one container in it and
// returns the fake host root.
func buildContainerFixture(t *testing.T, id string, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "sys", "fs", "cgroup", "system.slice", "docker-"+id+".scope")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The v2 marker, so cgroupMode reports the unified hierarchy.
	if err := os.WriteFile(filepath.Join(root, "sys", "fs", "cgroup", "cgroup.controllers"),
		[]byte("cpu io memory pids\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// collect drains everything the collector emits during one sample.
func collectOnce(t *testing.T, c *ContainerCollector) map[string][]Envelope {
	t.Helper()
	out := make(chan Envelope, 256)
	c.sample(context.Background(), out)
	close(out)

	got := map[string][]Envelope{}
	for env := range out {
		got[env.Source] = append(got[env.Source], env)
	}
	return got
}

func newTestCollector(t *testing.T, root string) *ContainerCollector {
	t.Helper()
	withHostRoot(t, root)
	c := NewContainerCollector("test-agent", ContainerOptions{Interval: time.Second})
	c.mode = cgroupMode()
	if c.mode != cgroupV2 {
		t.Fatalf("fixture did not present as cgroup v2, got %s", c.mode)
	}
	return c
}

func TestContainerCollector_EmitsCgroupMetricsWithoutDocker(t *testing.T) {
	// The whole point of reading cgroups for numbers: no daemon is running in
	// this test, and metrics still come out — labelled by id rather than name.
	root := buildContainerFixture(t, testContainerID, map[string]string{
		"cpu.stat":       "usage_usec 2000000\nuser_usec 1200000\nsystem_usec 800000\nnr_throttled 0\nthrottled_usec 0\n",
		"memory.current": "104857600\n",
		"memory.max":     "209715200\n",
		"memory.stat":    "file 20971520\ninactive_file 10485760\n",
		"io.stat":        "8:0 rbytes=4096 wbytes=8192 rios=1 wios=2\n",
		"pids.current":   "9\n",
	})
	c := newTestCollector(t, root)

	got := collectOnce(t, c)

	for _, name := range []string{
		metricContainerCPUTotal, metricContainerCPUUser, metricContainerCPUKernel,
		metricContainerMemUsage, metricContainerMemLimit, metricContainerMemPercent,
		metricContainerBlockIO, metricContainerPIDs,
	} {
		if len(got[name]) == 0 {
			t.Errorf("no %s emitted", name)
		}
	}

	if v := got[metricContainerCPUTotal][0].Value; v != 2_000_000_000 {
		t.Errorf("%s = %v, want 2e9", metricContainerCPUTotal, v)
	}
	// 100 MiB current less 10 MiB inactive file cache.
	if v := got[metricContainerMemUsage][0].Value; v != 94371840 {
		t.Errorf("%s = %v, want 94371840", metricContainerMemUsage, v)
	}
	// 90 MiB working set against a 200 MiB limit.
	if v := got[metricContainerMemPercent][0].Value; v < 44.9 || v > 45.1 {
		t.Errorf("%s = %v, want ~45", metricContainerMemPercent, v)
	}

	labels := got[metricContainerCPUTotal][0].Labels
	if labels["container.id"] != testContainerID[:12] {
		t.Errorf("container.id = %q, want the short id", labels["container.id"])
	}
	if labels["container.runtime"] != "docker" {
		t.Errorf("container.runtime = %q, want docker", labels["container.runtime"])
	}
	// No daemon means no creation time, so the counters must go out without a
	// start time rather than with a fabricated one.
	if _, ok := labels["_boot_time_unix"]; ok {
		t.Error("a start time was attached despite the container having no known creation time")
	}

	// Read and write are one metric distinguished by an attribute, matching
	// io_service_bytes_recursive.
	ops := map[string]float64{}
	for _, env := range got[metricContainerBlockIO] {
		ops[env.Labels["operation"]] = env.Value
	}
	if ops["read"] != 4096 || ops["write"] != 8192 {
		t.Errorf("blockio = %v, want read 4096 / write 8192", ops)
	}
}

// A limit of "max" has no denominator, so a percentage cannot be computed and
// must not be invented. Reporting the limit as zero would put every
// unconstrained container at 100%.
func TestContainerCollector_UnlimitedMemoryHasNoLimitOrPercent(t *testing.T) {
	root := buildContainerFixture(t, testContainerID, map[string]string{
		"memory.current": "104857600\n",
		"memory.max":     "max\n",
		"memory.stat":    "file 0\ninactive_file 0\n",
	})
	c := newTestCollector(t, root)

	got := collectOnce(t, c)

	if len(got[metricContainerMemUsage]) == 0 {
		t.Fatal("no memory usage emitted")
	}
	if len(got[metricContainerMemLimit]) != 0 {
		t.Errorf("%s emitted for an unconstrained container", metricContainerMemLimit)
	}
	if len(got[metricContainerMemPercent]) != 0 {
		t.Errorf("%s emitted with no limit to divide by", metricContainerMemPercent)
	}
}

// CPU utilisation needs two samples, and the first one must not guess. Emitting
// a value on the first sample would report the container's whole lifetime
// average as if it were the current rate.
func TestContainerCollector_CPUUtilizationNeedsTwoSamples(t *testing.T) {
	root := buildContainerFixture(t, testContainerID, map[string]string{
		"cpu.stat": "usage_usec 1000000\nuser_usec 1000000\nsystem_usec 0\n",
	})
	c := newTestCollector(t, root)

	if got := collectOnce(t, c); len(got[metricContainerCPUUtil]) != 0 {
		t.Fatalf("%s emitted on the first sample", metricContainerCPUUtil)
	}

	// Half a second of CPU consumed since the first sample. The exact
	// percentage depends on wall-clock elapsed, so this asserts only that a
	// value appears and is positive — the arithmetic itself is asserted below.
	dir := filepath.Join(root, "sys", "fs", "cgroup", "system.slice", "docker-"+testContainerID+".scope")
	if err := os.WriteFile(filepath.Join(dir, "cpu.stat"),
		[]byte("usage_usec 1500000\nuser_usec 1500000\nsystem_usec 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := collectOnce(t, c)
	if len(got[metricContainerCPUUtil]) == 0 {
		t.Fatal("no utilisation emitted on the second sample")
	}
	if v := got[metricContainerCPUUtil][0].Value; v <= 0 {
		t.Errorf("utilisation = %v, want a positive percentage", v)
	}
}

// Deliberately expressed as percent of ONE cpu, the way `docker stats` reports
// it, so a container using two cores reads 200%. Normalising by core count here
// would silently disagree with the number an operator checks it against.
func TestContainerCollector_CPUUtilizationIsPercentOfOneCore(t *testing.T) {
	root := buildContainerFixture(t, testContainerID, nil)
	c := newTestCollector(t, root)

	now := time.Now().UTC()
	// Two full seconds of CPU consumed over one second of wall clock: two cores
	// saturated.
	c.prevCPU[testContainerID] = cpuTick{usageNanos: 0, at: now.Add(-time.Second)}

	out := make(chan Envelope, 32)
	c.emit(context.Background(), out, now,
		dockerContainer{ID: testContainerID},
		filepath.Join(root, "sys", "fs", "cgroup", "system.slice", "docker-"+testContainerID+".scope"),
		cgroupStats{HaveCPU: true, CPUUsageNanos: 2_000_000_000})
	close(out)

	var util float64
	for env := range out {
		if env.Source == metricContainerCPUUtil {
			util = env.Value
		}
	}
	if util < 199 || util > 201 {
		t.Errorf("utilisation = %v, want ~200 (two saturated cores)", util)
	}
}

// Both caches are keyed by container id, and a host that runs short-lived
// containers — any CI runner, any cron-in-a-container — would otherwise grow
// one entry per container that has ever existed.
func TestContainerCollector_PrunesVanishedContainers(t *testing.T) {
	root := buildContainerFixture(t, testContainerID, map[string]string{
		"cpu.stat": "usage_usec 1000\nuser_usec 1000\nsystem_usec 0\n",
	})
	c := newTestCollector(t, root)

	collectOnce(t, c)
	if len(c.cgroupDirs) != 1 || len(c.prevCPU) != 1 {
		t.Fatalf("after one sample: %d cgroup dirs, %d cpu ticks; want 1 and 1",
			len(c.cgroupDirs), len(c.prevCPU))
	}

	// The container exits: its cgroup directory disappears.
	if err := os.RemoveAll(filepath.Join(root, "sys", "fs", "cgroup", "system.slice")); err != nil {
		t.Fatal(err)
	}
	collectOnce(t, c)

	if len(c.cgroupDirs) != 0 || len(c.prevCPU) != 0 {
		t.Errorf("state retained for a container that is gone: %d cgroup dirs, %d cpu ticks",
			len(c.cgroupDirs), len(c.prevCPU))
	}
}

func TestContainerCollector_Excludes(t *testing.T) {
	c := &ContainerCollector{
		opts:   ContainerOptions{ExcludeNames: []string{"buildkit"}, ExcludeImages: []string{"k8s.gcr.io/"}},
		selfID: testContainerID,
	}

	cases := []struct {
		name string
		ct   dockerContainer
		want bool
	}{
		{"own container", dockerContainer{ID: testContainerID}, true},
		{"name substring", dockerContainer{ID: "a", Names: []string{"/moby-buildkit-1"}}, true},
		{"image substring", dockerContainer{ID: "b", Image: "k8s.gcr.io/pause:3.9"}, true},
		{"unrelated", dockerContainer{ID: "c", Names: []string{"/api"}, Image: "nginx:1.25"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.skip(tc.ct); got != tc.want {
				t.Errorf("skip() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A cgroup v1 host must produce nothing rather than wrong numbers. v1 splits
// controllers across separate mounts whose layout depends on the cgroup driver,
// and reading the wrong one attributes another container's usage.
func TestContainerCollector_SkipsNonV2Hierarchy(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sys", "fs", "cgroup", "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	withHostRoot(t, root)

	if mode := cgroupMode(); mode != cgroupV1 {
		t.Fatalf("cgroupMode() = %s, want v1", mode)
	}

	c := NewContainerCollector("test-agent", ContainerOptions{Interval: time.Second})
	c.mode = cgroupV1
	if got := collectOnce(t, c); len(got) != 0 {
		t.Errorf("emitted %d metric families on a v1 host, want none", len(got))
	}
}

func TestDiscoverFromCgroups(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cgroup")
	other := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, dir := range []string{
		filepath.Join(root, "system.slice", "docker-"+testContainerID+".scope"),
		filepath.Join(root, "docker", other),
		// Neither of these is a container and both must be ignored.
		filepath.Join(root, "system.slice", "sshd.service"),
		filepath.Join(root, "system.slice", "init.scope"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	found := map[string]bool{}
	for _, ct := range discoverFromCgroups(root) {
		found[ct.ID] = true
	}
	if !found[testContainerID] || !found[other] {
		t.Errorf("discovered %v, want both container ids", found)
	}
	if len(found) != 2 {
		t.Errorf("discovered %d containers, want 2 — a systemd unit was mistaken for one", len(found))
	}
}

// Exercises the unix-socket dial path, which is the part of the hand-rolled
// client most likely to be wrong and the reason the docker SDK is not a
// dependency.
func TestDockerClient_ListsContainersOverUnixSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	defer ln.Close()

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode([]dockerContainer{{
			ID: testContainerID, Names: []string{"/api"}, Image: "nginx:1.25",
			State: "running", Created: 1700000000,
		}})
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	c := newDockerClient(sock)
	defer c.Close()

	if err := c.Available(context.Background()); err != nil {
		t.Fatalf("Available: %v", err)
	}
	got, err := c.Containers(context.Background())
	if err != nil {
		t.Fatalf("Containers: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d containers, want 1", len(got))
	}
	if got[0].Name() != "api" || got[0].Image != "nginx:1.25" || got[0].Created != 1700000000 {
		t.Errorf("decoded %+v, want name api / nginx:1.25 / created 1700000000", got[0])
	}
}

// An absent socket is the normal case for an agent on a host with no Docker, and
// for a containerised agent that was not given the mount. It must be reported
// as an ordinary error, not a panic or a hang.
func TestDockerClient_UnavailableSocket(t *testing.T) {
	c := newDockerClient(filepath.Join(t.TempDir(), "absent.sock"))
	defer c.Close()

	if err := c.Available(context.Background()); err == nil {
		t.Fatal("Available() reported a socket that does not exist as usable")
	}
	if _, err := c.Containers(context.Background()); err == nil {
		t.Fatal("Containers() succeeded against a socket that does not exist")
	}
}

func TestParseSelfCgroup(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{
			// cgroup v2 in a container: one line, whose path ends in the
			// container's own scope.
			"v2 systemd driver",
			"0::/system.slice/docker-" + testContainerID + ".scope",
			testContainerID,
		},
		{
			"v2 cgroupfs driver",
			"0::/docker/" + testContainerID,
			testContainerID,
		},
		{
			// On the host the path names a slice, not a container. Concluding
			// otherwise would make the agent warn about a hostname that is
			// perfectly stable.
			"running on the host",
			"0::/user.slice/user-1000.slice/session-3.scope",
			"",
		},
		{"empty", "", ""},
		{"malformed", "not a cgroup line", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseSelfCgroup(strings.NewReader(tc.in)); got != tc.want {
				t.Errorf("parseSelfCgroup() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The warn set is keyed by container id for the per-container diagnostics, so
// a host churning containers whose cgroups never resolve — the exact case that
// key exists for — would accumulate one entry per container forever. Same class
// of leak as the cgroup and cpu caches, and it needs the same pruning.
func TestContainerCollector_PrunesPerContainerWarnings(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sys", "fs", "cgroup"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sys", "fs", "cgroup", "cgroup.controllers"),
		[]byte("cpu memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withHostRoot(t, root)

	c := NewContainerCollector("test-agent", ContainerOptions{Interval: time.Second})
	c.mode = cgroupV2

	// A container the daemon reports but whose cgroup cannot be found: the
	// listing succeeds, the resolve does not, and a warning is recorded.
	c.warned[cgroupWarnPrefix+testContainerID] = true
	// A process-wide key, which must survive — it is not about any container.
	c.warned["docker-list"] = true

	collectOnce(t, c)

	if c.warned[cgroupWarnPrefix+testContainerID] {
		t.Error("a per-container warning was retained for a container that is gone")
	}
	if !c.warned["docker-list"] {
		t.Error("a process-wide warning was pruned; it would then be logged again every interval")
	}
}
