package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// This is a deliberately minimal Docker Engine API client, written against the
// documented HTTP contract with nothing but the standard library.
//
// Not using github.com/docker/docker/client is a considered trade, not an
// oversight. That module pulls in a large transitive tree — containerd, gRPC,
// OpenTelemetry's own SDK, several logging libraries — for a binary whose whole
// premise is that it is a single static file you can scp onto a host and run.
// What the agent needs from the daemon is one endpoint returning one list, and
// that is thirty lines of net/http.
//
// The client is also used for exactly one thing: names. Every number comes from
// the cgroup files (see cgroup2.go). That split is what lets container metrics
// keep working when the socket is not mounted, and is why the socket is
// optional rather than required.

// DefaultDockerEndpoint is where the Engine listens on a standard Linux install.
const DefaultDockerEndpoint = "/var/run/docker.sock"

// dockerAPITimeout bounds a single request to the daemon. The socket is local,
// so a healthy answer takes single-digit milliseconds; this is sized for a
// daemon that is wedged rather than one that is busy. It has to stay well under
// the collection interval, because a hung daemon must not stall the sample —
// container metadata going stale is a much smaller problem than metrics
// stopping.
const dockerAPITimeout = 3 * time.Second

// dockerContainer is the subset of the Engine's container listing the agent
// uses. Decoding into a narrow struct rather than a map keeps the field names
// checked at compile time and means a future API version adding fields costs
// nothing.
type dockerContainer struct {
	ID    string   `json:"Id"`
	Names []string `json:"Names"`
	Image string   `json:"Image"`
	State string   `json:"State"`
	// Created is unix seconds. It is what the collector reports as the start
	// time of this container's cumulative counters — strictly it is creation
	// rather than start, but for a running container the two differ by the
	// startup time of one process, and the alternative is omitting the start
	// time entirely. Zero when the container was discovered from cgroups
	// rather than from the daemon.
	Created int64             `json:"Created"`
	Labels  map[string]string `json:"Labels"`
}

// Name returns the container's display name without the Engine's leading slash.
//
// A container can have several names when it is aliased on a network; the first
// is the one docker ps shows and the one an operator will recognise. Falling
// back to the short id means a metric always carries something identifying,
// even for a container started without a name.
func (c dockerContainer) Name() string {
	for _, n := range c.Names {
		if n = strings.TrimPrefix(n, "/"); n != "" {
			return n
		}
	}
	return shortID(c.ID)
}

// dockerClient talks to the Engine over its unix socket.
type dockerClient struct {
	http     *http.Client
	endpoint string
}

// newDockerClient builds a client for a unix socket path.
//
// The URL host is a placeholder: the transport dials the socket regardless of
// what it says, but net/http still requires a syntactically valid URL, and
// "unix" reads better in an error message than the invented hostnames some
// clients use.
func newDockerClient(endpoint string) *dockerClient {
	if endpoint == "" {
		endpoint = DefaultDockerEndpoint
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", endpoint)
		},
		// One idle connection is the right number for a caller that makes a
		// single request every collection interval. Pooling more would hold
		// file descriptors open against a daemon we are barely talking to.
		MaxIdleConns:    1,
		IdleConnTimeout: 90 * time.Second,
	}
	return &dockerClient{
		http:     &http.Client{Transport: tr, Timeout: dockerAPITimeout},
		endpoint: endpoint,
	}
}

// Available reports whether the socket exists and is connectable.
//
// Checked separately from the first real call so the daemon can say "the socket
// is not mounted" once at startup, rather than the collector logging a failed
// request every interval for the life of a process that was simply never given
// the mount.
func (c *dockerClient) Available(ctx context.Context) error {
	if _, err := os.Stat(c.endpoint); err != nil {
		return fmt.Errorf("docker socket %s: %w", c.endpoint, err)
	}
	ctx, cancel := context.WithTimeout(ctx, dockerAPITimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.endpoint)
	if err != nil {
		return fmt.Errorf("docker socket %s: %w", c.endpoint, err)
	}
	return conn.Close()
}

// Containers lists the running containers.
//
// The path carries no API version. Docker treats an unversioned request as
// "use the daemon's current version", which is what we want: pinning a version
// means a daemon older than the pin rejects the call outright, and the fields
// read here — Id, Names, Image, Labels — have been stable since v1.24 in 2016.
func (c *dockerClient) Containers(ctx context.Context) ([]dockerContainer, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix/containers/json", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Bounded read: an error body from a misbehaving proxy in front of the
		// socket could otherwise be arbitrarily large.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("listing containers: docker returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out []dockerContainer
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding container list: %w", err)
	}
	return out, nil
}

// Close releases pooled connections.
func (c *dockerClient) Close() {
	c.http.CloseIdleConnections()
}
