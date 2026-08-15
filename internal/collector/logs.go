package collector

import (
	"context"
	"time"
)

// LogTailCollector follows a set of file globs and emits each new line as an
// Envelope. The actual tailing — rotation detection, offset persistence,
// partial-line handling, picking up files that appear after startup — lives in
// tail.go and is shared with the access log collector; this type is only the
// adapter from "a line of text" to our Envelope shape.
type LogTailCollector struct {
	agentID string
	mgr     *tailManager
}

func NewLogTailCollector(agentID string, globs []string, opts TailingOptions) *LogTailCollector {
	c := &LogTailCollector{agentID: agentID}
	c.mgr = newTailManager(tailOptions{
		globs:        globs,
		scanInterval: opts.ScanInterval,
		pollInterval: opts.PollInterval,
		maxLineBytes: opts.MaxLineBytes,
		registry:     opts.Registry,
	})
	return c
}

func (l *LogTailCollector) Name() string { return "log.tail" }

func (l *LogTailCollector) Start(ctx context.Context, out chan<- Envelope) error {
	l.mgr.opts.handle = func(path, line string, at time.Time) {
		// Blocking here is deliberate back-pressure: if the exporter is behind,
		// slowing the tailer is better than dropping lines. ctx keeps it
		// interruptible so shutdown is never held up by a stalled backend.
		select {
		case out <- Envelope{
			Kind:      KindLog,
			AgentID:   l.agentID,
			Source:    path,
			Timestamp: at,
			Message:   line,
		}:
		case <-ctx.Done():
		}
	}
	l.mgr.Start(ctx)
	return nil
}

// SetPaths replaces the watched globs on config reload.
func (l *LogTailCollector) SetPaths(globs []string) { l.mgr.UpdateGlobs(globs) }

func (l *LogTailCollector) Stop() error {
	l.mgr.Stop()
	return nil
}
