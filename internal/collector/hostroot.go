package collector

import (
	"os"
	"path"
	"strings"
)

// HostRootEnv names the environment variable that tells the agent where the
// host's own filesystem is mounted.
//
// Every reader in this package addresses /proc and /sys by absolute path,
// which is correct on a host and wrong in a container: there, /proc describes
// the agent's own namespace. An agent that reads its own /proc/meminfo inside
// a container reports the container's view of memory and calls it the host's,
// which is not a degraded answer but a confidently wrong one.
//
// The fix is the same one Datadog uses (HOST_PROC / DD_PROCFS_PATH): bind-mount
// the host's /proc and /sys somewhere the container can see them, and prefix
// every read. Set it to the mount point:
//
//	docker run -e AGENT_I_HOST_ROOT=/host \
//	  -v /proc:/host/proc:ro -v /sys:/host/sys:ro ...
//
// An environment variable rather than a config key, deliberately. The knob is
// a property of how the process was launched, not of what the operator wants
// collected, and it has to be settable by `docker run -e` on an image whose
// config file is baked in — the same reasoning that put the API token in the
// environment. Unset means "running on a host", which is the existing
// behaviour and stays the default.
const HostRootEnv = "AGENT_I_HOST_ROOT"

// hostRoot is resolved once at process start. It is read-only from then on, so
// the many collectors that consult it need no synchronisation — consistent
// with this package's rule that mutable state belongs to one goroutine.
var hostRoot = normalizeHostRoot(os.Getenv(HostRootEnv))

// normalizeHostRoot treats "" and "/" alike as "no prefix". A container that
// bind-mounts the host root at / has not moved anything, and joining "/" onto
// every path would be a no-op with an allocation.
func normalizeHostRoot(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || v == "/" {
		return ""
	}
	return path.Clean(v)
}

// hostPath rewrites an absolute path on the host into one reachable from this
// process. On a host it returns p unchanged.
//
// path.Join rather than filepath.Join because these are always slash-separated
// Linux paths, and the result must not depend on the OS the tests happen to
// run under.
func hostPath(p string) string {
	if hostRoot == "" {
		return p
	}
	return path.Join(hostRoot, p)
}

// HostRoot reports the configured prefix, empty when the agent is reading its
// own machine directly. Exported for the daemon's startup log: "which /proc am
// I reading" is the first question when container metrics look wrong, and it
// should not require attaching a debugger to answer.
func HostRoot() string { return hostRoot }

// SelfContainerID returns the id of the container this agent is running in, or
// "" when it is running directly on a host.
//
// Exported because the daemon needs it for something other than collection: an
// agent in a container names itself after the container unless told otherwise,
// and that name changes on every recreate. Knowing we are in a container is
// what lets that be reported rather than silently producing a new host id every
// restart.
//
// Deliberately not derived from AGENT_I_HOST_ROOT. That says where the host's
// files are mounted; this asks whether this process is containerised, and the
// two are independent — an agent can be given the mounts and still be running
// on the host, and vice versa.
func SelfContainerID() string { return readSelfContainerID() }
