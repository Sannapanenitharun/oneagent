package collector

import (
	"context"
	"log"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ProcessCollector reports what is running on the host, rolled up by
// executable.
//
// The rollup is the whole design, not an optimisation. Per-process series are
// the classic way to destroy a metrics backend: a host running a build, a cron
// fleet or anything that forks produces thousands of distinct PIDs an hour, and
// a series keyed by PID is a series that is born, receives a handful of points
// and is never written to again. Ten thousand processes across forty programs
// is forty series here, not ten thousand — and forty is the number that answers
// the question an operator actually asks, which is "what is using this box",
// not "what did PID 24601 do before it exited".
//
// The consequence is stated rather than hidden: this collector cannot tell you
// about one process. Per-instance detail belongs on an event path, where a
// record has a lifetime and does not become a permanent dimension.
type ProcessCollector struct {
	agentID string
	opts    ProcessOptions
	gate    *sendGate
	stop    chan struct{}

	// Owned by the goroutine Start launches, in sample(). No mutex, matching
	// the rest of this package: state belongs to one goroutine.
	prev     map[processKey]uint64
	prevAt   time.Time
	pageSize uint64
	warned   map[string]bool
}

// ProcessOptions configures process collection.
type ProcessOptions struct {
	Interval time.Duration
	// MaxExecutables bounds how many distinct executables get their own series.
	// Past it the remainder is summed into one bucket rather than dropped, so
	// the totals still add up.
	MaxExecutables int
	// ExcludeNames drops executables whose name contains any of these.
	ExcludeNames []string
}

// processKey identifies a process INSTANCE, not a PID.
//
// PIDs are recycled, on a busy host within minutes. Keying the previous CPU
// reading by PID alone means a new process inherits the baseline of whatever
// used to hold that number, and the first delta after the reuse is the
// difference between two unrelated counters — reported as a spike of hundreds
// of percent that nothing on the host explains. starttime is fixed for the life
// of a process, so the pair cannot be confused.
type processKey struct {
	pid   int
	start uint64
}

// Process metric names. process.* follows the OpenTelemetry process semantic
// conventions; process.count has no equivalent there because those conventions
// describe a single process and this collector describes a population.
const (
	metricProcessCount   = "process.count"
	metricProcessCPUUtil = "process.cpu.utilization"
	metricProcessMemory  = "process.memory.usage"
	metricProcessThreads = "process.threads"
)

// userHZ is the unit of the CPU counters in /proc/<pid>/stat.
//
// The kernel exposes them in clock ticks and the tick rate is a compile-time
// constant reachable only through sysconf(_SC_CLK_TCK), which the standard
// library does not expose and which cgo would be a poor trade to obtain. It has
// been 100 on every Linux/amd64 and Linux/arm64 kernel in practice for two
// decades, and it is the value every pure-Go process reader assumes. Wrong only
// on a kernel built with a non-default CONFIG_HZ, where the effect is a
// proportional scaling of CPU percentages rather than nonsense.
const userHZ = 100.0

// defaultMaxExecutables bounds the series this collector can produce. Forty
// distinct programs is a typical server; a few hundred is a busy one. The cap
// is well above both, so reaching it means something is wrong — which is why
// it is reported rather than silently applied.
const defaultMaxExecutables = 256

// otherExecutables is the bucket everything past the cap is summed into. It
// exists so the totals stay true: an operator adding up process.count must get
// the number of processes on the host, not the number that fit.
const otherExecutables = "(other)"

func NewProcessCollector(agentID string, opts ProcessOptions) *ProcessCollector {
	if opts.Interval <= 0 {
		opts.Interval = 30 * time.Second
	}
	if opts.MaxExecutables <= 0 {
		opts.MaxExecutables = defaultMaxExecutables
	}
	return &ProcessCollector{
		agentID:  agentID,
		opts:     opts,
		gate:     newSendGate("processes"),
		stop:     make(chan struct{}),
		prev:     map[processKey]uint64{},
		pageSize: uint64(os.Getpagesize()),
		warned:   map[string]bool{},
	}
}

func (c *ProcessCollector) Name() string { return "processes" }

func (c *ProcessCollector) Start(ctx context.Context, out chan<- Envelope) error {
	// The first sample only establishes CPU baselines — there is no previous
	// reading to subtract from, so it emits counts and memory but no
	// utilisation. Taken immediately so that the first useful sample arrives
	// one interval in rather than two.
	c.sample(ctx, out)
	log.Printf("processes: reading %s, rolled up by executable (max %d)",
		hostPath("/proc"), c.opts.MaxExecutables)

	ticker := time.NewTicker(c.opts.Interval)
	go func() {
		defer ticker.Stop()
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

func (c *ProcessCollector) Stop() error {
	close(c.stop)
	return nil
}

// execAgg accumulates one executable's population.
type execAgg struct {
	count    int
	cpuTicks uint64 // delta since the previous sample, across all instances
	haveCPU  bool
	rssBytes uint64
	threads  uint64
}

func (c *ProcessCollector) sample(ctx context.Context, out chan<- Envelope) {
	now := time.Now()
	stats, err := readProcessStats(hostPath("/proc"))
	if err != nil {
		c.warnOnce("read", "processes: %v — process metrics are not collected", err)
		return
	}
	c.warned["read"] = false

	elapsed := now.Sub(c.prevAt).Seconds()
	haveElapsed := !c.prevAt.IsZero() && elapsed > 0

	byExec := make(map[string]*execAgg, 64)
	seen := make(map[processKey]struct{}, len(stats))

	for _, p := range stats {
		if c.excluded(p.Name) {
			continue
		}
		key := processKey{pid: p.PID, start: p.StartTime}
		seen[key] = struct{}{}

		a := byExec[p.Name]
		if a == nil {
			a = &execAgg{}
			byExec[p.Name] = a
		}
		a.count++
		a.rssBytes += p.RSSPages * c.pageSize
		a.threads += p.Threads

		total := p.UTime + p.STime
		if prev, ok := c.prev[key]; ok && haveElapsed && total >= prev {
			// total < prev cannot happen for one instance — the counter is
			// monotonic and the key pins the instance — so the guard is there
			// only to make an impossible reading harmless rather than negative.
			a.cpuTicks += total - prev
			a.haveCPU = true
		}
		c.prev[key] = total
	}

	// Forget instances that have exited. Without this the map grows for the
	// life of the process on any host that forks, which is every host.
	for key := range c.prev {
		if _, ok := seen[key]; !ok {
			delete(c.prev, key)
		}
	}
	c.prevAt = now

	for _, e := range c.rollup(byExec) {
		labels := map[string]string{"process.executable.name": e.name}
		c.gate.send(ctx, out, Envelope{
			Kind: KindMetric, AgentID: c.agentID, Source: metricProcessCount,
			Timestamp: now.UTC(), Value: float64(e.agg.count), Labels: labels,
		})
		c.gate.send(ctx, out, Envelope{
			Kind: KindMetric, AgentID: c.agentID, Source: metricProcessMemory,
			Timestamp: now.UTC(), Value: float64(e.agg.rssBytes), Labels: mapCopy(labels),
		})
		c.gate.send(ctx, out, Envelope{
			Kind: KindMetric, AgentID: c.agentID, Source: metricProcessThreads,
			Timestamp: now.UTC(), Value: float64(e.agg.threads), Labels: mapCopy(labels),
		})
		if e.agg.haveCPU && haveElapsed {
			// Percent of ONE core, matching container.cpu.utilization in this
			// same agent. A program with four busy threads reads 400, which is
			// the number `top` shows and the one an operator can reason about
			// without knowing the core count.
			pct := (float64(e.agg.cpuTicks) / userHZ) / elapsed * 100
			c.gate.send(ctx, out, Envelope{
				Kind: KindMetric, AgentID: c.agentID, Source: metricProcessCPUUtil,
				Timestamp: now.UTC(), Value: pct, Labels: mapCopy(labels),
			})
		}
	}
}

// namedAgg pairs an executable with its totals, for ordering.
type namedAgg struct {
	name string
	agg  *execAgg
}

// rollup applies the cardinality cap.
//
// Past the cap the remainder is SUMMED into one bucket rather than dropped.
// Dropping would make process.count disagree with the number of processes on
// the host, and a total that is quietly short is worse than a coarse one: it
// invites conclusions from arithmetic that no longer holds.
func (c *ProcessCollector) rollup(byExec map[string]*execAgg) []namedAgg {
	all := make([]namedAgg, 0, len(byExec))
	for name, a := range byExec {
		all = append(all, namedAgg{name: name, agg: a})
	}
	if len(all) <= c.opts.MaxExecutables {
		// Sorted regardless, so the emission order is stable between samples
		// and a diff of two snapshots is readable.
		sort.Slice(all, func(i, j int) bool { return all[i].name < all[j].name })
		return all
	}

	// Which ones survive is decided by memory: it is the dimension that is
	// always available (CPU is absent on the first sample and for a process
	// that has not run since the last one) and the one that identifies the
	// programs worth naming.
	sort.Slice(all, func(i, j int) bool {
		if all[i].agg.rssBytes != all[j].agg.rssBytes {
			return all[i].agg.rssBytes > all[j].agg.rssBytes
		}
		return all[i].name < all[j].name
	})

	kept := all[:c.opts.MaxExecutables]
	other := &execAgg{}
	for _, e := range all[c.opts.MaxExecutables:] {
		other.count += e.agg.count
		other.rssBytes += e.agg.rssBytes
		other.threads += e.agg.threads
		other.cpuTicks += e.agg.cpuTicks
		other.haveCPU = other.haveCPU || e.agg.haveCPU
	}
	c.warnOnce("cardinality", "processes: %d distinct executables exceeds the cap of %d — "+
		"the %d largest by memory are reported individually and the remaining %d are summed "+
		"into %q, so totals stay correct while the series count stays bounded",
		len(all), c.opts.MaxExecutables, c.opts.MaxExecutables, len(all)-c.opts.MaxExecutables,
		otherExecutables)

	sort.Slice(kept, func(i, j int) bool { return kept[i].name < kept[j].name })
	return append(kept, namedAgg{name: otherExecutables, agg: other})
}

func (c *ProcessCollector) excluded(name string) bool {
	for _, pat := range c.opts.ExcludeNames {
		if pat != "" && strings.Contains(name, pat) {
			return true
		}
	}
	return false
}

func (c *ProcessCollector) warnOnce(key, format string, args ...any) {
	if c.warned[key] {
		return
	}
	c.warned[key] = true
	log.Printf(format, args...)
}

// procStat is the subset of /proc/<pid>/stat this collector reads.
type procStat struct {
	PID       int
	Name      string
	UTime     uint64
	STime     uint64
	Threads   uint64
	StartTime uint64
	RSSPages  uint64
}

// readProcessStats reads every process under procRoot.
//
// Errors on individual processes are not errors: a process that exits between
// the directory listing and the read of its stat file is the normal case on any
// busy host, and treating it as a failure would make the collector noisiest
// exactly when the host is busiest. Only failing to list /proc at all is
// reported.
func readProcessStats(procRoot string) ([]procStat, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	out := make([]procStat, 0, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue // /proc holds plenty that is not a process
		}
		raw, err := os.ReadFile(path.Join(procRoot, e.Name(), "stat"))
		if err != nil {
			continue
		}
		st, ok := parseProcStat(string(raw))
		if !ok {
			continue
		}
		out = append(out, st)
	}
	return out, nil
}

// parseProcStat parses one /proc/<pid>/stat line.
//
// The second field is the executable name in parentheses, and it is the reason
// this cannot be a plain Fields() split: the name is whatever the program was
// called, so it may contain spaces AND parentheses — "(sd-pam)" and
// "postgres: writer process" both occur on ordinary hosts. Splitting on
// whitespace shifts every subsequent field by an unpredictable amount, which
// silently reads CPU time out of the wrong column.
//
// Scanning to the LAST ')' is what the kernel's own documentation recommends
// and is the only parse that is correct for every name.
func parseProcStat(s string) (procStat, bool) {
	open := strings.IndexByte(s, '(')
	close := strings.LastIndexByte(s, ')')
	if open < 0 || close < 0 || close < open {
		return procStat{}, false
	}

	pid, err := strconv.Atoi(strings.TrimSpace(s[:open]))
	if err != nil {
		return procStat{}, false
	}
	name := s[open+1 : close]
	if name == "" {
		return procStat{}, false
	}

	// Fields after the name, where index 0 is field 3 (state) in the numbering
	// used by proc(5). Everything below is that numbering minus three.
	f := strings.Fields(s[close+1:])
	const (
		iUTime     = 11 // field 14
		iSTime     = 12 // field 15
		iThreads   = 17 // field 20
		iStartTime = 19 // field 22
		iRSS       = 21 // field 24, in pages
	)
	if len(f) <= iRSS {
		return procStat{}, false
	}

	num := func(i int) uint64 {
		v, err := strconv.ParseUint(f[i], 10, 64)
		if err != nil {
			return 0
		}
		return v
	}
	return procStat{
		PID:       pid,
		Name:      name,
		UTime:     num(iUTime),
		STime:     num(iSTime),
		Threads:   num(iThreads),
		StartTime: num(iStartTime),
		RSSPages:  num(iRSS),
	}, true
}
