package collector

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"time"
)

// LogTailCollector follows a set of file globs and emits each new line as
// an Envelope. Uses simple poll-based tailing (seek to end, read on
// interval) rather than inotify: less efficient per-file, but has zero
// platform-specific syscall dependencies and degrades predictably if a
// file is rotated out from under it — correctness over throughput for v1.
type LogTailCollector struct {
	agentID string
	globs   []string
	stop    chan struct{}
}

func NewLogTailCollector(agentID string, globs []string) *LogTailCollector {
	return &LogTailCollector{agentID: agentID, globs: globs, stop: make(chan struct{})}
}

func (l *LogTailCollector) Name() string { return "log.tail" }

func (l *LogTailCollector) Start(ctx context.Context, out chan<- Envelope) error {
	paths, err := l.resolvePaths()
	if err != nil {
		return err
	}
	for _, p := range paths {
		go l.tailFile(ctx, p, out)
	}
	return nil
}

func (l *LogTailCollector) Stop() error {
	close(l.stop)
	return nil
}

func (l *LogTailCollector) resolvePaths() ([]string, error) {
	var matched []string
	for _, g := range l.globs {
		found, err := filepath.Glob(g)
		if err != nil {
			return nil, err
		}
		matched = append(matched, found...)
	}
	return matched, nil
}

func (l *LogTailCollector) tailFile(ctx context.Context, path string, out chan<- Envelope) {
	f, err := os.Open(path)
	if err != nil {
		return // missing file at startup is non-fatal — other sources keep running
	}
	defer f.Close()

	// Start at end of file: agent reports new activity going forward,
	// not a full historical replay (would flood the exporter on first run).
	if _, err := f.Seek(0, 2); err != nil {
		return
	}

	reader := bufio.NewReader(f)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-l.stop:
			return
		case <-ticker.C:
			for {
				line, err := reader.ReadString('\n')
				if line != "" {
					out <- Envelope{
						Kind:      KindLog,
						AgentID:   l.agentID,
						Source:    path,
						Timestamp: time.Now().UTC(),
						Message:   line,
					}
				}
				if err != nil {
					break // caught up to EOF; wait for next tick
				}
			}
		}
	}
}
