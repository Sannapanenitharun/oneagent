package collector

import (
	"os"
	"path/filepath"
	"testing"
)

// writeCgroup lays out a fake cgroup v2 directory. Files the test does not
// name are simply absent, which is the point: the reader has to cope with a
// kernel that has some controllers and not others, and a fixture that always
// wrote every file would never exercise that.
func writeCgroup(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	return dir
}

func TestReadCgroupStats_FullyPopulated(t *testing.T) {
	dir := writeCgroup(t, map[string]string{
		"cpu.stat": "usage_usec 1500000\nuser_usec 1000000\nsystem_usec 500000\n" +
			"nr_periods 100\nnr_throttled 7\nthrottled_usec 250000\n",
		"memory.current": "209715200\n",
		"memory.max":     "536870912\n",
		"memory.stat":    "anon 104857600\nfile 104857600\ninactive_file 52428800\nslab 1024\n",
		"io.stat": "8:0 rbytes=1024 wbytes=2048 rios=10 wios=20 dbytes=0 dios=0\n" +
			"8:16 rbytes=512 wbytes=256 rios=5 wios=3 dbytes=0 dios=0\n",
		"pids.current": "42\n",
	})

	s, err := readCgroupStats(dir)
	if err != nil {
		t.Fatalf("readCgroupStats: %v", err)
	}

	// Microseconds in the file, nanoseconds in the struct. Getting this
	// backwards would produce CPU figures a thousand times too small, which is
	// plausible enough to go unnoticed on a mostly-idle container.
	if s.CPUUsageNanos != 1_500_000_000 {
		t.Errorf("CPUUsageNanos = %d, want 1500000000", s.CPUUsageNanos)
	}
	if s.CPUUserNanos != 1_000_000_000 || s.CPUSystemNanos != 500_000_000 {
		t.Errorf("user/system = %d/%d, want 1000000000/500000000", s.CPUUserNanos, s.CPUSystemNanos)
	}
	if s.CPUThrottledNanos != 250_000_000 || s.CPUThrottledPeriods != 7 {
		t.Errorf("throttled = %d ns over %d periods, want 250000000/7", s.CPUThrottledNanos, s.CPUThrottledPeriods)
	}
	if s.MemoryLimit != 536870912 {
		t.Errorf("MemoryLimit = %d, want 536870912", s.MemoryLimit)
	}
	// Summed across both devices, not just the first line.
	if s.IOReadBytes != 1536 || s.IOWriteBytes != 2304 {
		t.Errorf("io = %d read / %d write, want 1536/2304", s.IOReadBytes, s.IOWriteBytes)
	}
	if s.PIDsCurrent != 42 {
		t.Errorf("PIDsCurrent = %d, want 42", s.PIDsCurrent)
	}
	if !s.HaveCPU || !s.HaveMemory || !s.HaveIO || !s.HavePIDs {
		t.Errorf("have flags = cpu:%v mem:%v io:%v pids:%v, want all true",
			s.HaveCPU, s.HaveMemory, s.HaveIO, s.HavePIDs)
	}
}

// A container using 200 MiB of which 50 MiB is reclaimable page cache is using
// 150 MiB as far as the OOM killer is concerned. Reporting memory.current
// unmodified is the single most common way container memory dashboards end up
// showing every container pinned near its limit.
func TestMemoryWorkingSet_ExcludesReclaimableCache(t *testing.T) {
	s := cgroupStats{MemoryCurrent: 209715200, MemoryInactiveFile: 52428800}
	if got := s.MemoryWorkingSet(); got != 157286400 {
		t.Errorf("MemoryWorkingSet() = %d, want 157286400", got)
	}
}

// inactive_file can momentarily exceed memory.current because the two are read
// from different files microseconds apart. Unsigned subtraction would wrap to
// roughly 18 exabytes and render every chart unusable.
func TestMemoryWorkingSet_DoesNotUnderflow(t *testing.T) {
	s := cgroupStats{MemoryCurrent: 100, MemoryInactiveFile: 200}
	if got := s.MemoryWorkingSet(); got != 0 {
		t.Errorf("MemoryWorkingSet() = %d, want 0", got)
	}
}

// "max" is the kernel's word for unconstrained. Parsing it as a number fails,
// and the failure must read as "no limit" rather than "a limit of zero" — a
// limit of zero would make every unconstrained container report 100% memory use.
func TestReadCgroupLimit_MaxMeansUnlimited(t *testing.T) {
	dir := writeCgroup(t, map[string]string{
		"memory.max":  "max\n",
		"memory.high": "1073741824\n",
	})

	if _, ok := readCgroupLimit(filepath.Join(dir, "memory.max")); ok {
		t.Error("readCgroupLimit(max) reported a limit; want ok=false")
	}
	v, ok := readCgroupLimit(filepath.Join(dir, "memory.high"))
	if !ok || v != 1073741824 {
		t.Errorf("readCgroupLimit = %d (ok=%v), want 1073741824 (ok=true)", v, ok)
	}
}

// A kernel without the io controller, or a delegated cgroup with pids disabled,
// should cost exactly those metrics. The alternative — failing the whole read —
// would turn one missing controller into a container with no data at all.
func TestReadCgroupStats_PartialControllers(t *testing.T) {
	dir := writeCgroup(t, map[string]string{
		"memory.current": "1048576\n",
	})

	s, err := readCgroupStats(dir)
	if err != nil {
		t.Fatalf("readCgroupStats: %v", err)
	}
	if !s.HaveMemory || s.MemoryCurrent != 1048576 {
		t.Errorf("memory not read: have=%v current=%d", s.HaveMemory, s.MemoryCurrent)
	}
	if s.HaveCPU || s.HaveIO || s.HavePIDs {
		t.Errorf("absent controllers reported as present: cpu:%v io:%v pids:%v",
			s.HaveCPU, s.HaveIO, s.HavePIDs)
	}
}

// A container that has issued no block I/O has an io.stat with no device lines.
// That is a real reading of zero, not a missing one, and must be emitted —
// otherwise an idle container simply vanishes from the I/O charts.
func TestReadCgroupIOStat_EmptyFileIsZeroNotError(t *testing.T) {
	dir := writeCgroup(t, map[string]string{"io.stat": ""})
	r, w, err := readCgroupIOStat(filepath.Join(dir, "io.stat"))
	if err != nil {
		t.Fatalf("readCgroupIOStat: %v", err)
	}
	if r != 0 || w != 0 {
		t.Errorf("got %d/%d, want 0/0", r, w)
	}
}

func TestReadCgroupStats_MissingDirectoryIsAnError(t *testing.T) {
	if _, err := readCgroupStats(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("readCgroupStats on a missing directory returned nil error")
	}
}

const testContainerID = "3f2a1b8c9d0e4f5a6b7c8d9e0f1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c"

// The systemd cgroup driver is the default on essentially every distribution
// that ships systemd, so this path resolving without a walk is what keeps the
// common case cheap.
func TestResolveCgroupDir_SystemdLayout(t *testing.T) {
	root := t.TempDir()
	want := filepath.Join(root, "system.slice", "docker-"+testContainerID+".scope")
	if err := os.MkdirAll(want, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := resolveCgroupDir(root, testContainerID)
	if err != nil {
		t.Fatalf("resolveCgroupDir: %v", err)
	}
	if got != filepath.ToSlash(want) && got != want {
		t.Errorf("resolveCgroupDir = %q, want %q", got, want)
	}
}

// A container started under a cgroup-parent, or inside a user slice, lands
// somewhere none of the candidate paths predict. The walk is what stops those
// hosts from being a silent hole in the data.
func TestResolveCgroupDir_FallsBackToSearch(t *testing.T) {
	root := t.TempDir()
	want := filepath.Join(root, "user.slice", "user-1000.slice", "docker-"+testContainerID+".scope")
	if err := os.MkdirAll(want, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := resolveCgroupDir(root, testContainerID)
	if err != nil {
		t.Fatalf("resolveCgroupDir: %v", err)
	}
	if filepath.Clean(got) != filepath.Clean(want) {
		t.Errorf("resolveCgroupDir = %q, want %q", got, want)
	}
}

func TestResolveCgroupDir_NotFound(t *testing.T) {
	if _, err := resolveCgroupDir(t.TempDir(), testContainerID); err == nil {
		t.Fatal("resolveCgroupDir found a container that does not exist")
	}
}

// The walk must terminate. A deep tree — /sys/fs/cgroup on a Kubernetes node is
// genuinely deep — would otherwise be traversed in full on every unresolvable
// container.
func TestSearchCgroupDir_RespectsDepthLimit(t *testing.T) {
	root := t.TempDir()
	deep := root
	for i := 0; i < cgroupSearchMaxDepth+3; i++ {
		deep = filepath.Join(deep, "level")
	}
	buried := filepath.Join(deep, "docker-"+testContainerID+".scope")
	if err := os.MkdirAll(buried, 0o755); err != nil {
		t.Fatal(err)
	}

	if got := searchCgroupDir(root, testContainerID, 0); got != "" {
		t.Errorf("searchCgroupDir descended past the depth limit and found %q", got)
	}
}

func TestReadCgroupFirstPID(t *testing.T) {
	dir := writeCgroup(t, map[string]string{"cgroup.procs": "\n1234\n1235\n"})
	pid, err := readCgroupFirstPID(dir)
	if err != nil {
		t.Fatalf("readCgroupFirstPID: %v", err)
	}
	if pid != 1234 {
		t.Errorf("pid = %d, want 1234", pid)
	}
}

// A container shutting down empties its cgroup between the listing and this
// read. It has to be an error rather than pid 0, which would send the network
// reader at /proc/0.
func TestReadCgroupFirstPID_EmptyCgroup(t *testing.T) {
	dir := writeCgroup(t, map[string]string{"cgroup.procs": ""})
	if pid, err := readCgroupFirstPID(dir); err == nil {
		t.Fatalf("readCgroupFirstPID returned pid %d for an empty cgroup; want an error", pid)
	}
}
