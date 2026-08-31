package collector

import (
	"context"
	"errors"
	"io"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DockerLogStreamCollector collects container stdout and stderr by following
// the Engine API's log endpoint, one stream per container.
//
// It exists because the file-based collector cannot work for an agent that is
// not root. The json-file driver writes to /var/lib/docker/containers, which is
// mode 0700 and owned by root; no group grant opens it, so a packaged agent
// running as its own service account gets container metrics and container
// network counters but no container logs at all. The daemon's log endpoint is
// reachable with membership of the docker group, which is the same grant that
// already buys container names.
//
// The trade against DockerLogCollector is real and worth stating. Streaming
// costs the daemon a goroutine and a copy per container, and it puts log
// delivery behind the health of a process that has nothing to do with logging:
// a wedged dockerd stops logs here, where the file reader would keep going. So
// the file reader stays the default wherever its directory is readable, and
// this is what a host uses when it is not. See containerLogSource.
type DockerLogStreamCollector struct {
	agentID  string
	docker   *dockerClient
	exclude  []string
	registry *OffsetRegistry
	maxLine  int
	// refresh is how often the container list is re-read to pick up new
	// containers. It is not a polling interval for the logs themselves: those
	// arrive on an open connection as they are written.
	refresh time.Duration

	selfID string
	// started is the cut-off for a container the agent has never seen before.
	// See resumeFrom.
	started time.Time

	stop chan struct{}
	done chan struct{}

	// readers is owned by the manager goroutine alone. Each entry cancels one
	// follower. The follower goroutines share nothing with it: they are handed
	// their container by value at start and communicate only by sending on the
	// envelope channel, which is what keeps this collector free of locks over
	// its own state.
	readers map[string]context.CancelFunc
	wg      sync.WaitGroup
	// listWarned belongs to the manager goroutine. Per-container warnings are
	// deliberately NOT held here: they are local to the follower that produces
	// them, because a map shared between the manager and every follower would
	// be a race, and the state is per container anyway.
	listWarned bool
	// running records whether Start launched the manager goroutine, so Stop
	// does not wait on one that was never started.
	running bool
}

// dockerStreamRefresh bounds how often the container list is re-read. New
// containers wait at most this long before their logs are followed; their
// earlier lines are not lost, because a container the collector has never seen
// is streamed from the beginning of its log.
const dockerStreamRefresh = 10 * time.Second

// dockerStreamRetry is the pause before reconnecting a stream that ended.
// Streams end for ordinary reasons — the container exited, the daemon was
// restarted — so this is a settle time, not a backoff from an error condition.
const dockerStreamRetry = 5 * time.Second

func NewDockerLogStreamCollector(agentID string, opts ContainerLogOptions, tail TailingOptions, dockerEndpoint string) *DockerLogStreamCollector {
	return &DockerLogStreamCollector{
		agentID:  agentID,
		docker:   newDockerClient(dockerEndpoint),
		exclude:  opts.ExcludeNames,
		registry: tail.Registry,
		maxLine:  tail.MaxLineBytes,
		refresh:  dockerStreamRefresh,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		readers:  map[string]context.CancelFunc{},
	}
}

func (c *DockerLogStreamCollector) Name() string { return "container.logs.api" }

// Start never returns an error, including when the socket is missing.
//
// A collector that fails to start aborts the agent, and an unreachable Docker
// socket is a configuration fact about one signal, not a reason to stop
// collecting host metrics. The manager loop says so once and keeps looking, so
// a host where dockerd is installed or restarted later recovers on its own.
func (c *DockerLogStreamCollector) Start(ctx context.Context, out chan<- Envelope) error {
	c.selfID = readSelfContainerID()

	send := func(env Envelope) {
		// Blocking back-pressure, matching every other log path: slowing the
		// reader when the exporter is behind loses nothing, whereas dropping
		// loses a line permanently.
		select {
		case out <- env:
		case <-ctx.Done():
		}
	}

	c.started = time.Now().UTC()
	c.running = true
	go c.run(ctx, send)
	log.Printf("container logs: following the docker api at %s", c.docker.endpoint)
	return nil
}

// Stop tolerates never having been started. Start and Stop are both called from
// the daemon's own goroutine, so the flag needs no synchronisation — and
// without it, stopping a collector whose Start never ran would wait forever on
// a goroutine that was never launched.
func (c *DockerLogStreamCollector) Stop() error {
	if !c.running {
		return nil
	}
	c.running = false
	close(c.stop)
	<-c.done
	c.docker.Close()
	return nil
}

// run is the manager goroutine: it keeps the set of followers in step with the
// set of running containers.
func (c *DockerLogStreamCollector) run(ctx context.Context, send func(Envelope)) {
	defer close(c.done)
	defer func() {
		// Cancel every follower and wait for it before reporting done, so Stop
		// does not return while goroutines are still sending on a channel the
		// daemon is about to stop draining.
		for id, cancel := range c.readers {
			cancel()
			delete(c.readers, id)
		}
		c.wg.Wait()
		// Persist whatever was committed while shutting down, so a clean stop
		// resumes exactly where it left off rather than at the last periodic
		// flush.
		c.flushCursors()
	}()

	t := time.NewTicker(c.refresh)
	defer t.Stop()

	c.reconcile(ctx, send)
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stop:
			return
		case <-t.C:
			c.reconcile(ctx, send)
			c.flushCursors()
		}
	}
}

// flushCursors writes the resume cursors to disk.
//
// The file-tailing collectors get this from the tail manager, which flushes the
// same registry on every scan. This collector has no tail manager — it follows
// sockets, not files — so without this the cursors it commits live only in
// memory, and every restart resumes at "now", silently losing whatever each
// container logged while the agent was down. That is precisely the failure the
// registry exists to prevent, so it is done on the same schedule.
func (c *DockerLogStreamCollector) flushCursors() {
	if err := c.registry.Flush(); err != nil {
		log.Printf("container logs: persisting resume cursors: %v", err)
	}
}

// reconcile starts a follower for every container that should have one and
// cancels the followers whose containers are gone.
func (c *DockerLogStreamCollector) reconcile(ctx context.Context, send func(Envelope)) {
	listCtx, cancel := context.WithTimeout(ctx, dockerAPITimeout)
	listed, err := c.docker.Containers(listCtx)
	cancel()
	if err != nil {
		// Deliberately does not tear down the existing followers: they are on
		// their own connections and keep delivering while the list call is
		// failing, which is exactly what should happen when the daemon is
		// briefly busy.
		if !c.listWarned {
			c.listWarned = true
			log.Printf("container logs: %v — no new containers will be followed "+
				"until the daemon answers again", err)
		}
		return
	}
	c.listWarned = false
	// Re-checked on every listing rather than once at startup: the hostname
	// fallback needs the daemon's list to confirm against, and at Start there
	// is none. Cheap, and it means an agent that could not identify itself on
	// the first pass is not stuck collecting its own output forever.
	c.selfID = resolveSelfContainerID(c.selfID, listed)

	running := make(map[string]bool, len(listed))
	for _, ct := range listed {
		if ct.ID == "" || (c.selfID != "" && ct.ID == c.selfID) {
			continue
		}
		if excludedByName(ct, c.exclude) {
			continue
		}
		running[ct.ID] = true
		if _, following := c.readers[ct.ID]; following {
			continue
		}
		readerCtx, cancelReader := context.WithCancel(ctx)
		c.readers[ct.ID] = cancelReader
		c.wg.Add(1)
		go c.follow(readerCtx, ct, send)
	}

	for id, cancelReader := range c.readers {
		if !running[id] {
			cancelReader()
			delete(c.readers, id)
		}
	}
}

// follow keeps one container's log stream open until its context is cancelled.
//
// It runs on its own goroutine and owns nothing shared: ct is a copy, the
// cursor lives in the registry (which is already safe for concurrent use), and
// send is a channel send. That is what lets the manager above hold its maps
// without a lock.
func (c *DockerLogStreamCollector) follow(ctx context.Context, ct dockerContainer, send func(Envelope)) {
	defer c.wg.Done()

	// Per-container warn state, held as a local rather than in a shared map so
	// that a repeatedly-failing stream logs once without the manager and every
	// follower writing to the same map.
	var warned bool

	for ctx.Err() == nil {
		err := c.stream(ctx, ct, send)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, errLogDriverUnsupported):
			// The container's logging driver cannot replay logs at all —
			// journald, awslogs, a syslog sink. Retrying forever would be a
			// request every five seconds for the life of the container against
			// an answer that cannot change while it runs.
			log.Printf("container logs: %s uses a log driver the api cannot read (%v) — "+
				"its logs are not collected", ct.Name(), err)
			return
		case err != nil && !errors.Is(err, io.EOF):
			if !warned {
				warned = true
				log.Printf("container logs: %s: %v — reconnecting", ct.Name(), err)
			}
		default:
			// A clean end (the container exited, or the daemon closed the
			// connection) clears the warning, so a stream that recovers and
			// later fails again says so instead of failing silently.
			warned = false
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(dockerStreamRetry):
		}
	}
}

// stream opens one connection and decodes it until it ends.
func (c *DockerLogStreamCollector) stream(ctx context.Context, ct dockerContainer, send func(Envelope)) error {
	ttyCtx, cancel := context.WithTimeout(ctx, dockerAPITimeout)
	tty, err := c.docker.TTY(ttyCtx, ct.ID)
	cancel()
	if err != nil {
		return err
	}

	// A context for this connection alone. The watchdog below is tied to it
	// rather than to the follower's context, so a stream that ends on its own
	// releases the goroutine immediately. Tying it to the follower's context
	// would leak one goroutine per reconnect for the life of the container.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()

	body, err := c.docker.Logs(streamCtx, ct.ID, c.resumeFrom(ct))
	if err != nil {
		return err
	}
	defer body.Close()

	// Closing the body is what unblocks a read parked waiting for the next
	// line. The request's context does that eventually, but only once the
	// transport notices; closing directly makes shutdown immediate.
	go func() {
		<-streamCtx.Done()
		body.Close()
	}()

	dec := newDockerStreamDecoder(tty, c.maxLine)
	return dec.Decode(body, func(l dockerStreamLine) {
		c.emit(send, ct, l)
	})
}

// resumeFrom decides where a container's stream should start.
func (c *DockerLogStreamCollector) resumeFrom(ct dockerContainer) time.Time {
	if nanos, _, ok := c.registry.Lookup(dockerCursorID(ct.ID)); ok && nanos > 0 {
		return time.Unix(0, nanos).UTC()
	}
	// No cursor: this container has not been followed before.
	//
	// One that was created after the collector started is new, so its whole log
	// is wanted — a zero time asks the daemon for everything, which for a
	// container that has just started is a handful of lines.
	//
	// One that predates the collector is pre-existing, and replaying its
	// history would re-send however many days of logs it has accumulated the
	// first time container collection is switched on. That is the same rule the
	// file tailer applies to a file it has never seen.
	if ct.Created > 0 && time.Unix(ct.Created, 0).UTC().After(c.started) {
		return time.Time{}
	}
	return c.started
}

// dockerCursorID namespaces a container's resume cursor in the offset registry.
//
// The registry is keyed by device:inode for files, so a prefix that cannot
// occur in that form keeps the two kinds of entry from ever colliding. Reusing
// the registry rather than adding a second state file is what gives this path
// the same crash-safe, atomically-renamed persistence the file tailer has, and
// the same at-least-once commit-on-delivery behaviour: the timestamp rides on
// the envelope and is committed downstream by CommitTailOffset.
func dockerCursorID(id string) string { return "docker:" + id }

func (c *DockerLogStreamCollector) emit(send func(Envelope), ct dockerContainer, l dockerStreamLine) {
	labels := containerLabels(ct)
	if l.Stream != "" {
		labels["stream"] = l.Stream
	}

	at := l.At
	if at.IsZero() {
		// The daemon's timestamp prefix was missing or unparseable. The line is
		// still delivered — losing a log line to a formatting detail would be
		// worse — but it carries no cursor: committing "now" would advance the
		// resume point past lines that have not been delivered yet, turning a
		// duplicate into a permanent gap.
		send(Envelope{
			Kind:      KindLog,
			AgentID:   c.agentID,
			Source:    "container/" + ct.Name(),
			Timestamp: time.Now().UTC(),
			Message:   l.Text,
			Labels:    labels,
		})
		return
	}

	labels[LabelTailID] = dockerCursorID(ct.ID)
	labels[LabelTailEnd] = strconv.FormatInt(at.UnixNano(), 10)

	send(Envelope{
		Kind:      KindLog,
		AgentID:   c.agentID,
		Source:    "container/" + ct.Name(),
		Timestamp: at,
		Message:   l.Text,
		Labels:    labels,
	})
}

// excludedByName reports whether a container's name matches any exclusion.
// Shared with DockerLogCollector so the two log paths cannot drift on which
// containers they leave out.
func excludedByName(ct dockerContainer, exclude []string) bool {
	name := ct.Name()
	for _, pat := range exclude {
		if pat != "" && strings.Contains(name, pat) {
			return true
		}
	}
	return false
}
