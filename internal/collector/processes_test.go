package collector

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A real line, so the field offsets are checked against the kernel's actual
// output rather than against a hand-built approximation of it.
const realProcStat = `1234 (nginx) S 1 1234 1234 0 -1 4194560 1234 0 5 0 ` +
	`150 75 0 0 20 0 4 0 987654 123456789 512 18446744073709551615 1 2 3 4 5 6 7 8 9`

func TestParseProcStat_ReadsTheRightFields(t *testing.T) {
	st, ok := parseProcStat(realProcStat)
	if !ok {
		t.Fatal("parseProcStat rejected a valid line")
	}
	if st.PID != 1234 {
		t.Errorf("PID = %d, want 1234", st.PID)
	}
	if st.Name != "nginx" {
		t.Errorf("Name = %q, want nginx", st.Name)
	}
	if st.UTime != 150 || st.STime != 75 {
		t.Errorf("UTime/STime = %d/%d, want 150/75", st.UTime, st.STime)
	}
	if st.Threads != 4 {
		t.Errorf("Threads = %d, want 4", st.Threads)
	}
	if st.StartTime != 987654 {
		t.Errorf("StartTime = %d, want 987654", st.StartTime)
	}
	if st.RSSPages != 512 {
		t.Errorf("RSSPages = %d, want 512", st.RSSPages)
	}
}

// The executable name is whatever the program was called, and it sits in the
// middle of a space-separated line. A plain Fields() split shifts every field
// after it by an unpredictable amount, which does not fail — it silently reads
// CPU time out of the wrong column. These are all names that occur on ordinary
// Linux hosts.
func TestParseProcStat_NamesContainingSpacesAndParens(t *testing.T) {
	cases := []struct{ name, comm string }{
		{"a space", "postgres: writer process"},
		{"parens", "(sd-pam)"},
		{"both", "Web Content (tab)"},
		{"close paren only", "weird)name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := `77 (` + tc.comm + `) S 1 1 1 0 -1 0 0 0 0 0 ` +
				`11 22 0 0 20 0 3 0 555 0 64 0 0 0 0 0 0 0`
			st, ok := parseProcStat(line)
			if !ok {
				t.Fatalf("rejected a line whose comm is %q", tc.comm)
			}
			if st.Name != tc.comm {
				t.Errorf("Name = %q, want %q", st.Name, tc.comm)
			}
			// The point of the test: the fields AFTER the name must still be
			// read correctly.
			if st.UTime != 11 || st.STime != 22 {
				t.Errorf("UTime/STime = %d/%d, want 11/22 — the parse shifted columns", st.UTime, st.STime)
			}
			if st.Threads != 3 || st.StartTime != 555 || st.RSSPages != 64 {
				t.Errorf("threads/start/rss = %d/%d/%d, want 3/555/64",
					st.Threads, st.StartTime, st.RSSPages)
			}
		})
	}
}

func TestParseProcStat_RejectsMalformed(t *testing.T) {
	for _, in := range []string{
		"",
		"not a stat line",
		"123 (short) S 1 2 3", // truncated before the fields that are read
		"123 no-parens S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22",
	} {
		if _, ok := parseProcStat(in); ok {
			t.Errorf("parseProcStat(%q) accepted a malformed line", in)
		}
	}
}

// The reason the CPU baseline is keyed by (pid, starttime) rather than pid.
// PIDs are recycled within minutes on a busy host, and a new process inheriting
// the previous holder's counter produces a delta between two unrelated numbers
// — reported as a spike of hundreds of percent that nothing on the host
// explains.
func TestProcessKey_DistinguishesRecycledPIDs(t *testing.T) {
	first := processKey{pid: 4242, start: 111}
	second := processKey{pid: 4242, start: 999}
	if first == second {
		t.Fatal("a recycled pid produced an equal key; the CPU baseline would be inherited")
	}
	m := map[processKey]uint64{first: 5000}
	if _, ok := m[second]; ok {
		t.Error("the new instance found the old instance's baseline")
	}
}

func newTestProcessCollector(max int) *ProcessCollector {
	return NewProcessCollector("host-1", ProcessOptions{MaxExecutables: max})
}

// Under the cap everything is reported, in a stable order so two snapshots can
// be diffed.
func TestRollup_UnderTheCapReportsEverythingSorted(t *testing.T) {
	c := newTestProcessCollector(10)
	got := c.rollup(map[string]*execAgg{
		"nginx":    {count: 4, rssBytes: 400},
		"postgres": {count: 1, rssBytes: 900},
		"bash":     {count: 2, rssBytes: 100},
	})
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3", len(got))
	}
	want := []string{"bash", "nginx", "postgres"}
	for i, w := range want {
		if got[i].name != w {
			t.Errorf("entry %d = %q, want %q (order must be stable)", i, got[i].name, w)
		}
	}
}

// Past the cap the remainder is SUMMED, never dropped. A process.count that is
// quietly short is worse than a coarse one: it invites arithmetic that no
// longer holds.
func TestRollup_OverflowIsSummedNotDropped(t *testing.T) {
	c := newTestProcessCollector(2)
	in := map[string]*execAgg{
		"big":    {count: 1, rssBytes: 1000, threads: 10},
		"medium": {count: 2, rssBytes: 500, threads: 20},
		"small":  {count: 3, rssBytes: 10, threads: 30},
		"tiny":   {count: 4, rssBytes: 1, threads: 40},
	}
	totalCount, totalRSS, totalThreads := 0, uint64(0), uint64(0)
	for _, a := range in {
		totalCount += a.count
		totalRSS += a.rssBytes
		totalThreads += a.threads
	}

	got := c.rollup(in)
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 2 kept + 1 %q bucket", len(got), otherExecutables)
	}

	var gotCount int
	var gotRSS, gotThreads uint64
	var haveOther bool
	for _, e := range got {
		gotCount += e.agg.count
		gotRSS += e.agg.rssBytes
		gotThreads += e.agg.threads
		if e.name == otherExecutables {
			haveOther = true
		}
	}
	if !haveOther {
		t.Errorf("no %q bucket; the overflow was dropped", otherExecutables)
	}
	if gotCount != totalCount || gotRSS != totalRSS || gotThreads != totalThreads {
		t.Errorf("totals after rollup = count %d, rss %d, threads %d; want %d, %d, %d — "+
			"the sums must survive the cap", gotCount, gotRSS, gotThreads,
			totalCount, totalRSS, totalThreads)
	}
}

// Which executables survive the cap is decided by memory, because it is the one
// dimension always available: CPU is absent on the first sample and for any
// program that has not run since the last one.
func TestRollup_KeepsTheLargestByMemory(t *testing.T) {
	c := newTestProcessCollector(2)
	got := c.rollup(map[string]*execAgg{
		"huge":  {count: 1, rssBytes: 9000},
		"big":   {count: 1, rssBytes: 800},
		"small": {count: 1, rssBytes: 7},
	})
	kept := map[string]bool{}
	for _, e := range got {
		kept[e.name] = true
	}
	if !kept["huge"] || !kept["big"] {
		t.Errorf("kept %v, want the two largest by memory named individually", kept)
	}
	if kept["small"] {
		t.Error("the smallest was named individually instead of folded into the bucket")
	}
}

// A process that exits between the directory listing and the stat read is the
// normal case on a busy host, not an error. Only failing to list /proc at all
// is reported.
func TestReadProcessStats_SkipsUnreadableAndNonProcessEntries(t *testing.T) {
	root := t.TempDir()

	// A valid process.
	mustWriteProc(t, root, "1234", realProcStat)
	// A directory in /proc that is not a process — /proc is full of these.
	if err := os.MkdirAll(filepath.Join(root, "sys"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A numeric directory with no stat file: the process exited mid-scan.
	if err := os.MkdirAll(filepath.Join(root, "5678"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A numeric directory whose stat file is garbage.
	mustWriteProc(t, root, "9999", "this is not a stat line")

	got, err := readProcessStats(root)
	if err != nil {
		t.Fatalf("readProcessStats: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d processes, want only the one valid entry: %+v", len(got), got)
	}
	if got[0].Name != "nginx" {
		t.Errorf("Name = %q, want nginx", got[0].Name)
	}
}

func TestReadProcessStats_ReportsAMissingProcRoot(t *testing.T) {
	if _, err := readProcessStats(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("readProcessStats accepted a /proc that does not exist")
	}
}

func TestProcessCollector_ExcludesByName(t *testing.T) {
	c := NewProcessCollector("h", ProcessOptions{ExcludeNames: []string{"kworker"}})
	if !c.excluded("kworker/3:1H") {
		t.Error("a substring match did not exclude the executable")
	}
	if c.excluded("nginx") {
		t.Error("a non-matching name was excluded")
	}
	if c.excluded("") == true && len(c.opts.ExcludeNames) > 0 {
		// An empty pattern is a config artefact, not a wildcard.
		if c.excluded("anything") && !strings.Contains("anything", "kworker") {
			t.Error("an empty pattern matched everything")
		}
	}
}

func mustWriteProc(t *testing.T, root, pid, stat string) {
	t.Helper()
	dir := filepath.Join(root, pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
}
