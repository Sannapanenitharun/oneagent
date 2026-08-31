package collector

import (
	"context"
	"encoding/json"
	"log"
	"path"
	"strings"
	"time"
)

// DockerLogCollector collects stdout and stderr from every container on the
// host by reading the json-file log driver's files directly.
//
// Reading files rather than streaming from the daemon's /containers/<id>/logs
// endpoint is the same choice Datadog made in Agent 6.33/7.33, and for the same
// reason: the socket path costs the daemon a goroutine and a copy per container
// and puts log delivery behind the health of a process that has nothing to do
// with logging, while the files are already on disk and are read by machinery
// this agent already has. It also means a wedged dockerd stops metadata, not
// logs.
//
// The files are the json-file driver's. A host configured with journald,
// local, or a remote driver has nothing here, which is detected and reported
// rather than presenting as a host whose containers never log.
type DockerLogCollector struct {
	agentID string
	root    string
	mgr     *tailManager
	docker  *dockerClient
	exclude []string

	// State below belongs to the tail manager's goroutine. Both hooks it runs
	// — handle and onTick — are called from there, which is what lets the
	// metadata cache and the partial-line buffers be plain maps.
	meta        map[string]dockerContainer
	partial     map[string]string
	discarding  map[string]bool
	lastRefresh time.Time
	// metaFailed remembers whether the last refresh failed, which is what
	// selects the longer retry interval above.
	metaFailed bool
	selfID     string
	send       func(Envelope)
}

// ContainerLogOptions configures container log collection.
type ContainerLogOptions struct {
	// Root is the directory the json-file driver writes under. Overridable
	// because a host with a relocated data-root does not have it in the usual
	// place, and because the containerised agent sees it through a bind mount.
	Root string
	// ExcludeNames drops containers whose name contains any of these.
	ExcludeNames []string
}

// DefaultDockerLogRoot is where the json-file driver stores logs on a stock
// Linux install: <data-root>/containers/<id>/<id>-json.log.
const DefaultDockerLogRoot = "/var/lib/docker/containers"

// dockerMetaRefresh bounds how often the daemon is asked to re-describe the
// containers it is running.
//
// The tail manager ticks at the poll interval — twice a second by default —
// and an HTTP round trip at that rate to label log lines would be absurd. A
// container's name and image never change during its life, so the only thing
// this interval delays is picking up a container that started since the last
// refresh, and those lines are labelled by id until it does rather than being
// dropped.
const dockerMetaRefresh = 30 * time.Second

// dockerMetaRetry is how long to wait after a failed refresh. Long, because a
// daemon that just refused to answer is not usually about to start, and every
// attempt costs the tailer the full request timeout. Container names go stale
// meanwhile; log lines keep flowing, labelled by id.
const dockerMetaRetry = 5 * time.Minute

// dockerLogLine is one record as the json-file driver writes it.
type dockerLogLine struct {
	Log    string `json:"log"`
	Stream string `json:"stream"`
	Time   string `json:"time"`
}

func NewDockerLogCollector(agentID string, opts ContainerLogOptions, tail TailingOptions, dockerEndpoint string) *DockerLogCollector {
	root := opts.Root
	if root == "" {
		root = DefaultDockerLogRoot
	}
	root = hostPath(root)

	c := &DockerLogCollector{
		agentID:    agentID,
		root:       root,
		docker:     newDockerClient(dockerEndpoint),
		exclude:    opts.ExcludeNames,
		meta:       map[string]dockerContainer{},
		partial:    map[string]string{},
		discarding: map[string]bool{},
	}

	c.mgr = newTailManager(tailOptions{
		// One glob covers every container, including ones created after
		// startup: the manager re-evaluates it on every scan, which is exactly
		// the behaviour a container host needs and the reason no separate
		// discovery loop is required here.
		globs:        []string{path.Join(root, "*", "*-json.log")},
		scanInterval: tail.ScanInterval,
		pollInterval: tail.PollInterval,
		maxLineBytes: tail.MaxLineBytes,
		registry:     tail.Registry,
		onTick:       c.refreshMetadata,
		onFileClosed: c.forget,
	})
	return c
}

func (c *DockerLogCollector) Name() string { return "container.logs" }

func (c *DockerLogCollector) Start(ctx context.Context, out chan<- Envelope) error {
	c.selfID = readSelfContainerID()
	c.send = func(env Envelope) {
		// Blocking back-pressure, matching LogTailCollector: slowing the tailer
		// when the exporter is behind loses nothing, whereas dropping loses a
		// log line permanently. ctx keeps shutdown responsive.
		select {
		case out <- env:
		case <-ctx.Done():
		}
	}
	c.mgr.opts.handle = c.handle
	c.mgr.Start(ctx)
	log.Printf("container logs: tailing %s", path.Join(c.root, "*", "*-json.log"))
	return nil
}

func (c *DockerLogCollector) Stop() error {
	c.mgr.Stop()
	return nil
}

// refreshMetadata re-reads the container list, at most every
// dockerMetaRefresh.
//
// Runs on the tail manager's goroutine, so the map it writes is the same one
// handle reads with no synchronisation. A failed refresh keeps the previous
// map: stale names are better than no names, and a daemon that is briefly
// unavailable should not blank the labels on every log line meanwhile.
func (c *DockerLogCollector) refreshMetadata() {
	now := time.Now()
	// A failed refresh waits far longer than a successful one before trying
	// again. This call runs on the tail manager's goroutine, so the time it
	// spends waiting on the daemon is time the tailer is not reading files —
	// negligible against a healthy local socket answering in milliseconds, but
	// a wedged dockerd holds it for the full timeout, and at the ordinary
	// interval that would stall log collection for three seconds out of every
	// thirty, indefinitely. Backing off turns a permanent 10% stall into a
	// negligible one while still recovering on its own once the daemon
	// returns.
	wait := dockerMetaRefresh
	if c.metaFailed {
		wait = dockerMetaRetry
	}
	if !c.lastRefresh.IsZero() && now.Sub(c.lastRefresh) < wait {
		return
	}
	c.lastRefresh = now

	ctx, cancel := context.WithTimeout(context.Background(), dockerAPITimeout)
	defer cancel()
	listed, err := c.docker.Containers(ctx)
	if err != nil {
		c.metaFailed = true
		return
	}
	c.metaFailed = false
	next := make(map[string]dockerContainer, len(listed))
	for _, ct := range listed {
		next[ct.ID] = ct
	}
	c.meta = next
}

// forget releases the partial-line buffer for a file the manager has dropped,
// so a host that churns containers does not accumulate one entry per container
// that ever ran.
func (c *DockerLogCollector) forget(fileID string) {
	delete(c.partial, fileID)
	delete(c.discarding, fileID)
}

// handle turns one line of a json-file log into an Envelope.
func (c *DockerLogCollector) handle(ln tailLine) {
	id := containerIDFromLogPath(ln.Path)
	if id == "" {
		return
	}
	if c.selfID != "" && id == c.selfID {
		// The agent's own container. Collecting it would ship the agent's
		// diagnostics about collecting logs as log data, which is noise at
		// best and confusing at worst when debugging a delivery problem.
		return
	}

	var rec dockerLogLine
	if err := json.Unmarshal([]byte(ln.Line), &rec); err != nil {
		// Not a json-file record. Skipped rather than forwarded raw: a file
		// under this path that is not this format is not something the agent
		// understands, and emitting the JSON text as a log message would put
		// unparsed escaping in front of whoever reads it.
		return
	}

	ct, known := c.meta[id]
	if !known {
		ct = dockerContainer{ID: id}
	}
	if c.excluded(ct) {
		return
	}

	msg := rec.Log

	// Everything after an over-long record was truncated is dropped until the
	// line actually ends, so the remainder does not surface as a second record
	// that looks like a real log line but begins mid-sentence. This mirrors the
	// file tailer's skipToNewline exactly — the two paths should not disagree
	// about what an over-long line does.
	if c.discarding[ln.FileID] {
		if strings.HasSuffix(msg, "\n") {
			delete(c.discarding, ln.FileID)
		}
		return
	}

	// Docker splits any line longer than 16 KiB into several records, marked
	// only by the absence of a trailing newline. Without reassembly a single
	// long log line — a stack trace, an encoded payload, a verbose SQL
	// statement — arrives as several unrelated records cut at arbitrary byte
	// boundaries, which is worse than truncation because nothing says it
	// happened.
	if !strings.HasSuffix(msg, "\n") {
		buffered := c.partial[ln.FileID] + msg
		// maxLineBytes bounds it for the same reason the tailer does: a stream
		// that never emits a newline must not buffer without limit.
		if max := c.mgr.opts.maxLineBytes; max > 0 && len(buffered) > max {
			delete(c.partial, ln.FileID)
			c.discarding[ln.FileID] = true
			c.emit(ln, ct, rec, buffered[:max]+truncatedSuffix)
			return
		}
		c.partial[ln.FileID] = buffered
		return
	}
	if held, ok := c.partial[ln.FileID]; ok {
		msg = held + msg
		delete(c.partial, ln.FileID)
	}

	msg = strings.TrimSuffix(msg, "\n")
	// The cap applies to a record that arrived complete in one chunk too, not
	// only to one accumulated across several.
	if max := c.mgr.opts.maxLineBytes; max > 0 && len(msg) > max {
		msg = msg[:max] + truncatedSuffix
	}
	c.emit(ln, ct, rec, msg)
}

func (c *DockerLogCollector) emit(ln tailLine, ct dockerContainer, rec dockerLogLine, message string) {
	labels := containerLabels(ct)
	if rec.Stream != "" {
		labels["stream"] = rec.Stream
	}
	// The file is kept as an attribute rather than as Source, because the path
	// is a 64-hex-character directory nobody can read. Source carries the
	// container's name instead — the thing an operator would filter on.
	labels["log.file.path"] = ln.Path

	// Docker's own timestamp, not the moment the agent read the line. On a
	// host catching up after a restart those differ by however long the agent
	// was down, and ordering by read time would interleave the backlog wrongly
	// against everything else.
	at := ln.At
	if rec.Time != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, rec.Time); err == nil {
			at = parsed.UTC()
		}
	}

	c.send(Envelope{
		Kind:      KindLog,
		AgentID:   c.agentID,
		Source:    "container/" + ct.Name(),
		Timestamp: at,
		Message:   message,
		// Provenance is what lets the exporter commit this file's read offset
		// once the line is actually delivered. Dropping it here would break
		// at-least-once for container logs specifically, which is the kind of
		// gap that only shows up as missing data after a crash.
		Labels: withTailProvenance(labels, ln),
	})
}

func (c *DockerLogCollector) excluded(ct dockerContainer) bool {
	name := ct.Name()
	for _, pat := range c.exclude {
		if pat != "" && strings.Contains(name, pat) {
			return true
		}
	}
	return false
}

// containerIDFromLogPath recovers the container id from the log file's path.
//
//	/var/lib/docker/containers/<id>/<id>-json.log
//
// The parent directory is used rather than the filename because the filename
// is what a rotated file's suffix is appended to (<id>-json.log.1), while the
// directory is stable.
func containerIDFromLogPath(p string) string {
	dir := path.Base(path.Dir(strings.ReplaceAll(p, "\\", "/")))
	if len(dir) != 64 || !isHex(dir) {
		return ""
	}
	return dir
}
