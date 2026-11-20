# Security and risk posture

Read this before you point a repository at your own hardware. It says what the design protects, what it does not, and
what you have to do yourself. The detailed threat model is in the [README](../README.md#threat-model); vulnerability
reports go through [SECURITY.md](../SECURITY.md).

## The one-paragraph version

Self-hosted runners execute whatever a workflow says, on your machine. This tool makes that safer in three ways: each job
gets a **fresh, single-use container** that is deleted afterwards; the part that talks to GitHub (the **scaler**) has **no
Docker access**, and the part that decides what to start (the **guard**) has **no network, no GitHub credential, no Docker
socket and does not run as root**. Only a tiny **Docker policy proxy** holds the socket, and it refuses every Docker call that
is not exactly a container the guard's config describes. So taking over the scaler, or even the guard, does not hand over
your host; and a **firewall** keeps jobs off your LAN. It does **not** make it safe
to run code you do not trust. Use **private repos you control** and treat anyone who can push to them as someone who can run
commands on this machine.

## Who can do what

| If someone has… | They can | They cannot (by design) |
|---|---|---|
| **a workflow in an allowed repo** (push access, a bot such as Dependabot, a stolen token) | run code as root *in a runner container*; read that runner's credentials; use the internet; read other jobs' leftovers only through caches (see below). With a Docker pool, run containers in the job's own Docker daemon | reach your LAN or the host's own services (the firewall); keep anything after the job except warm caches from trusted runs. **With a privileged Docker pool they become root on the host**; with Sysbox that route is closed, but the host kernel is still shared |
| **control of the scaler** (a bug in its GitHub handling, or its credential) | start up to `max_slots` runners built exactly from your config; stop them; register runners in your scale set | create containers of their own design, mount host paths, change images or privileges, point a runner at another server or org (the guard validates every registration against your pinned tenant URL) |
| **the scaler's GitHub App key** | register runners and so run workflows of their own in your org | anything outside "Self-hosted runners" (the App has no other permission); tokens last an hour |
| **control of the guard** | what the config allows and no more: start and stop containers built exactly from `guard.yaml` (a runner, its Docker sidecar, the config's one-shot helpers), on this pool's own names and networks | create any other container, mount any other host path, run any other image or flag, run commands in containers other than the pool's own sidecars, touch other containers on the host. The [Docker policy proxy](#the-docker-policy-proxy) refuses these even though the guard asks |
| **control of the Docker policy proxy** | everything: it holds the Docker socket, which is root on the host | n/a. This is why it is small, has no network, a read-only filesystem and runs as an unprivileged user that only has the socket's group |
| **someone on your LAN** | reach the dashboard (if you turned it on) | reach runners from outside; there is no inbound port. Runners are only reachable by what is on their own network |
| **a fork or a public repo** | nothing: the runner group blocks public repos, and fork PR workflows are off or need approval | n/a, provided you did QUICKSTART step 2 |

## Pick a pool by how much you trust the jobs

| Pool | What a job gets | Worst case from a malicious job | Use for |
|---|---|---|---|
| **lite** (no Docker) | root inside one container, no Docker, firewall on | a kernel exploit, or abuse of a misconfigured mount | everything that does not need Docker: tests, lint, builds |
| **sysbox** | the same, plus its own Docker daemon running unprivileged | the same, plus bugs in Sysbox or in a Docker daemon sharing the host kernel; kernel module autoload is blocked by `module_blocklist` | repos that build images or run service containers |
| **privileged** | the same, plus a *privileged* dind | **root on the host**, by starting a privileged container | only if you accept that for every repo on that pool |

Move a repo to the least powerful pool that works. The pools are a convention, not a boundary: they share one GitHub runner
group on the Free plan, and a workflow's `runs-on` chooses the pool. Several pools can share one guard, one policy proxy and
one scaler (one `guard.yaml` with `pools:`); that does not weaken the split: each pool keeps its own names, networks and
directories, the policy proxy allows a request only when one pool's policy allows all of it, and a request never gets rights from
another pool's config. The shared parts are the processes, so a bug in the guard or scaler reaches every pool at once, which is
the case for keeping a pool with **privileged** dind in a stack of its own.

## What the tool does about it

- **Single-use runners.** Each runner registers with a one-time config, takes one job, and its containers and work volume are removed.
- **Split of duties.** Scaler: network, credential, no Docker. Guard: Docker socket, no network, no credential.
- **The guard builds every container from `guard.yaml` only.** The scaler can send a slot number and a registration, nothing else. The guard
  refuses configs that point at another tenant, another runner name or another work folder. Every refusal is logged as `DENY`.
- **Hardened containers.** Read-only filesystems, all capabilities dropped, `no-new-privileges`, memory, CPU and process limits, and a
  `max_runtime`.
- **Firewall fence.** Runner networks reach the internet and nothing private. New runners wait for the fence (the gate file), and the fence is checked against the kernel every few seconds.
- **Cache isolation.** Warm caches survive only after a *trusted* run (a push, schedule, dispatch or release on `refs/heads/main`). Anything else wipes them,
  so a pull request cannot plant code for a later main build.
- **Resource budget.** One memory budget across pools, with optional headroom for other services on the host.
- **Supply chain.** Releases are built on GitHub-hosted runners with pinned actions, scanned with `govulncheck`, and carry a build attestation. `main` and release tags are protected.

## What it does not do (know these)

1. **It cannot protect you from code you chose to run.** Root in a runner container can read the runner's own credentials, use your internet connection, and attack anything the firewall allows.
2. **The Docker policy proxy is root-equivalent.** It holds the Docker socket. It is small and isolated, but a flaw in it, or a malicious image tag you pull for it, is a host compromise. Pin the image by digest and verify the attestation. The proxy also **trusts `guard.yaml`**: a config that asks for a privileged sidecar (the privileged pool does) gets one. Keep the file writable only by you, and mount it read-only, as the examples do.
3. **Jobs share the host kernel.** A kernel escape is a host compromise on every pool. Sysbox narrows common routes, it is not a VM.
4. **Branch pushes can change workflows** on plans without branch protection. Your trust boundary is "who can push to these repos".
5. **Caches are shared across repos on `main`.** A trusted job in one repo can leave code for a trusted job in another.
6. **The fence is a blocklist of private ranges.** It lives in its own nftables table and is read back every 3 seconds, so if something deletes it there is a gap of at most a few seconds (the guard logs it, and with `gate_loss: stop` removes running slots after `gate_grace`). IPv6 is fenced only if you list the networks in `FENCE_SRC6`; the guard creates IPv4-only networks. A job can still send data out to the internet, including your secrets: secrets given to a job are visible to that job.
7. **The dashboard is optional and runs inside the scaler.** If you turn it on, keep it on your LAN or VPN behind a reverse proxy, change the default `admin` password immediately, and do not expose it to the internet.
8. **The scale-set API is a public preview** from GitHub, and so is the library this tool uses. Pin versions and read the changelog before upgrading.

## Your checklist

- [ ] Only private repos in the runner group; "Allow public repositories" off (verify with the `gh api` command in the QUICKSTART).
- [ ] Few people with push access; no Dependabot or other bots on pools with Docker (or send their jobs to `ubuntu-latest`).
- [ ] The GitHub App has only *Self-hosted runners: read and write*; its key is a root-owned `0400` file, not in git, not in an environment variable.
- [ ] `tenant_url` is pinned.
- [ ] Image tags are replaced by digests, and `gh attestation verify` passes for the runner-scaling image.
- [ ] You can see logs after a restart (journald), and you alert on `DENY` bursts, `GATE WITHDRAWN` and `module_blocklist_failures_total`.
- [ ] The firewall test (`curl` to a LAN address from a job) fails as it should, and you re-run it after Docker or kernel upgrades.
- [ ] Only `docker-policy` mounts `/var/run/docker.sock`: `docker ps -q | xargs docker inspect --format '{{.Name}} {{range .Mounts}}{{.Source}} {{end}}' | grep docker.sock` lists nothing else from this stack.
- [ ] You alert on `DENY` lines from `docker-policy`: the guard never makes a refused call in normal operation, so any is news.
- [ ] Host patched; Docker and, if used, Sysbox kept current.
- [ ] A dashboard, if any, is LAN/VPN-only with a changed password.

## The Docker policy proxy

**The problem.** Whatever holds the Docker socket is root on the host: one `POST /containers/create` with `"Privileged": true` and a
bind mount of `/` is enough. The guard's whole job is to create containers, so a bug in the guard, or something that
takes it over, would otherwise be the end of the host. A usual Docker socket proxy (for example
[`tecnativa/docker-socket-proxy`](https://github.com/Tecnativa/docker-socket-proxy) or
[`wollomatic/socket-proxy`](https://github.com/wollomatic/socket-proxy)) filters by method and path only. It cannot tell the guard's
normal create from a malicious one, because the difference is in the request **body**.

**What we do instead.** `runner-scaling docker-policy` is a proxy that reads the body. It loads the same `guard.yaml` as the
guard and, with the guard's own code, rebuilds the one container each request is allowed to be. A request that differs in any field
(a privileged flag, an extra mount, another image, a capability, a different network, an unknown extra setting) is refused with 403
and logged as `DENY`. The guard has no Docker socket and does not run as root; it reaches Docker only through the proxy's unix socket.

| Call | What the proxy allows |
|---|---|
| `POST /containers/create` | only a container named `<prefix>-runner-N`, `-dind-N` or `-wipe-N` whose whole body equals what the config builds for that slot. A runner's JIT registration is validated against the pinned tenant too. One-shot helpers may only run the guard's own commands (wipe a work directory, wipe or trim the caches, set `memory_high`, write the module blocklist) with exactly the mounts that command uses |
| start, inspect, stats, remove | only containers that carry the runner-scaling label; removing or starting needs this pool's label |
| `POST /containers/{id}/exec` | only in this pool's own dind sidecars, only the config's wipe and `prune_on_stop` commands, no extra options; `exec/start` and `exec/json` only for exec ids it saw created |
| images | only a pull or inspect of exactly the configured images |
| networks, volumes | only this pool's names, with the config's subnet and labels |
| everything else | refused: `/info`, `/build`, `/swarm`, `/plugins`, copy-in/out (`archive`), attach, other containers, other API versions, encoded or odd paths, connection upgrades, bodies over 256 KiB |

The guard can still list its own containers, and read the runner-scaling containers of other pools for the shared memory budget.
It cannot see or touch anything else on the host.

**What it does not do.**

- It trusts the config. If `guard.yaml` says `privileged: true` for the sidecar, a privileged sidecar is allowed. The proxy removes
  the guard (and the scaler) from your trust base; it does not remove the config.
- It does not look inside images, and it cannot restrain what a job does inside the container it was given. It is not a VM.
- It is itself root-equivalent: it holds the socket. It is one file you can read in
  [`internal/guard/dockerpolicy.go`](../internal/guard/dockerpolicy.go). It reads its config once at start, so restart it with the guard when you change the config.
- It checks requests, not what the daemon does with a request that is allowed; a Docker or kernel bug is out of its reach.

**How it was tested.** Unit tests drive the real guard through the real proxy against a fake daemon, for the privileged, `work_bind`,
caches, `memory_high`, module-blocklist and no-dind variants, and fail if the proxy refuses anything the guard does in normal operation. Negative tests
change one field of a valid request at a time and require a refusal; removing the body comparison makes 21 of them fail.
It was also run against a real Docker daemon: start and stop of a slot with sidecar, runner, health check, `memory_high` helper, exec, cache
wipe and removal, with no refusal; then the proxy's socket was attacked with a privileged create, `/info`, a listing of all containers, and
removing, starting and exec'ing in an unrelated container: all refused. The Sysbox pool was tested the same way on a real daemon (`docker:28-dind` under `sysbox-runc`, `memory_high` on both containers, the module-blocklist helper writing and then removing its host file, a real `docker run` and `docker build` inside the sidecar, teardown with prune and cache wipe). Changing the runtime, privilege, a bind, the network, a capability, a PID mode, the memory limit or the image of that sidecar, one at a time, was refused at exactly that field. A real GitHub job through the whole chain has not been run yet.

**One more layer is possible** if you want it: a Docker authorization plugin (for example one backed by Open Policy Agent) is enforced
inside the daemon itself, so it would also constrain the proxy. It is daemon-wide, needs a daemon restart to install, and has
not been tried with this project.
