package collector

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newTestRegistry(t *testing.T) *OffsetRegistry {
	t.Helper()
	reg, err := NewOffsetRegistry(filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatalf("NewOffsetRegistry: %v", err)
	}
	return reg
}

func newTestStreamCollector(t *testing.T, reg *OffsetRegistry) *DockerLogStreamCollector {
	t.Helper()
	c := NewDockerLogStreamCollector("host-1", ContainerLogOptions{}, TailingOptions{
		Registry:     reg,
		MaxLineBytes: 1024,
	}, "/var/run/docker.sock")
	c.started = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	return c
}

// A recorded cursor is the whole point of persisting one: after a restart the
// stream must resume where delivery stopped, not at "now" (losing everything
// written while the agent was down) and not at the beginning (re-sending the
// container's entire history).
func TestResumeFrom_UsesTheRecordedCursor(t *testing.T) {
	reg := newTestRegistry(t)
	c := newTestStreamCollector(t, reg)

	cursor := time.Date(2026, 8, 31, 11, 30, 0, 0, time.UTC)
	reg.Commit(dockerCursorID("abc123"), "container/web", cursor.UnixNano(), 0)

	got := c.resumeFrom(dockerContainer{ID: "abc123"})
	if !got.Equal(cursor) {
		t.Errorf("resumeFrom = %v, want the recorded cursor %v", got, cursor)
	}
}

// A container that started after the agent did is new, so all of its output is
// wanted. A zero time means "no since parameter", which asks the daemon for
// everything it has — a handful of lines for a container that just started.
func TestResumeFrom_NewContainerStreamsFromTheBeginning(t *testing.T) {
	c := newTestStreamCollector(t, newTestRegistry(t))

	ct := dockerContainer{ID: "new1", Created: c.started.Add(time.Minute).Unix()}
	if got := c.resumeFrom(ct); !got.IsZero() {
		t.Errorf("resumeFrom = %v, want the zero time so the whole log is streamed", got)
	}
}

// A container that predates the agent has however many days of accumulated
// logs. Replaying them the first time container collection is switched on would
// flood the backend with history nobody asked for, so collection starts at the
// point the agent did — the same rule the file tailer applies to a file it has
// never seen.
func TestResumeFrom_PreExistingContainerSkipsHistory(t *testing.T) {
	c := newTestStreamCollector(t, newTestRegistry(t))

	ct := dockerContainer{ID: "old1", Created: c.started.Add(-48 * time.Hour).Unix()}
	if got := c.resumeFrom(ct); !got.Equal(c.started) {
		t.Errorf("resumeFrom = %v, want the collector's start time %v", got, c.started)
	}
}

// A container discovered through the cgroup fallback has no Created time. It
// must be treated as pre-existing rather than replayed from the beginning:
// zero would be read as "the epoch", and asking for everything since 1970 is
// the flood this rule exists to prevent.
func TestResumeFrom_UnknownCreationTimeSkipsHistory(t *testing.T) {
	c := newTestStreamCollector(t, newTestRegistry(t))

	if got := c.resumeFrom(dockerContainer{ID: "nocreate"}); !got.Equal(c.started) {
		t.Errorf("resumeFrom = %v, want the collector's start time %v", got, c.started)
	}
}

// The cursor rides on the envelope so that it is committed downstream, once the
// line has actually been delivered. Without these labels the api path would
// have no at-least-once guarantee at all — a crash between reading a line and
// exporting it would lose it silently.
func TestStreamEmit_CarriesTheResumeCursor(t *testing.T) {
	c := newTestStreamCollector(t, newTestRegistry(t))

	at := time.Date(2026, 8, 31, 12, 5, 0, 500, time.UTC)
	var got Envelope
	c.emit(func(e Envelope) { got = e },
		dockerContainer{ID: "abcdef0123456789", Names: []string{"/web"}, Image: "nginx:1.27"},
		dockerStreamLine{Stream: "stderr", At: at, Text: "upstream timed out"})

	if got.Kind != KindLog {
		t.Errorf("Kind = %v, want a log", got.Kind)
	}
	if got.Source != "container/web" {
		t.Errorf("Source = %q, want the container name", got.Source)
	}
	if got.Message != "upstream timed out" {
		t.Errorf("Message = %q", got.Message)
	}
	if !got.Timestamp.Equal(at) {
		t.Errorf("Timestamp = %v, want the daemon's %v", got.Timestamp, at)
	}
	if got.Labels["stream"] != "stderr" {
		t.Errorf("stream label = %q", got.Labels["stream"])
	}
	if got.Labels["container.image.name"] != "nginx:1.27" {
		t.Errorf("image label = %q", got.Labels["container.image.name"])
	}
	if got.Labels[LabelTailID] != "docker:abcdef0123456789" {
		t.Errorf("%s = %q", LabelTailID, got.Labels[LabelTailID])
	}
	if want := strconv.FormatInt(at.UnixNano(), 10); got.Labels[LabelTailEnd] != want {
		t.Errorf("%s = %q, want %q", LabelTailEnd, got.Labels[LabelTailEnd], want)
	}
}

// The cursor must never be advanced by a line whose position is unknown.
// Committing "now" for an untimestamped record would move the resume point past
// records that have not been delivered, turning a recoverable duplicate into a
// permanent gap.
func TestStreamEmit_OmitsCursorWhenTheTimestampIsUnknown(t *testing.T) {
	c := newTestStreamCollector(t, newTestRegistry(t))

	var got Envelope
	c.emit(func(e Envelope) { got = e },
		dockerContainer{ID: "abc", Names: []string{"/web"}},
		dockerStreamLine{Stream: "stdout", Text: "line with no timestamp"})

	if got.Message != "line with no timestamp" {
		t.Errorf("Message = %q — the line must still be delivered", got.Message)
	}
	if _, ok := got.Labels[LabelTailID]; ok {
		t.Errorf("envelope carries a cursor (%q) for a line with no known position",
			got.Labels[LabelTailEnd])
	}
	if got.Timestamp.IsZero() {
		t.Error("Timestamp is zero; the envelope still needs a time to be ordered by")
	}
}

// The end-to-end reason the cursor exists: what the collector emits has to be
// something the existing commit path understands, so that the api reader gets
// the same at-least-once behaviour the file reader already has.
func TestStreamEmit_CursorIsCommittableByTheExistingPath(t *testing.T) {
	reg := newTestRegistry(t)
	c := newTestStreamCollector(t, reg)

	at := time.Date(2026, 8, 31, 12, 6, 0, 0, time.UTC)
	var env Envelope
	c.emit(func(e Envelope) { env = e },
		dockerContainer{ID: "feed01", Names: []string{"/api"}},
		dockerStreamLine{Stream: "stdout", At: at, Text: "served"})

	CommitTailOffset(reg, env)

	got, _, ok := reg.Lookup(dockerCursorID("feed01"))
	if !ok {
		t.Fatal("CommitTailOffset recorded nothing for the container")
	}
	if got != at.UnixNano() {
		t.Errorf("stored cursor = %d, want %d", got, at.UnixNano())
	}
}

// Exclusions have to mean the same thing whichever reader the host selected.
func TestExcludedByName_SharedBetweenBothLogReaders(t *testing.T) {
	ct := dockerContainer{ID: "x", Names: []string{"/buildkit-worker"}}
	if !excludedByName(ct, []string{"buildkit"}) {
		t.Error("a substring match did not exclude the container")
	}
	if excludedByName(ct, []string{"postgres"}) {
		t.Error("a non-matching pattern excluded the container")
	}
	// An empty pattern is a config artefact (a trailing list item), not a
	// wildcard. Treating it as one would silently collect nothing.
	if excludedByName(ct, []string{""}) {
		t.Error("an empty exclusion pattern excluded everything")
	}
}

func TestResolveContainerLogSource(t *testing.T) {
	readable := t.TempDir()

	// A directory that does not exist takes the same branch a 0700 root-owned
	// one does for an unprivileged agent: os.Open fails, so the files cannot be
	// the source. It is tested this way because the test binary runs as root in
	// CI, and root bypasses the permission bit the real case turns on.
	missing := filepath.Join(readable, "definitely-not-here")

	cases := []struct {
		name string
		want ContainerLogSource
		give ContainerLogSource
		root string
	}{
		{"auto picks the files when they are readable", ContainerLogSourceFile, ContainerLogSourceAuto, readable},
		{"auto falls back to the api when they are not", ContainerLogSourceAPI, ContainerLogSourceAuto, missing},
		{"an explicit file choice is honoured even when unreadable", ContainerLogSourceFile, ContainerLogSourceFile, missing},
		{"an explicit api choice is honoured even when readable", ContainerLogSourceAPI, ContainerLogSourceAPI, readable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, why := ResolveContainerLogSource(tc.give, tc.root)
			if got != tc.want {
				t.Errorf("source = %q, want %q (reason given: %s)", got, tc.want, why)
			}
			if why == "" {
				t.Error("no reason was returned; the startup log has nothing to print")
			}
		})
	}
}

// The fallback message is the only thing that tells an operator why their
// container logs are coming from the daemon, and what to grant if they would
// rather they did not.
func TestResolveContainerLogSource_FallbackSaysWhatToGrant(t *testing.T) {
	_, why := ResolveContainerLogSource(ContainerLogSourceAuto,
		filepath.Join(t.TempDir(), "absent"))
	for _, want := range []string{"docker api", "docker group"} {
		if !strings.Contains(why, want) {
			t.Errorf("fallback reason %q does not mention %q", why, want)
		}
	}
}

// Without --cgroupns host the agent cannot read its own id out of
// /proc/self/cgroup, and an agent that does not recognise itself collects its
// own stdout. For the log collectors that is not merely redundant: each
// envelope the agent writes is read back, wrapped in another envelope and
// written again, so the escaping doubles every pass until the pipeline is
// saturated. Confirming the hostname against the daemon's own list is what
// closes that hole.
func TestResolveSelfContainerID_FallsBackToHostnameConfirmedByTheDaemon(t *testing.T) {
	const host = "abcdef012345"
	full := host + "6789abcdef0123456789abcdef0123456789abcdef0123456789"

	got := resolveSelfContainerIDWithHost(host, "", []dockerContainer{
		{ID: "1111111111112222222222223333333333334444444444445555555555556666"},
		{ID: full},
	})
	if got != full {
		t.Errorf("resolveSelfContainerID = %q, want the container whose id the hostname prefixes (%q)", got, full)
	}
}

// A cgroup-derived id is authoritative and must not be second-guessed by a
// hostname that happens to look like one.
func TestResolveSelfContainerID_PrefersTheCgroupAnswer(t *testing.T) {
	const fromCgroup = "aaaaaaaaaaaabbbbbbbbbbbbccccccccccccddddddddddddeeeeeeeeeeeeffff"
	got := resolveSelfContainerIDWithHost("abcdef012345", fromCgroup, []dockerContainer{
		{ID: "abcdef0123459999999999999999999999999999999999999999999999999999"},
	})
	if got != fromCgroup {
		t.Errorf("resolveSelfContainerID = %q, want the cgroup answer %q", got, fromCgroup)
	}
}

// An ordinary hostname must never be prefix-matched against container ids: a
// host called "web" would otherwise exclude any container whose id starts with
// those letters.
func TestResolveSelfContainerID_IgnoresHostnamesThatAreNotShortIDs(t *testing.T) {
	listed := []dockerContainer{
		{ID: "web01abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234"},
	}
	for _, host := range []string{"web01", "ip-172-31-33-81", "not-hex-here", "abcdefghijkl"} {
		if got := resolveSelfContainerIDWithHost(host, "", listed); got != "" {
			t.Errorf("hostname %q matched container %q; only a 12-hex short id may", host, got)
		}
	}
}

// A hostname that looks like a short id but matches nothing running is not the
// agent's container. Returning it anyway would exclude a container that does
// not exist, which is harmless, but the confirmation is the whole reason this
// fallback is safe.
func TestResolveSelfContainerID_RequiresAMatchInTheDaemonsList(t *testing.T) {
	got := resolveSelfContainerIDWithHost("abcdef012345", "", []dockerContainer{
		{ID: "999999999999888888888888777777777777666666666666555555555555444"},
	})
	if got != "" {
		t.Errorf("resolveSelfContainerID = %q, want empty when nothing running matches", got)
	}
}

// The cursors are useless unless they reach disk. The file-tailing collectors
// get that from the tail manager, which flushes the shared registry on every
// scan; this collector has no tail manager, so it has to do it itself. Without
// this the resume cursor lives only in memory and every restart begins at
// "now", losing whatever each container logged while the agent was down —
// which is the exact failure the registry exists to prevent.
func TestStreamCollector_PersistsCursorsToDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")

	reg, err := NewOffsetRegistry(path)
	if err != nil {
		t.Fatalf("NewOffsetRegistry: %v", err)
	}
	c := newTestStreamCollector(t, reg)

	at := time.Date(2026, 8, 31, 12, 7, 0, 0, time.UTC)
	var env Envelope
	c.emit(func(e Envelope) { env = e },
		dockerContainer{ID: "persist01", Names: []string{"/web"}},
		dockerStreamLine{Stream: "stdout", At: at, Text: "hello"})
	CommitTailOffset(reg, env)

	c.flushCursors()

	// Re-open from disk: this is what the next process does.
	reopened, err := NewOffsetRegistry(path)
	if err != nil {
		t.Fatalf("re-opening the registry: %v", err)
	}
	got, _, ok := reopened.Lookup(dockerCursorID("persist01"))
	if !ok {
		t.Fatal("the cursor did not survive a flush and re-open")
	}
	if got != at.UnixNano() {
		t.Errorf("cursor = %d, want %d", got, at.UnixNano())
	}

	// And the collector must then resume from it rather than from its own
	// start time, which is the behaviour the persistence is for.
	next := newTestStreamCollector(t, reopened)
	if resume := next.resumeFrom(dockerContainer{ID: "persist01"}); !resume.Equal(at) {
		t.Errorf("resumeFrom after restart = %v, want the persisted cursor %v", resume, at)
	}
}
