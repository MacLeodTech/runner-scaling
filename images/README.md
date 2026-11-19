# CI runner images

Prebuilt images for the pools, so a job does not download or compile its tools on every run. Separate from the
scaler/guard image: own Dockerfile, own build, own changelog line. **DRAFT: nothing here is wired into a pool yet.**

| target | adds | used by |
|---|---|---|
| `base` | pinned runner image, jq, python3 (venv, pip), build-essential, gosec, gitleaks, govulncheck | (parent) |
| `lite` | Go, Node (two lines), JDK 17 and 21, Maven, Rust | `homelab-lite` |
| `sysbox` | `lite` + Playwright Chromium and its system libraries | `homelab-sysbox` |

Tool versions and checksums come from a versions file the consuming repo owns (format: `tool-versions.example`), so
this directory stays generic and the image cannot drift from the repo's workflow toolchain. Every download is
verified against a sha256/sha512 from that file; the one exception is Playwright's browser download, which publishes
no checksums. govulncheck is compiled at build time, verified by Go's checksum database.

```
images/build.sh --versions ~/projects/Assay/ci/tool-versions            # lite and sysbox
images/check.sh --versions ~/projects/Assay/ci/tool-versions guard-lite.yaml guard-sysbox.yaml
```

Tags: `runner-ci:<target>-<date>-<hash>` (hash of the versions file and Dockerfile) and the moving `runner-ci:<target>`.
Local builds only; publishing to ghcr comes later. Expected size: `lite` about 4.4 GB, `sysbox` about 5.4 GB on disk,
of which the 2.4 GB runner base is shared with what runs today (layers are shared between the two images).

## Rules

- Tools live under `/opt/ci`, symlinked into `/usr/local/bin`. Never bake anything under `/opt/hostedtoolcache`: the
  guards bind-mount a per-slot directory over it and would hide it.
- Go and Node are also laid out as an actions toolcache in `/opt/ci/toolcache`. To let `actions/setup-go` and
  `setup-node` find them without downloading, a guard can set `RUNNER_TOOL_CACHE: /opt/ci/toolcache` (and drop the
  toolcache bind). Not enabled in the proposal: see "Open questions".
- The image build and its checks run on GitHub-hosted runners or by hand, never on these pools.

## Switching a pool (needs the owner's go-ahead: restarting a guard kills its running jobs)

In `guard-lite.yaml` and `guard-sysbox.yaml`, `runner.image` changes from the pinned myoung34 digest to
`runner-ci:lite-<date>-<hash>` / `runner-ci:sysbox-<date>-<hash>`; then `autoscale.sh check` and restart that guard.
See `guard-image.diff` for the exact lines.

## Open questions

- A locally built image has no registry digest, so it cannot be pinned by digest until it is pushed. The guard must
  accept a plain tag for now (verify with `autoscale.sh check`).
- `RUNNER_TOOL_CACHE` seeding versus the per-slot persistent toolcache bind (kept only for trusted main runs).
- Jobs still need to stop installing what the image has (Assay's `toolchain` action already skips when `mvn`/`rustup`
  are on PATH).
