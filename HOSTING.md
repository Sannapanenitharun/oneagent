# Hosting Agent-I releases (so the one-line installer works)

`scripts/get.sh` is the real `curl | bash` one-liner — it detects the
host's OS/architecture, downloads a prebuilt binary, verifies its
checksum, and installs everything. It needs the binaries published
somewhere public first. Two ways to do that:

## Option A — GitHub Releases (recommended, fully automated)

This repo already includes `.github/workflows/release.yml`, tested logic
(cross-compilation confirmed for linux/amd64 and linux/arm64 in this
session — see the build step earlier in this conversation). Once this
code is pushed to a GitHub repo:

```bash
git tag v1.0.0
git push --tags
```

That single push triggers the workflow, which builds both architectures,
generates `checksums.txt`, and publishes them as a GitHub Release —
automatically attaching `scripts/get.sh` itself as a release asset too.

Then the real one-liner for anyone to run is:

```bash
curl -fsSL https://github.com/Sannapanenitharun/oneagent/releases/latest/download/get.sh \
  | sudo bash
```

(The repo is already baked in as the default in `get.sh`, so no `AGENT_I_REPO` env var is needed unless you fork it elsewhere.)

## Option B — Your own hosting (S3, a static file server, etc.)

`get.sh` supports `AGENT_I_BASE_URL` to bypass the GitHub Releases URL
pattern entirely — point it at any URL that serves the same three files
GitHub Releases would:

- `agent-i_linux_amd64.tar.gz`
- `agent-i_linux_arm64.tar.gz`
- `checksums.txt` (sha256sum output format — `<hash>  <filename>` per line)

Build and publish them yourself:

```bash
for arch in amd64 arm64; do
  mkdir agent-i_linux_$arch
  GOOS=linux GOARCH=$arch CGO_ENABLED=0 go build -o agent-i_linux_$arch/agent-i ./cmd/agent
  cp configs/agent.yaml systemd/agent-i.service agent-i_linux_$arch/
  tar -czf agent-i_linux_$arch.tar.gz agent-i_linux_$arch
done
sha256sum *.tar.gz > checksums.txt
# upload the .tar.gz files + checksums.txt to your host
```

Then the one-liner becomes:

```bash
curl -fsSL https://your-host.example.com/get.sh \
  | AGENT_I_BASE_URL=https://your-host.example.com sudo -E bash
```

## Verification performed in this session (and what wasn't)

Confirmed, with real commands and output:
- Cross-compilation for both target architectures succeeds
- `get.sh`'s full flow — arch detection, download, checksum
  verification, user creation, binary/config install — works end to end
  against a local mock server standing in for GitHub Releases
- The installed binary runs correctly against the installed config

Not verified (environment limitation, not a script issue):
- The `systemctl enable --now` step — this sandbox's container has no
  systemd as PID 1, so that line couldn't be exercised here. It's
  standard systemd usage and matches the (already-tested-elsewhere)
  pattern in `scripts/install.sh`; worth a real VM/server smoke test
  before you rely on it unattended.
- The actual GitHub Releases upload/download path — untested because
  this sandbox has no credentials to push to a GitHub repo and no
  network path to fetch from real Releases URLs. The logic mirrors what
  was tested against the local mock server exactly (same script, same
  URL shape via `AGENT_I_BASE_URL`), but that's inference, not a
  live-tested claim.
