// Package hostinfo describes the operating system the agent is running on.
//
// It exists because the agent previously asserted this rather than reading it.
// os.type was runtime.GOOS — a compile-time constant, so every binary built
// for Linux reported "linux" whether it was on Amazon Linux 2, Ubuntu 24.04 or
// a scratch container — and the fleet table simply printed the string "linux"
// in its OS column. Neither is a measurement; both are the build configuration
// wearing a measurement's clothes.
//
// The distinction matters as soon as there is more than one host. "Which of
// these is still on the old distro", "did the kernel patch land everywhere",
// "is this failure confined to the 22.04 boxes" are the ordinary questions of
// running a fleet, and none of them can be asked of a constant.
//
// Everything here degrades rather than fails. A container without
// /etc/os-release, a distro with a partial one, a kernel that does not expose
// osrelease: each simply contributes nothing, because an agent that refused to
// start over a missing description file would be trading all of its telemetry
// for one label.
package hostinfo

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// OS is what could be discovered about the operating system. Every field is
// optional and empty means "not determined" — never a guess.
type OS struct {
	// Type is the OTel os.type value: linux, windows, darwin. This one is
	// genuinely known at build time, since a native binary cannot run on
	// another kernel.
	Type string
	// Name is the distribution: "Ubuntu", "Amazon Linux", "Debian GNU/Linux".
	Name string
	// Version is the distribution version: "24.04", "2023".
	Version string
	// Description is the human-readable line, with the kernel appended when
	// known: "Ubuntu 24.04.1 LTS (Linux 6.8.0-1017-aws)".
	Description string
	// KernelVersion is the release string from uname -r.
	KernelVersion string
	// Arch is the OTel host.arch value: amd64, arm64.
	Arch string
}

// Detect reads the OS description from the running system.
func Detect() OS { return detectUnder("/") }

// detectUnder is Detect with a settable root so the parsing can be tested
// against captured /etc/os-release files from real distributions rather than
// against whatever the test machine happens to be running.
func detectUnder(root string) OS {
	o := OS{
		Type: runtime.GOOS,
		// GOARCH already spells the values OTel defines for host.arch —
		// amd64, arm64, 386 — so no translation table is needed.
		Arch: runtime.GOARCH,
	}

	rel := parseOSRelease(filepath.Join(root, "etc", "os-release"))
	if len(rel) == 0 {
		// Some distributions ship it only in /usr/lib, and a system with a
		// read-only or minimal /etc may have just the one copy.
		rel = parseOSRelease(filepath.Join(root, "usr", "lib", "os-release"))
	}

	o.Name = firstNonEmpty(rel["NAME"], rel["ID"])
	o.Version = firstNonEmpty(rel["VERSION_ID"], rel["VERSION"], rel["BUILD_ID"])
	o.KernelVersion = readKernel(root)

	// PRETTY_NAME is what the distribution calls itself in full and is worth
	// preferring over anything assembled here. The kernel is appended because
	// the two are read together far more often than either is read alone.
	pretty := firstNonEmpty(rel["PRETTY_NAME"], strings.TrimSpace(o.Name+" "+o.Version))
	switch {
	case pretty != "" && o.KernelVersion != "":
		o.Description = pretty + " (Linux " + o.KernelVersion + ")"
	case pretty != "":
		o.Description = pretty
	case o.KernelVersion != "":
		o.Description = "Linux " + o.KernelVersion
	}

	return o
}

// ResourceAttributes renders the discovery as OTel semantic-convention
// attributes, omitting anything that was not determined.
//
// Omission is deliberate and is the whole point of the package: a backend can
// tell "this agent could not read its os-release" from "this host is called
// linux" only if the absent case is genuinely absent, rather than filled in
// with a plausible default.
func (o OS) ResourceAttributes() map[string]string {
	attrs := make(map[string]string, 6)
	set := func(k, v string) {
		if v = strings.TrimSpace(v); v != "" {
			attrs[k] = v
		}
	}
	set("os.type", o.Type)
	set("os.name", o.Name)
	set("os.version", o.Version)
	set("os.description", o.Description)
	set("host.arch", o.Arch)
	return attrs
}

// parseOSRelease reads the shell-fragment format defined by os-release(5).
//
// The format is a restricted shell assignment: KEY=value, where the value may
// be unquoted, single-quoted or double-quoted. Real files in the wild mix all
// three — Ubuntu double-quotes PRETTY_NAME and leaves VERSION_ID quoted,
// Amazon Linux quotes some fields and not others — so the quoting is stripped
// rather than assumed.
func parseOSRelease(path string) map[string]string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	out := make(map[string]string, 12)
	sc := bufio.NewScanner(f)
	// A malformed or hostile file must not be read into memory without bound.
	sc.Buffer(make([]byte, 0, 4096), 64*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		out[key] = val
	}
	if err := sc.Err(); err != nil {
		// A partial read is still better than none: the fields already parsed
		// are valid, and the caller treats missing ones as undetermined.
		return out
	}
	return out
}

// readKernel returns the kernel release, the same string uname -r prints.
//
// Read from /proc rather than by shelling out to uname: the agent builds with
// CGO_ENABLED=0 and ships as a static binary onto hosts whose contents it
// cannot assume, so depending on an external command for a label would make
// this fail exactly on the minimal images where it is least verifiable.
func readKernel(root string) string {
	b, err := os.ReadFile(filepath.Join(root, "proc", "sys", "kernel", "osrelease"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
