package collector

import (
	"context"
)

// LogTailCollector follows a set of file globs and emits each new record as an
// Envelope. The actual tailing — rotation detection, offset persistence,
// partial-line handling, picking up files that appear after startup — lives in
// tail.go and is shared with the access log collector; this type is only the
// adapter from "a record of text" to our Envelope shape.
//
// A record is one physical line unless multiline assembly is configured, in
// which case it may be several — see multiline.go.
type LogTailCollector struct {
	agentID string
	mgr     *tailManager
	ml      *multilineAssembler
}

// NewLogTailCollector builds the collector. A multiline configuration that does
// not compile is returned as an error rather than being ignored: a start
// pattern with a typo in it would otherwise look exactly like a working
// configuration that simply never matched.
func NewLogTailCollector(agentID string, globs []string, opts TailingOptions, ml MultilineOptions, multilineEnabled bool) (*LogTailCollector, error) {
	c := &LogTailCollector{agentID: agentID}

	topts := tailOptions{
		globs:        globs,
		scanInterval: opts.ScanInterval,
		pollInterval: opts.PollInterval,
		maxLineBytes: opts.MaxLineBytes,
		registry:     opts.Registry,
	}

	if multilineEnabled {
		a, err := newMultilineAssembler(ml, nil) // emit is wired in Start
		if err != nil {
			return nil, err
		}
		c.ml = a
		// Both hooks run on the tail manager's goroutine, which is the same one
		// that delivers lines — so the assembler is single-threaded and needs no
		// lock, in keeping with how the rest of this package works.
		topts.onTick = a.flushIdle
		topts.onFileClosed = a.forget
	}

	c.mgr = newTailManager(topts)
	return c, nil
}

func (l *LogTailCollector) Name() string { return "log.tail" }

func (l *LogTailCollector) Start(ctx context.Context, out chan<- Envelope) error {
	send := func(ln tailLine) {
		// Blocking here is deliberate back-pressure: if the exporter is behind,
		// slowing the tailer is better than dropping lines. ctx keeps it
		// interruptible so shutdown is never held up by a stalled backend.
		select {
		case out <- Envelope{
			Kind:      KindLog,
			AgentID:   l.agentID,
			Source:    ln.Path,
			Timestamp: ln.At,
			Message:   ln.Line,
			Labels:    tailProvenanceLabels(ln),
		}:
		case <-ctx.Done():
		}
	}

	if l.ml != nil {
		l.ml.emit = send
		l.mgr.opts.handle = l.ml.handle
	} else {
		l.mgr.opts.handle = send
	}

	l.mgr.Start(ctx)
	return nil
}

// SetPaths replaces the watched globs on config reload.
func (l *LogTailCollector) SetPaths(globs []string) { l.mgr.UpdateGlobs(globs) }

func (l *LogTailCollector) Stop() error {
	l.mgr.Stop()
	// After the manager's goroutine has finished, nothing else touches the
	// assembler — so emitting whatever is still open is safe here, and means a
	// record in progress at shutdown is reported rather than dropped.
	if l.ml != nil {
		l.ml.flushAll()
	}
	return nil
}
