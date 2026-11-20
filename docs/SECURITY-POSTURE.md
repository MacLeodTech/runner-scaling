# Security and risk posture

Read this before you point a repository at your own hardware. It says what the design protects, what it does not, and
what you have to do yourself. The detailed threat model is in the [README](../README.md#threat-model); vulnerability
reports go through [SECURITY.md](../SECURITY.md).

## The one-paragraph version

Self-hosted runners execute whatever a workflow says, on your machine. This tool makes that safer in three ways: each job
gets a **fresh, single-use container** that is deleted afterwards; the part that talks to GitHub (the **scaler**) has **no
Docker access**, and the part that has Docker access (the **guard**) has **no network and no GitHub credential**, so
taking over the scaler does not hand over your host; and a **firewall** keeps jobs off your LAN. It does **not** make it safe
to run code you do not trust. Use **private repos you control** and treat anyone who can push to them as someone who can run
commands on this machine.

## Who can do what

| If someone has… | They can | They cannot (by design) |
|---|---|---|
| **a workflow in an allowed repo** (push access, a bot such as Dependabot, a stolen token) | run code as root *in a runner container*; read that runner's credentials; use the internet; read other jobs' leftovers only through caches (see below). With a Docker pool, run containers in the job's own Docker daemon | reach your LAN or the host's own services (the firewall); keep anything after the job except warm caches from trusted runs. **With a privileged Docker pool they become root on the host**; with Sysbox that route is closed, but the host kernel is still shared |
| **control of the scaler** (a bug in its GitHub handling, or its credential) | start up to `max_slots` runners built exactly from your config; stop them; register runners in your scale set | create containers of their own design, mount host paths, change images or privileges, point a runner at another server or org (the guard validates every registration against your pinned tenant URL) |
| **the scaler's GitHub App key** | register runners and so run workflows of their own in your org | anything outside "Self-hosted runners" (the App has no other permission); tokens last an hour |
| **control of the guard** | everything: it holds the Docker socket, which is root on the host | n/a. This is why the guard is small, has no network, no credentials, a read-only filesystem and a six-call API |
| **someone on your LAN** | reach the dashboard (if you turned it on) | reach runners from outside; there is no inbound port. Runners are only reachable by what is on their own network |
| **a fork or a public repo** | nothing: the runner group blocks public repos, and fork PR workflows are off or need approval | n/a, provided you did QUICKSTART step 2 |

## Pick a pool by how much you trust the jobs

| Pool | What a job gets | Worst case from a malicious job | Use for |
|---|---|---|---|
| **lite** (no Docker) | root inside one container, no Docker, firewall on | a kernel exploit, or abuse of a misconfigured mount | everything that does not need Docker: tests, lint, builds |
| **sysbox** | the same, plus its own Docker daemon running unprivileged | the same, plus bugs in Sysbox or in a Docker daemon sharing the host kernel; kernel module autoload is blocked by `module_blocklist` | repos that build images or run service containers |
| **privileged** | the same, plus a *privileged* dind | **root on the host**, by starting a privileged container | only if you accept that for every repo on that pool |

Move a repo to the least powerful pool that works. The pools are a convention, not a boundary: they share one GitHub runner
group on the Free plan, and a workflow's `runs-on` chooses the pool.

## What the tool does about it

- **Single-use runners.** Each runner registers with a one-time config, takes one job, and its containers and work volume are removed.
- **Split of duties.** Scaler: network, credential, no Docker. Guard: Docker socket, no network, no credential.
- **The guard builds every container from `guard.yaml` only.** The scaler can send a slot number and a registration, nothing else. The guard
  refuses configs that point at another tenant, another runner name or another work folder. Every refusal is logged as `DENY`.
- **Hardened containers.** Read-only filesystems, all capabilities dropped, `no-new-privileges`, memory, CPU and process limits, and a
  `max_runtime`.
- **Firewall fence.** Runner networks reach the internet and nothing private. New runners wait for the fence (the gate file).
- **Cache isolation.** Warm caches survive only after a *trusted* run (a push, schedule, dispatch or release on `refs/heads/main`). Anything else wipes them,
  so a pull request cannot plant code for a later main build.
- **Resource budget.** One memory budget across pools, with optional headroom for other services on the host.
- **Supply chain.** Releases are built on GitHub-hosted runners with pinned actions, scanned with `govulncheck`, and carry a build attestation. `main` and release tags are protected.

## What it does not do (know these)

1. **It cannot protect you from code you chose to run.** Root in a runner container can read the runner's own credentials, use your internet connection, and attack anything the firewall allows.
2. **The guard is root-equivalent.** It holds the Docker socket. It is small and isolated, but a flaw in it, or a malicious image tag you pull for it, is a host compromise. Pin its image by digest and verify the attestation.
3. **Jobs share the host kernel.** A kernel escape is a host compromise on every pool. Sysbox narrows common routes, it is not a VM.
4. **Branch pushes can change workflows** on plans without branch protection. Your trust boundary is "who can push to these repos".
5. **Caches are shared across repos on `main`.** A trusted job in one repo can leave code for a trusted job in another.
6. **The firewall is IPv4 only,** and has a gap of up to 30 seconds if its rules are flushed (the guard logs it). A job can still send data out to the internet, including your secrets: secrets given to a job are visible to that job.
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
- [ ] Host patched; Docker and, if used, Sysbox kept current.
- [ ] A dashboard, if any, is LAN/VPN-only with a changed password.

## Should the guard talk to Docker through a socket proxy?

A socket proxy (for example [`wollomatic/socket-proxy`](https://github.com/wollomatic/socket-proxy) or
[`tecnativa/docker-socket-proxy`](https://github.com/Tecnativa/docker-socket-proxy)) sits between a client and
`docker.sock` and allows only chosen API paths and methods. It is a good idea for many tools. For the guard it helps
less than it seems, because **the guard's job is to create containers**, and creating containers is the capability that
equals root.

What the guard needs from Docker (every call it makes):

| Method and path | Used for |
|---|---|
| `GET /images/{ref}/json`, `POST /images/create` | check and pull the runner and dind images |
| `GET /networks/{name}`, `POST /networks/create` | per-slot networks (and check their subnet) |
| `POST /volumes/create`, `DELETE /volumes/{name}` | per-slot work volume |
| `POST /containers/create`, `POST /containers/{id}/start`, `DELETE /containers/{id}` | create, start, remove runners, dind and one-shot wipers |
| `GET /containers/json`, `GET /containers/{id}/json`, `GET /containers/{id}/stats` | list, inspect, memory and CPU figures |
| `POST /containers/{id}/exec`, `POST /exec/{id}/start`, `GET /exec/{id}/json` | wipe the work directory and prune inside dind |

What a proxy **can** add: it removes everything else from the guard's reach. A taken-over guard could not, through the
proxy, use `/build`, `/swarm`, `/plugins`, `/system`, `/commit`, copying files in or out of containers
(`/containers/{id}/archive`), attaching to containers, or changing the daemon's configuration. That is real
defence in depth against a bug in the guard.

What it **cannot** do: path-and-method filters do not read the request body. `POST /containers/create` with
`"Privileged": true` or a bind mount of `/` looks identical to the guard's normal request, and an attacker who controls the guard can
send exactly that. So a proxy does **not** change the statement "the guard is root-equivalent". It narrows it.

The costs: one more privileged container to trust, update and keep healthy, and the guard container needs a way to reach it without losing its
`network_mode: none` (a proxy that listens on a unix socket, shared through a volume, keeps that).

Our recommendation:

- **Optional, not default.** If you want it, use a proxy that supports an allow-list per method and path (regular expressions),
  allow only the table above, and mount the real socket only into the proxy. **We have not tested this combination.** Treat the
  table as the starting point, point the guard at the proxy with `--docker <socket path>` (or `DOCKER_SOCKET`), and run your own test
  of start, job, stop and the memory-stats calls before relying on it. Contributions that add a tested example are welcome.
- **What would really constrain the guard** is an enforcement point that reads the request body: a Docker authorization plugin
  (for example one backed by Open Policy Agent) that rejects container creation unless it matches the shape the guard is configured to
  produce (no `Privileged` unless the config says so, binds only from an allow-list, images only from the config). It protects against a
  compromised guard, but it is daemon-wide, needs a daemon restart to install, and has not been tried with this project.
- **The biggest lever you control** is the same as before: keep the guard image pinned by digest and verified, keep its config
  read-only, and keep its surface small. Rootless Docker or Podman would change the model more fundamentally; they are not supported or tested here.
