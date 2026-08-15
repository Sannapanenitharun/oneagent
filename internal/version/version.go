// Package version carries the agent's build identity.
//
// This exists because of a real diagnostic dead end: a host was reporting
// only a subset of the metrics its code emitted, and there was no way to
// tell whether the binary on that host predated the collectors in
// question. Answering "which build is this?" required reading the metric
// output and inferring backwards. The binary now states it, both on the
// command line and in the telemetry it ships.
package version

// Version is the build's identity, stamped at link time:
//
//	go build -ldflags "-X github.com/agent-i/agent/internal/version.Version=$(git describe --tags --always --dirty)"
//
// The default is deliberately "dev" rather than a plausible-looking
// number — an unstamped binary should be obviously unstamped, not
// mistakable for a release. scripts/install.sh stamps it from the git
// checkout it builds from, so a --dirty suffix is a real signal that the
// deployed binary does not correspond to any commit.
var Version = "dev"
