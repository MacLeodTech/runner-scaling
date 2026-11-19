# CI runner images

Prebuilt images for the pools, so a job does not download or compile its tools on every run. Separate from the
scaler/guard image: own Dockerfile, own build, own changelog line. **DRAFT: nothing here is wired into a pool yet.**

| target | adds | used by |
|---|---|---|
| `base` | pinned runner image, jq, python3 (venv, pip), build-essential, gosec, gitleaks, govulncheck | (parent) |
| `lite` | Go, Node (two lines), JDK 17 and 21, Maven, Rust, Playwright Chromium and its system libraries | `homelab-lite` |
| `sysbox` | same as `lite` today; its own tag so the pools can diverge | `homelab-sysbox` |

Tool versions and checksums come from a versions file the consuming repo owns (format: `tool-versions.example`), so
this directory stays generic and the image cannot drift from the repo's workflow toolchain. Every download is
verified against a sha256/sha512 from that file; the one exception is Playwright's browser download, which publishes
no checksums. govulncheck is compiled at build time, verified by Go's checksum database.

```
images/build.sh --versions ~/projects/Assay/ci/tool-versions            # lite and sysbox
images/check.sh --versions ~/projects/Assay/ci/tool-versions guard-lite.yaml guard-sysbox.yaml
```

Tags: `runner-ci:<target>-<date>-<hash>` (hash of the versions file and Dockerfile) and the moving `runner-ci:<target>`.
Local builds only; publishing to ghcr comes later. Expected size: `lite` and `sysbox` both about 4.9 GB on disk (the 2.4 GB runner base is shared with what runs today; the two images are identical, so they share every layer).

## Rules

- Tools live under `/opt/ci`, symlinked into `/usr/local/bin`. Never bake anything under `/opt/hostedtoolcache`: the
  guards bind-mount a per-slot directory over it and would hide it.
- Go and Node are laid out as an actions toolcache in `/opt/ci/toolcache`, and the guards set `RUNNER_TOOL_CACHE` to
  it (see `guard-image.diff`), so `actions/setup-go` and `setup-node` find them without downloading. Tested with the
  real pinned actions in a throwaway container: Go 1.26 (even with `check-latest: true`) resolves to the baked 1.26.x
  and Node 24 and 22 are found. Caveat: when a newer 1.26.x appears upstream, `check-latest` downloads it until the
  image is rebuilt. Python 3.12 (Ubuntu's own, as a self-contained `--copies` venv entry) is seeded the same way and
  was tested with the real `setup-python`: resolved from the cache, no download, `pip install` into a venv and
  `pip install -e` work. The guard must set `AGENT_TOOLSDIRECTORY` as well as `RUNNER_TOOL_CACHE`: the runner image
  sets the former to `/opt/hostedtoolcache` and `setup-python` honours it over the latter.
- The image build and its checks run on GitHub-hosted runners or by hand, never on these pools.

## Switching a pool (needs the owner's go-ahead: restarting a guard kills its running jobs)

In `guard-lite.yaml` and `guard-sysbox.yaml`, `runner.image` changes from the pinned myoung34 digest to
`runner-ci:lite-<date>-<hash>` / `runner-ci:sysbox-<date>-<hash>`; then `autoscale.sh check` and restart that guard.
See `guard-image.diff` for the exact lines.

## Open questions

- A locally built image has no registry digest, so it cannot be pinned by digest until it is pushed. The guard must
  accept a plain tag for now (verify with `autoscale.sh check`).
- The per-slot persistent toolcache bind is gone in the proposal; anything not baked in is downloaded per job.
- Chromium: sandboxed launch works under the lite caps and 3G (no `--no-sandbox`). `/dev/shm` is Docker's default 64M;
  Chrome falls back to /tmp, so a larger shm is optional (`shm_size`, if the guard supports it) not required.
- Jobs still need to stop installing what the image has (Assay's `toolchain` action already skips when `mvn`/`rustup`
  are on PATH).

## Jobs can still install their own tools

The tool dirs are writable in the container layer and the toolcache is not a read-only volume, so a job that asks for
something the image lacks installs it itself. Verified under the lite guard's limits (cap_drop ALL plus the guard's six
caps, 3G, no-new-privileges, root): `setup-go`, `setup-node` and `setup-python` download a version the image lacks into
`/opt/ci/toolcache`; `apt-get update && apt-get install` works (the lists are stripped to save space, so `update` is
required first); `pip install`, `npm install -g`, `cargo install` and curl into `~/.local/bin` work; tools a job adds to
`GITHUB_PATH` come first on the PATH and override the baked ones. `/opt/ci/cargo/bin` is on the image PATH.

## What was trimmed (and what was not)

Removed: JDK `jmods` (only for jlink), Go's `test`/`api`/`doc`/`misc` dirs, Node's docs, the full Chromium build
(`--only-shell`: Playwright's `headless: true` uses the headless shell; a job needing headed Chromium runs
`npx playwright install chromium` itself), apt lists, npm cache. Kept on purpose: both JDKs (17 and 21), both Node
lines, Node headers (`include/`, for native addons), Maven, Rust minimal profile, man pages and docs (about 10 MB,
license texts).
