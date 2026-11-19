# Changelog

Format follows [Keep a Changelog](https://keepachangelog.com/). Versions follow [semver](https://semver.org/); until
1.0, a minor version may change the config or flags.

## [Unreleased]

### Changed
- Dashboard: one **Runners** section. Each slot now carries the job it is running (repo, trigger, who, step progress, step list, stall flag) next to its CPU and memory, so the separate **Running** list is gone; it only appears, as "Running (no runner slot)", for a job with no matching slot (for example just after a scaler restart).

### Added
- **Job detail from GitHub** on the dashboard, `/api/state`, `runners` and `/metrics`: each runner slot and job shows its repo, job, workflow, trigger, branch or PR, commit title and who triggered it; running jobs show the current step (`7/14`, name, time in it, progress bar) with an expandable step list; finished jobs keep their step timings and the failed step. Needs `Actions: read` on the App or on a token (`DASHBOARD_GITHUB_TOKEN[_FILE]`); without it the dashboard says so and carries on. See the README.
- A **stalled** flag for a job that sits in one step for `DASHBOARD_STALL_AFTER` (10m) while its runner is idle, and `runner_scaling_jobs_stalled`.
- Optional `DASHBOARD_FAILURE_LOGS=on`: the last 40 log lines of failed jobs.
- `runners slots` shows repo, job, who and step per runner; `runners jobs` the same, and `-v` lists steps and failure logs.
- Metrics `runner_scaling_github_up`, `runner_scaling_github_rate_remaining`, `runner_scaling_github_requests_total`.

### Fixed
- Dashboard and `runners` showed busy runners as "idle" and listed no running or queued jobs. The tracker keyed jobs by `runnerRequestId`, which live scale set messages send as 0, so every job collapsed into one entry. Jobs are now keyed by the request id when set, else the job id, else repo, run and job name. The runners themselves were never affected.
- `runners host --gate-file` reported a gate it could not read (root-only Docker volume) as MISSING; it now says `unknown`. The token-file warning no longer suggests `chmod 600` on a file a container also reads.

### Documentation
- README: a section on running a repo-level scale set, for setups without an organization (credentials, no runner groups, public-repo warning). Marked as not run end to end.

## [0.8.0] - 2026-11-19

### Added
- Dashboard **Sessions** page: each user sees and can end their own logins (address, browser, sign-in time, last
  activity); admins see and can end everyone's. API: `GET /api/sessions`, `POST /api/sessions/{id}/revoke`.
- Dashboard **audit log**: login, failed-login, lockout, logout, password, user and session-revoke events are appended
  to a size-bounded JSON-lines file (`DASHBOARD_AUDIT_FILE`, `DASHBOARD_AUDIT_MAX`; on by default next to the users
  file, `off` to disable), with an admin-only **Audit** page (`GET /api/audit`).
- `runners`: warns when the token file is readable by other users; `runners completion bash|zsh`; `runners host`
  takes `--gate-file` and `--fence` to check the firewall gate and that containers sit inside the fenced range.
- CI attaches `runners` binaries (linux and macOS, amd64 and arm64) and `SHA256SUMS` to each `v*` release.

### Changed
- The firewall script falls back to the `FORWARD` chain when Docker has no `DOCKER-USER` chain (Docker's nftables
  backend), instead of failing. The example compose file logs to journald, so logs survive container removal.

### Fixed
- The scaler compared scale-set labels in a way that let a duplicate label hide a change (`[x,y]` vs `[x,x]` counted as
  equal), so a label change might not recreate the scale set. It now compares the two label sets.

### Tests
- `ensureScaleSet` against a fake GitHub (create, unchanged, in-place update, recreate when the update is ignored).
- Wire-format contract test for `internal/api`, and tests for the `cmd/runner-scaling` helpers.

## [0.7.0] - 2026-11-18

### Added
- `cache_max` (guard config): caps the warm caches a slot keeps after trusted jobs. Over the cap, the guard empties
  them after the job (a cold start for the next one), so cache growth is bounded.
- The dashboard links each job to its workflow run on GitHub.

## [0.6.1] - 2026-11-18

### Security
- The JIT config is passed to the runner as `ACTIONS_RUNNER_INPUT_JITCONFIG` instead of `--jitconfig`, so it no longer
  appears in the runner's `/proc/<pid>/cmdline` (readable by every user on the host, and by jobs through `ps`).

### Documentation
- Reverse-proxy example for the dashboard (Caddy, LAN only, `X-Forwarded-Proto`), and a note on running `:latest`.

## [0.6.0] - 2026-11-18

### Added

- **Dashboard accounts** (`DASHBOARD_USERS_FILE`, opt-in). A login page instead of the shared token for browsers. The
  first start creates `admin`/`admin`, which must choose a new password at first login. Admins add users in the dashboard;
  each gets a generated one-time password they must change at first login (also for resets). Roles `admin` and `user`
  (view only), PBKDF2-HMAC-SHA256 hashes in a 0600 file, 12 h in-memory sessions (HttpOnly, SameSite=Strict), a lockout
  after five failures, JSON-only and same-origin state changes. The token keeps working for peers, the `runners` CLI and
  Prometheus. See README, "Dashboard accounts".

## [0.5.0] - 2026-11-18

### Added

- **`module_blocklist`.** The guard can now stop jobs from making the host kernel load modules. A container with
  `NET_ADMIN` that creates a link of an unloaded type (`ip link add type ifb`) makes the kernel autoload that module on
  the host, with plain runc and under Sysbox alike. At startup the guard writes `install <module> /bin/false` rules to
  `/etc/modprobe.d/50-runner-scaling-<prefix>.conf` from a one-shot container with no network and only that directory
  mounted. Opt-in (omitted: the host is not touched; `[]` removes the guard's own file), idempotent, and it never touches a
  file it did not write. A failed write is logged and counted (`module_blocklist_failures_total`); the pool keeps running.
  Checked on a real host: a listed module was refused (`Unknown device type`), an unlisted one still loaded.

### Changed

- Docs: the Sysbox section records two verified results. Writes to `/proc/sys` and `/proc/sysrq-trigger` from a
  privileged inner container are refused under Sysbox (and change the host under a plain privileged container), and module
  autoload happens under Sysbox too, which `module_blocklist` now covers.

## [0.4.0] - 2026-11-18

### Added

- **`runners` CLI** (`cmd/runners`): a read-only command that shows pools, slots, jobs and memory from the scaler's
  dashboard (`status`, `slots`, `jobs`, `memory`, `watch`, `--json`). It needs only the dashboard URL and token,
  which it reads from `--token-file`, `RUNNERS_TOKEN`, or `~/.config/runners/token`. `runners host` lists the runner
  containers on the host with their runtime and privilege flag; it runs the `docker` CLI.
- **Sysbox support, tested.** `runtime: sysbox-runc` on `dind` runs the sidecar without `privileged`. A real image
  build took the same time as under privileged dind, three Sysbox slots ran at once next to a privileged one, and the
  inner privileged container had an unprivileged host uid, 16 `/dev` entries (218 under privileged dind) and no host
  block devices (14). New `examples/guard-sysbox.yaml` and a README section, "Running dind under Sysbox", with the
  install pitfalls, what was measured, and what it does not cover.

### Changed

- The README's threat model points at the Sysbox section instead of saying the runtime was untested.
- Docs: `examples/guard.yaml` mentions `runtime: sysbox-runc` next to `privileged`.

### Known limitations

- Sysbox narrows the "any job is root on the host" route; it does not remove the shared kernel. A job with
  `NET_ADMIN` can still make the host kernel load a module (`dummy`, `ifb`, `bonding`, `nlmon` seen), with or without Sysbox.
- Sysbox was tried with one real workflow and a probe suite, not weeks of use. Reboot behaviour is untested.

## [0.3.0] - 2026-11-18

### Changed

- Docs: the ghcr image is in private preview for now; the README and example compose say how to build it locally.

### Added

- **`runtime:`** on `runner` and `dind`: the name of an OCI runtime registered with Docker (Sysbox, gVisor), so the
  dind sidecar can run without `privileged`. Passed through as-is; untested against a real runtime so far.
- **Gate watch.** The guard now notices when `require_file` vanishes while slots are running (the firewall lost its
  rules): one `GATE WITHDRAWN` error line and a `gate_withdrawn_total` count per outage. Running jobs are left alone.
- **Runner image check.** `scripts/check-runner-image.sh guard.yaml [...]` compares the runner version inside each
  pinned runner image with the newest `actions/runner` release. Runners never update themselves
  (`DisableUpdate`), so run it now and then.

## [0.2.0] - 2026-11-18

### Added

- **Metrics.** `GET /metrics` on the scaler's dashboard listener, in Prometheus text format and behind the dashboard
  token: jobs queued and running, slots by state, memory use against the budget, histograms of job start latency,
  job duration and slot start time, and the guard's counters (denials, refusals, reaps by reason, starts).
- **Guard counters.** The guard counts what it does and serves it on a new read-only `GET /v1/counters`, and `GET
  /v1/info` now reports `memory_budget`. The scaler re-exports the counters, since the guard has no network.

## [0.1.0] - 2026-11-18

First release. It depends on `github.com/actions/scaleset` v0.5.0 and the runner scale set API, both in public
preview: GitHub may change them.

### Added

- **Scaler and guard.** The scaler long-polls GitHub for the runner scale set and holds the GitHub credential (a
  GitHub App or a token, from the environment or a file). The guard holds the Docker socket, has no network, and
  builds every container from `guard.yaml`. The scaler can only pick a slot number and hand over a JIT config.
- **Ephemeral runners.** Each queued job gets a fresh runner, and optionally its own `dind` sidecar and network, which
  is removed after that one job. Nothing runs while nothing is queued. Slots start in parallel, and a failed start is
  retried after 15 seconds.
- **JIT config checks.** The guard requires every URL in a JIT config to be https and under the pinned tenant URL
  (or on a shared host such as the Actions broker), the runner name and work folder to match the request, and every
  file name to be a plain name. The tenant URL is pinned on first use.
- **Pools.** Run several guard + scaler pairs, for example one with `dind` and one without. They share one
  host-wide `memory_budget` enforced by the guards (admission check, reserve, reap-newest backstop), plus `cpu_shares`.
  A new slot counts at half its memory limits for its first 2 minutes (`memory_ramp`, `memory_ramp_share`), then at
  measured use, so a burst of fresh jobs isn't admitted against memory they haven't used yet.
- **Cache isolation.** Directories in `cache_dirs` are emptied after every job except one the scaler vouches for
  (push, schedule, dispatch or release on `CACHE_TRUSTED_REFS`, default `refs/heads/main`), and at startup. A slot
  whose wipe failed is not reused until a wipe succeeds. Work directories (`work_bind`) are emptied after every job.
- **Firewall gate.** `require_file` holds back every slot until a host firewall has put its rules in place.
  `examples/firewall.sh` is a sidecar that applies the fence and keeps it applied.
- **Soft memory limit.** Optional `memory_high` on `runner` and `dind` slows a container down past it (cgroup v2
  `memory.high`) instead of OOM-killing it. Docker can't set it, so the guard writes it from a one-shot container.
- **Limits.** `max_runtime` reaps a stuck runner; `memory`, `cpus`, `pids` and `log_opts` limit each container.
- **Dashboard.** A read-only status page (`DASHBOARD_ADDR`) with the slots, running and queued jobs, recent
  results, and each runner's CPU, memory, processes, network and disk use. `DASHBOARD_PEERS` folds other pools in.
- **Examples** for both pool kinds (`examples/guard.yaml`, `examples/guard-lite.yaml`, `examples/compose.yaml`).
- **Fuzzing and network checks.** A fuzz test for the JIT config validator (`FuzzValidate`), and the guard refuses to
  reuse an existing Docker network whose subnet differs from the one it expects.
- CI on GitHub-hosted runners: `gofmt`, `go vet`, `go test -race`, `govulncheck`, and an image build with build
  provenance attestation on `v*` tags.

### Known limitations

- **With a `dind` sidecar, any job is root on the host** (the sidecar is privileged). Only run jobs from repos you
  trust on that pool, and use a pool without `dind` for everything else. Running dind under Sysbox, and the other
  pool under gVisor, is planned but not in this release. See the README's threat model.
- Caches are shared between jobs from different repos on a trusted ref.
- The runner image is pinned with `DisableUpdate`, so it needs a deliberate bump when GitHub retires its runner
  version.
- The firewall script needs Docker's iptables backend with the `DOCKER-USER` chain.

[0.6.0]: https://github.com/MacLeodTech/runner-scaling/releases/tag/v0.6.0
[0.5.0]: https://github.com/MacLeodTech/runner-scaling/releases/tag/v0.5.0
[0.4.0]: https://github.com/MacLeodTech/runner-scaling/releases/tag/v0.4.0
[0.3.0]: https://github.com/MacLeodTech/runner-scaling/releases/tag/v0.3.0
[0.2.0]: https://github.com/MacLeodTech/runner-scaling/releases/tag/v0.2.0
[0.1.0]: https://github.com/MacLeodTech/runner-scaling/releases/tag/v0.1.0
