# Changelog

Format follows [Keep a Changelog](https://keepachangelog.com/). Versions follow [semver](https://semver.org/); until
1.0, a minor version may change the config or flags.

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

[0.3.0]: https://github.com/MacLeodTech/runner-scaling/releases/tag/v0.3.0
[0.2.0]: https://github.com/MacLeodTech/runner-scaling/releases/tag/v0.2.0
[0.1.0]: https://github.com/MacLeodTech/runner-scaling/releases/tag/v0.1.0
