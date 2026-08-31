package hostinfo

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// writeRoot builds a fake system root so the parsing can be tested against
// captured os-release files from real distributions rather than against
// whatever the machine running the tests happens to be.
func writeRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// Verbatim from Ubuntu 24.04. Kept exact — the quoting is inconsistent between
// fields in the real file and that inconsistency is what the parser has to
// survive.
const ubuntu2404 = `PRETTY_NAME="Ubuntu 24.04.1 LTS"
NAME="Ubuntu"
VERSION_ID="24.04"
VERSION="24.04.1 LTS (Noble Numbat)"
VERSION_CODENAME=noble
ID=ubuntu
ID_LIKE=debian
HOME_URL="https://www.ubuntu.com/"
UBUNTU_CODENAME=noble
`

// Verbatim from Amazon Linux 2023, which quotes a different subset.
const amazonLinux2023 = `NAME="Amazon Linux"
VERSION="2023"
ID="amzn"
ID_LIKE="fedora"
VERSION_ID="2023"
PLATFORM_ID="platform:al8"
PRETTY_NAME="Amazon Linux 2023"
ANSI_COLOR="0;33"
HOME_URL="https://aws.amazon.com/linux/amazon-linux-2023/"
`

func TestDetect_Ubuntu(t *testing.T) {
	root := writeRoot(t, map[string]string{
		"etc/os-release":            ubuntu2404,
		"proc/sys/kernel/osrelease": "6.8.0-1017-aws\n",
	})
	got := detectUnder(root)

	if got.Name != "Ubuntu" {
		t.Errorf("Name = %q, want Ubuntu", got.Name)
	}
	if got.Version != "24.04" {
		t.Errorf("Version = %q, want 24.04 (VERSION_ID, not the prose VERSION)", got.Version)
	}
	if got.KernelVersion != "6.8.0-1017-aws" {
		t.Errorf("KernelVersion = %q", got.KernelVersion)
	}
	want := "Ubuntu 24.04.1 LTS (Linux 6.8.0-1017-aws)"
	if got.Description != want {
		t.Errorf("Description = %q, want %q", got.Description, want)
	}
}

func TestDetect_AmazonLinux(t *testing.T) {
	root := writeRoot(t, map[string]string{
		"etc/os-release":            amazonLinux2023,
		"proc/sys/kernel/osrelease": "6.1.112-122.189.amzn2023.x86_64\n",
	})
	got := detectUnder(root)

	if got.Name != "Amazon Linux" {
		t.Errorf("Name = %q, want Amazon Linux", got.Name)
	}
	if got.Version != "2023" {
		t.Errorf("Version = %q, want 2023", got.Version)
	}
	if got.Description != "Amazon Linux 2023 (Linux 6.1.112-122.189.amzn2023.x86_64)" {
		t.Errorf("Description = %q", got.Description)
	}
}

// The two distributions above must not come out looking the same, which is the
// entire reason this package replaced a constant.
func TestDetect_DistinguishesDistributions(t *testing.T) {
	a := detectUnder(writeRoot(t, map[string]string{"etc/os-release": ubuntu2404}))
	b := detectUnder(writeRoot(t, map[string]string{"etc/os-release": amazonLinux2023}))
	if a.Name == b.Name || a.Description == b.Description {
		t.Fatalf("two different distributions reported identically: %+v vs %+v", a, b)
	}
}

// A scratch or distroless container has no os-release at all. The agent still
// runs there and must still report what it does know.
func TestDetect_NoOSRelease(t *testing.T) {
	root := writeRoot(t, map[string]string{"proc/sys/kernel/osrelease": "5.15.0\n"})
	got := detectUnder(root)

	if got.Name != "" || got.Version != "" {
		t.Errorf("invented a distribution from nothing: %+v", got)
	}
	if got.Type != runtime.GOOS {
		t.Errorf("Type = %q, want %q", got.Type, runtime.GOOS)
	}
	if got.KernelVersion != "5.15.0" {
		t.Errorf("KernelVersion = %q, want 5.15.0", got.KernelVersion)
	}
	if got.Description != "Linux 5.15.0" {
		t.Errorf("Description = %q, want the kernel alone", got.Description)
	}
}

// Nothing readable at all: still no invention, and no crash.
func TestDetect_EmptyRoot(t *testing.T) {
	got := detectUnder(writeRoot(t, nil))
	if got.Name != "" || got.Version != "" || got.KernelVersion != "" || got.Description != "" {
		t.Fatalf("reported something on a system that said nothing: %+v", got)
	}
	if got.Type == "" || got.Arch == "" {
		t.Fatal("type and arch are known from the build and must always be set")
	}
}

// Some minimal systems ship it only under /usr/lib.
func TestDetect_FallsBackToUsrLib(t *testing.T) {
	root := writeRoot(t, map[string]string{"usr/lib/os-release": ubuntu2404})
	if got := detectUnder(root); got.Name != "Ubuntu" {
		t.Fatalf("Name = %q, want Ubuntu from /usr/lib/os-release", got.Name)
	}
}

// /etc wins when both exist, matching os-release(5).
func TestDetect_EtcWinsOverUsrLib(t *testing.T) {
	root := writeRoot(t, map[string]string{
		"etc/os-release":     ubuntu2404,
		"usr/lib/os-release": amazonLinux2023,
	})
	if got := detectUnder(root); got.Name != "Ubuntu" {
		t.Fatalf("Name = %q, want the /etc copy to win", got.Name)
	}
}

func TestParseOSRelease_Quoting(t *testing.T) {
	root := writeRoot(t, map[string]string{
		"etc/os-release": "NAME=\"Double\"\nVERSION_ID='single'\nID=bare\n# a comment\n\nMALFORMED\n",
	})
	rel := parseOSRelease(filepath.Join(root, "etc", "os-release"))

	if rel["NAME"] != "Double" {
		t.Errorf("double quotes not stripped: %q", rel["NAME"])
	}
	if rel["VERSION_ID"] != "single" {
		t.Errorf("single quotes not stripped: %q", rel["VERSION_ID"])
	}
	if rel["ID"] != "bare" {
		t.Errorf("unquoted value mangled: %q", rel["ID"])
	}
	if _, ok := rel["MALFORMED"]; ok {
		t.Error("a line with no '=' became a key")
	}
	if len(rel) != 3 {
		t.Errorf("parsed %d keys, want 3: %v", len(rel), rel)
	}
}

// ID and VERSION are the fallbacks when the preferred keys are absent, since a
// partial file is more common than a complete one on trimmed-down images.
func TestParseOSRelease_FallbackKeys(t *testing.T) {
	root := writeRoot(t, map[string]string{"etc/os-release": "ID=alpine\nVERSION=3.20\n"})
	got := detectUnder(root)
	if got.Name != "alpine" {
		t.Errorf("Name = %q, want the ID as fallback", got.Name)
	}
	if got.Version != "3.20" {
		t.Errorf("Version = %q, want VERSION as fallback", got.Version)
	}
}

func TestResourceAttributes_OmitsWhatWasNotFound(t *testing.T) {
	attrs := OS{Type: "linux", Arch: "arm64"}.ResourceAttributes()

	if attrs["os.type"] != "linux" || attrs["host.arch"] != "arm64" {
		t.Fatalf("known values missing: %v", attrs)
	}
	// Present-but-empty would be worse than absent: a backend cannot tell a
	// host with no name from a host named "" and would render one as the other.
	for _, k := range []string{"os.name", "os.version", "os.description"} {
		if _, ok := attrs[k]; ok {
			t.Errorf("%s present for an undetermined value", k)
		}
	}
}

func TestResourceAttributes_FullSet(t *testing.T) {
	root := writeRoot(t, map[string]string{
		"etc/os-release":            ubuntu2404,
		"proc/sys/kernel/osrelease": "6.8.0-1017-aws\n",
	})
	attrs := detectUnder(root).ResourceAttributes()
	for _, k := range []string{"os.type", "os.name", "os.version", "os.description", "host.arch"} {
		if attrs[k] == "" {
			t.Errorf("%s is empty on a fully described host: %v", k, attrs)
		}
	}
}
