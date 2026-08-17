package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// This file answers "give me API calls into this host" — not by packet
// capture or code instrumentation (both much heavier asks: packet capture
// raises its own scope/security questions, and instrumentation requires
// touching every app's code), but by parsing the access log the web
// server or app is most likely already writing. That covers the common
// case — nginx, Apache, and most app frameworks' request logs — without
// requiring any change to the monitored application.

type AccessLogFormat string

const (
	FormatCombined AccessLogFormat = "combined" // nginx/Apache combined log format, with an optional trailing $request_time field
	FormatJSON     AccessLogFormat = "json"     // one JSON object per line
)

// JSONFieldMap lets a JSON-format access log use whatever field names the
// app actually emits, rather than forcing one fixed schema.
type JSONFieldMap struct {
	Method     string // default "method"
	Path       string // default "path"
	Status     string // default "status"
	DurationMs string // default "duration_ms"
	RemoteAddr string // default "remote_addr"
}

func (m JSONFieldMap) withDefaults() JSONFieldMap {
	if m.Method == "" {
		m.Method = "method"
	}
	if m.Path == "" {
		m.Path = "path"
	}
	if m.Status == "" {
		m.Status = "status"
	}
	if m.DurationMs == "" {
		m.DurationMs = "duration_ms"
	}
	if m.RemoteAddr == "" {
		m.RemoteAddr = "remote_addr"
	}
	return m
}

// apiCallEvent is the parsed, format-agnostic shape both parsers produce.
type apiCallEvent struct {
	method     string
	path       string
	status     int
	durationMs float64 // 0 if the log line didn't carry timing
	remoteAddr string
	timestamp  time.Time
}

// combinedLogPattern matches the standard Apache/nginx "combined" format,
// with an optional trailing request-time field — the single most common
// real-world extension (nginx: log_format combined_with_time '... $request_time';).
// Example line this matches:
//
//	127.0.0.1 - - [12/Aug/2026:04:05:06 +0000] "GET /api/users HTTP/1.1" 200 1234 "-" "curl/8.0" 0.042
var combinedLogPattern = regexp.MustCompile(
	`^(\S+) \S+ \S+ \[([^\]]+)\] "(\S+) (\S+) [^"]*" (\d{3}) (?:\d+|-) "[^"]*" "[^"]*"(?:\s+([\d.]+))?`,
)

// combinedTimeLayout matches nginx/Apache's default $time_local format.
const combinedTimeLayout = "02/Jan/2006:15:04:05 -0700"

func parseCombinedLine(line string) (*apiCallEvent, error) {
	m := combinedLogPattern.FindStringSubmatch(line)
	if m == nil {
		return nil, fmt.Errorf("line does not match combined log format")
	}

	status, err := strconv.Atoi(m[5])
	if err != nil {
		return nil, fmt.Errorf("parsing status code: %w", err)
	}

	ts, err := time.Parse(combinedTimeLayout, m[2])
	if err != nil {
		ts = time.Now().UTC() // malformed timestamp shouldn't drop an otherwise-valid request
	}

	var durationMs float64
	if m[6] != "" {
		if secs, err := strconv.ParseFloat(m[6], 64); err == nil {
			durationMs = secs * 1000
		}
	}

	return &apiCallEvent{
		method:     m[3],
		path:       m[4],
		status:     status,
		durationMs: durationMs,
		remoteAddr: m[1],
		timestamp:  ts,
	}, nil
}

func parseJSONLine(line string, fields JSONFieldMap) (*apiCallEvent, error) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return nil, fmt.Errorf("parsing JSON access log line: %w", err)
	}

	method, _ := raw[fields.Method].(string)
	path, _ := raw[fields.Path].(string)
	remoteAddr, _ := raw[fields.RemoteAddr].(string)
	if method == "" || path == "" {
		return nil, fmt.Errorf("JSON line missing required fields %q/%q", fields.Method, fields.Path)
	}

	status := 0
	switch v := raw[fields.Status].(type) {
	case float64:
		status = int(v)
	case string:
		status, _ = strconv.Atoi(v)
	}

	var durationMs float64
	switch v := raw[fields.DurationMs].(type) {
	case float64:
		durationMs = v
	case string:
		durationMs, _ = strconv.ParseFloat(v, 64)
	}

	return &apiCallEvent{
		method:     strings.ToUpper(method),
		path:       path,
		status:     status,
		durationMs: durationMs,
		remoteAddr: remoteAddr,
		timestamp:  time.Now().UTC(), // JSON access logs vary too much in timestamp field naming/format to guess reliably; wall-clock-at-parse-time is close enough since this is a near-real-time tailer
	}, nil
}

// AccessLogCollector tails access log files and emits a KindAPICall envelope
// per successfully-parsed request. Tailing itself — rotation, offsets,
// partial lines, files appearing after startup — is handled by the shared
// tailer in tail.go; everything specific to access logs is the parsing above.
type AccessLogCollector struct {
	agentID string
	format  AccessLogFormat
	fields  JSONFieldMap
	mgr     *tailManager
}

func NewAccessLogCollector(agentID string, globs []string, format AccessLogFormat, fields JSONFieldMap, opts TailingOptions) *AccessLogCollector {
	c := &AccessLogCollector{
		agentID: agentID,
		format:  format,
		fields:  fields.withDefaults(),
	}
	c.mgr = newTailManager(tailOptions{
		globs:        globs,
		scanInterval: opts.ScanInterval,
		pollInterval: opts.PollInterval,
		maxLineBytes: opts.MaxLineBytes,
		registry:     opts.Registry,
	})
	return c
}

func (a *AccessLogCollector) Name() string { return "http.access_log" }

func (a *AccessLogCollector) Start(ctx context.Context, out chan<- Envelope) error {
	a.mgr.opts.handle = func(ln tailLine) {
		env, ok := a.parse(ln.Path, ln.Line)
		if !ok {
			// Unparseable lines commit nothing. They need no special handling:
			// the next line that does parse carries an offset past them, so the
			// skipped bytes are covered without being separately tracked.
			return
		}
		env.Labels = withTailProvenance(env.Labels, ln)
		// Deliberately blocking, like the log tailer: this reads from a file
		// with a persisted read offset, so slowing down means lines arrive
		// later, not that they are lost. A bounded drop here would discard a
		// request record permanently in exchange for nothing.
		select {
		case out <- env:
		case <-ctx.Done():
		}
	}
	a.mgr.Start(ctx)
	return nil
}

// SetPaths replaces the watched globs on config reload.
func (a *AccessLogCollector) SetPaths(globs []string) { a.mgr.UpdateGlobs(globs) }

func (a *AccessLogCollector) Stop() error {
	a.mgr.Stop()
	return nil
}

// parse turns one access log line into an Envelope. A line that does not parse
// is dropped: one malformed entry should not stop the tailer. Note that with
// the shared tailer a "malformed" line is now genuinely malformed — previously
// this also silently swallowed the two half-lines produced every time a write
// was caught mid-line.
func (a *AccessLogCollector) parse(path, line string) (Envelope, bool) {
	var (
		event *apiCallEvent
		err   error
	)
	switch a.format {
	case FormatJSON:
		event, err = parseJSONLine(line, a.fields)
	default:
		event, err = parseCombinedLine(line)
	}
	if err != nil {
		return Envelope{}, false
	}

	labels := map[string]string{
		"method": event.method,
		"path":   event.path,
		"status": strconv.Itoa(event.status),
	}
	if event.remoteAddr != "" {
		labels["remote_addr"] = event.remoteAddr
	}

	return Envelope{
		Kind:      KindAPICall,
		AgentID:   a.agentID,
		Source:    "http.access_log:" + path,
		Timestamp: event.timestamp,
		Labels:    labels,
		Value:     event.durationMs,
	}, true
}
