# Quickstart: from nothing to a working runner

This takes you from a bare Linux host to a GitHub Actions job running on your own hardware, in about 30 minutes. It
starts with the **safest and smallest** setup (runners without Docker) and shows how to add more afterwards.

How to read it: every step says **You do** (something only you can do) and what the tool does for you. Each step ends
with a **Check** so you know it worked before you move on.

- [The fast path (recommended)](#the-fast-path-recommended)
- [0. What you are building, and what it costs you](#0-what-you-are-building-and-what-it-costs-you)
- [1. Check the host](#1-check-the-host)
- [2. Prepare GitHub](#2-prepare-github)
- [3. Create the GitHub App](#3-create-the-github-app)
- [4. Put the files on the host](#4-put-the-files-on-the-host)
- [5. Start it](#5-start-it)
- [6. Run a first job (and pin the tenant)](#6-run-a-first-job-and-pin-the-tenant)
- [7. Make it permanent and safe](#7-make-it-permanent-and-safe)
- [Add a pool that has Docker](#add-a-pool-that-has-docker)
- [Day two: updating, watching, removing](#day-two-updating-watching-removing)

Stuck? See [TROUBLESHOOTING.md](TROUBLESHOOTING.md). Want the risks first? Read
[SECURITY-POSTURE.md](SECURITY-POSTURE.md) before step 2; it is short.

---

## The fast path (recommended)

`runner-scaling` also ships as a setup tool. It checks your host, creates the GitHub App through your browser, and writes
every file in steps 3 and 4 for you. Steps 0 to 7 below explain what each piece is and are the manual route; read step 0 and
[SECURITY-POSTURE.md](SECURITY-POSTURE.md) either way, and do step 2 (limiting who can use the runners) yourself.

```
# 1. Get the tool (Linux; use the arm64 file on a Raspberry Pi-class machine) and verify it
V=$(curl -fsSL -o /dev/null -w '%{url_effective}' https://github.com/MacLeodTech/runner-scaling/releases/latest | sed 's#.*/##')
curl -fsSLO https://github.com/MacLeodTech/runner-scaling/releases/download/$V/runner-scaling-linux-amd64
curl -fsSL https://github.com/MacLeodTech/runner-scaling/releases/download/$V/SHA256SUMS | grep runner-scaling-linux-amd64 | sha256sum -c
chmod +x runner-scaling-linux-amd64 && mv runner-scaling-linux-amd64 runner-scaling

# 2. Is this host ready? Every FAIL line says how to fix it.
./runner-scaling doctor

# 3. Do QUICKSTART step 2 (runner group and workflow settings) on github.com, then:
./runner-scaling init --org YOUR-ORG --create-app --dir ~/runner-scaling
```

`init --create-app` prints an address. Open it in a browser signed in to GitHub as an owner of the organization, press the
button, press **Create GitHub App**, then **Install** it on the organization. The terminal picks up the Client ID and
installation id by itself, writes `compose.yaml`, `guard-lite.yaml`, `.env` and `secrets/github_app.pem`
(a free subnet, slots and memory sized for this host), and runs `doctor` on the result. It then prints the one `sudo chown`
command that hands the key to the scaler's user. (On a headless host: `--listen 127.0.0.1:8765` and
`ssh -L 8765:127.0.0.1:8765 <host>` from your laptop.) Already made an App by hand? Use `--client-id`, `--installation-id`
and `--key-file` instead of `--create-app`.

```
# 4. Start it
cd ~/runner-scaling && docker compose up -d

# 5. Run any workflow on  runs-on: [self-hosted, ci-lite]  (step 6 has a ready-made one). The guard refuses its first
#    registration on purpose; this shows what it saw, asks you to confirm it is your organization's, pins it and restarts:
./runner-scaling pin-tenant --dir ~/runner-scaling
```

Then continue at [step 6's check](#6-run-a-first-job-and-pin-the-tenant) (the job should now run and the LAN should be
blocked) and [step 7](#7-make-it-permanent-and-safe). Something off? `runner-scaling doctor --dir ~/runner-scaling` again.

---

## 0. What you are building, and what it costs you

Three small containers and one helper run all the time. They hold **no runners while idle**, and each holds only what it
needs:

```
GitHub <-- outbound HTTPS only -- scaler --unix--> guard --unix--> docker-policy --docker.sock--> Docker
                                    |                |                  |
                      holds the GitHub App key  builds a runner   checks EVERY Docker call against
                      has NO Docker access      from guard-lite   guard-lite.yaml; refuses anything
                                                .yaml only. No    that differs by one field. The
                                                network, key or   only container with the Docker
                                                socket; not root  socket. No network; not root
```

When a job is queued, the scaler gets a single-use runner registration from GitHub and hands it to the guard. The
guard asks `docker-policy` to start a fresh runner container, which allows exactly the container the config describes
and nothing else. The runner takes that one job, and the container is deleted afterwards. A firewall sidecar stops those
containers reaching your LAN or the host.

What you must accept (details in [SECURITY-POSTURE.md](SECURITY-POSTURE.md)):

- **Only run code you trust.** Anyone who can get a workflow onto a repo that uses these runners can run code on your
  hardware. Use **private repos only**, and never public ones.
- **`docker-policy` holds the Docker socket**, which is root on the host. It is kept tiny on purpose, and it is the one
  place that decides what Docker is asked to do, so keep `guard-lite.yaml` writable only by you.
- Runners without Docker (this quickstart) are root inside their container, not on the host. Runners **with** Docker
  are a bigger risk and are covered [at the end](#add-a-pool-that-has-docker).

You will need: a Linux machine you control, a GitHub **organization** (a free one works; for a single repo without an
org see the [README](../README.md#without-an-organization-repo-level-scale-set)), and admin rights on both.

## 1. Check the host

**You do:** run these on the machine that will host the runners. Each line says what you need and why.

| Need | Why | Check | Good looks like |
|---|---|---|---|
| Docker Engine 25 or newer | the guard speaks Engine API v1.44 | `docker version --format '{{.Server.Version}} (api {{.Server.APIVersion}})'` | API `1.44` or higher |
| Docker Compose v2 | runs the stack | `docker compose version` | `v2.x` or newer |
| A normal (rootful) Docker | the guard needs the real socket; rootless Docker is untested | `docker info --format '{{.SecurityOptions}}'` | does **not** list `rootless` |
| `nf_tables` in the kernel | the firewall sidecar fences the runners | `sudo nft list tables` (or `sudo iptables -L -n \| head -3`) | no error |
| cgroup v2 (recommended) | only `memory_high` needs it; the memory budget works on v1 | `stat -fc %T /sys/fs/cgroup` | `cgroup2fs` |
| Correct clock | the GitHub App signs short-lived tokens; a skewed clock is rejected | `timedatectl \| grep synchronized` | `System clock synchronized: yes` |
| Outbound HTTPS (443) | the scaler and runners reach GitHub and GHCR | `curl -sI https://api.github.com \| head -1` and `curl -sI https://ghcr.io \| head -1` | an `HTTP` status line |
| Disk | the runner image is about 2.4 GB, plus caches and job workspaces | `df -h /var/lib/docker /srv` | at least 30 GB free |
| Memory | each runner may use up to its `memory` limit (3 GB in the lite example) | `free -h` | enough for your slots plus the host's own use (the example caps runners at 8 GB total) |
| systemd journal (optional) | the example logs to journald so logs survive restarts | `systemctl is-system-running` | `running` (or `degraded`); no systemd? see step 4 |

Also: your user must be able to run `docker` (the `docker` group, or use `sudo`), and you need `sudo` once for the
secret file in step 3.

**Check:** you can answer "yes" to every row. If a row fails, fix it first; every later problem is harder to see.

> Do you also need Docker *inside* jobs (`docker build`, service containers)? Not yet. Finish this quickstart with the
> lite pool, then see [Add a pool that has Docker](#add-a-pool-that-has-docker).

## 2. Prepare GitHub

**You do**, in your organization's settings on github.com (or with the `gh` commands shown):

1. **Limit who can use the runners.** Organization settings > Actions > Runner groups > **Default**:
   - Repository access: **Selected repositories**, and add only the private repos you will use.
   - Make sure **Allow public repositories** is **off** (it is off by default).
   - Check it from a terminal:
     ```
     gh api orgs/YOUR-ORG/actions/runner-groups --jq '.runner_groups[] | {name,visibility,allows_public_repositories}'
     ```
     You want `"visibility":"selected"` and `"allows_public_repositories":false`.
2. **Tighten workflow defaults** (Organization settings > Actions > General):
   - Fork pull request workflows: require approval for all outside contributors (or keep them off).
   - Default workflow token permissions: **Read repository contents** only.
   - Optional but good: allow only selected actions and require them to be pinned by SHA.
3. **Know the limit of the Free plan:** branch protection is not available on private repos there, so anyone who can
   push a branch can change a workflow, and that workflow runs on your hardware. Only give push access to people you
   would trust with a shell on this host.

**Check:** the `gh api` command above prints what you want.

## 3. Create the GitHub App

The scaler needs a credential that can create runner registrations and nothing else. A GitHub App is the right
tool: its permission is limited to self-hosted runners and its tokens expire after an hour.

**You do:**

1. Organization settings > Developer settings > GitHub Apps > **New GitHub App**.
2. Name: anything (for example `runner-scaler`). Homepage URL: any URL.
3. **Untick Webhook > Active.** The scaler polls; nothing calls it.
4. Permissions: **Organization permissions > Self-hosted runners: Read and write.** Grant nothing else.
5. Where can it be installed: **Only on this account.**
6. Create it, then on the App's page: **Generate a private key** (a `.pem` downloads) and note the **Client ID**
   (starts with `Iv`).
7. **Install App** (left menu) on your organization, for all repositories (it only manages runners). The address bar then shows
   `.../settings/installations/<number>`: that number is the **installation ID**.

**You do next**, on the host, in the directory where the stack will live (this guide uses `/opt/runner-scaling`):

```
sudo mkdir -p /opt/runner-scaling/secrets && cd /opt/runner-scaling
# put the downloaded key at secrets/github_app.pem, then:
sudo chown 65532:65532 secrets/github_app.pem   # the scaler container runs as uid 65532
sudo chmod 0400 secrets/github_app.pem          # only that user can read it
```

Why a file and not an environment variable: anything in an environment variable shows up in `docker inspect`; a file
mounted into the scaler does not.

**Check:** `sudo ls -ln /opt/runner-scaling/secrets/github_app.pem` shows owner `65532` and mode `-r--------`. Keep
the Client ID and installation ID for the next step.

## 4. Put the files on the host

**You do:** copy these files from this repo's [`examples/`](../examples) directory into `/opt/runner-scaling`
(download them from GitHub or `git clone` the repo):

| Copy | As | What it is |
|---|---|---|
| `compose.lite.yaml` | `compose.yaml` | the firewall, docker-policy, guard and scaler containers |
| `guard-lite.yaml` | `guard-lite.yaml` | **the one config that decides what a runner is** |
| `env.example` | `.env` | your GitHub App identifiers and the Docker socket's group id |

Then edit:

1. **`.env`**: put in your Client ID and installation ID, and the Docker socket's group id: `DOCKER_GID=$(stat -c %g /var/run/docker.sock)`
   (a number such as `988` or `999`). `docker-policy` joins that group so it can use the socket without running as root.
2. **`guard-lite.yaml`**: set `github_url: https://github.com/YOUR-ORG`. Leave `tenant_url` commented out for now
   (step 6 explains). The defaults (4 slots, 3 GB per runner, 8 GB total, 6 h job limit) suit a 16 GB machine; lower
   `max_slots` and `memory_budget` on a smaller one. `memory_budget` must be at least what one slot may use (here 3 GB).
3. **No systemd on the host?** In `compose.yaml` delete the `logging:` block under `x-hardened`.
4. **Your LAN already uses `172.30.0.0/16`?** Pick another unused private range and change it in three places: `subnet`
   in `guard-lite.yaml`, and `FENCE_SRC` in `compose.yaml`. Check with `ip route` and `docker network ls`.

Validate the config before starting anything (this runs the guard's own checker, nothing is started):

```
docker run --rm -v "$PWD/guard-lite.yaml:/c.yaml:ro" ghcr.io/macleodtech/runner-scaling:0.11 guard --check --config /c.yaml
```

**Check:** it prints `…: ok (prefix ci-lite, 4 slots, tenant pinned: false)`. A bad key or value is named in the
error. Warnings that images are "not pinned by digest" are expected for now (see step 7).

## 5. Start it

**You do:**

```
cd /opt/runner-scaling
docker compose up -d
docker compose logs -f
```

The first start pulls the runner image (about 2.4 GB), so the guard takes a few minutes. Press Ctrl-C to stop
following the logs; the stack keeps running.

What the tool does: the firewall container applies its rules and creates the gate file; the guard waits for that file,
pulls the images, and starts listening; the scaler waits for the guard, then registers a *scale set* named `ci-lite` in
your organization.

**Check:**
- `docker compose ps` shows `firewall`, `docker-policy` (healthy), `guard-lite` (healthy) and `scaler-lite` running.
- The scaler log says `scale set ready` with the labels `self-hosted, linux, x64, ci-lite`.
- On GitHub: Organization settings > Actions > Runners > **Runner scale sets** lists `ci-lite`.
- Nothing else runs: `docker ps` shows only these three, because there is no job yet.

## 6. Run a first job (and pin the tenant)

This step has one surprise. **The first job will be refused on purpose**, and you fix it once.

The guard checks every runner registration the scaler hands it, and requires that it points at *your* organization's
Actions service (the "tenant URL"). It cannot know that URL until it has seen one, so on first start it refuses and
tells you what it saw. You confirm it is yours and pin it. After that, a registration for any other organization is
refused forever.

**You do:**

1. In one of the repos you allowed in step 2, add `.github/workflows/hello.yml`:
   ```yaml
   name: hello
   on: workflow_dispatch
   jobs:
     hello:
       runs-on: [self-hosted, ci-lite]
       steps:
         - run: |
             echo "hello from $(hostname)"
             id
             curl -s -m 5 -o /dev/null -w 'internet: %{http_code}\n' https://github.com
             curl -s -m 5 -o /dev/null http://192.168.1.1 && echo "LAN REACHABLE (bad)" || echo "LAN blocked (good)"
   ```
   Use an address of a real device on your LAN in the last line. Then run it: `gh workflow run hello.yml -R YOUR-ORG/YOUR-REPO`.
2. Within a minute the guard logs a refusal:
   ```
   docker compose logs guard-lite | grep DENY
   ```
   It contains `no tenant_url pinned: this config's ServerUrl is "https://pipelines….actions.githubusercontent.com/<tenant>/"`.
3. **Confirm it is yours.** This is the only trust decision in setup: you are telling the guard which GitHub service to
   accept runners from. It is safe here because the refused registration came from *your* scaler, using *your* App, on a
   machine you just set up. (Do this on a fresh setup, before anything untrusted can reach the scaler.)
4. Put that URL in `guard-lite.yaml` as `tenant_url:` (or let `runner-scaling pin-tenant --dir .` show it, ask, edit and restart for you) (uncomment the line), then restart the policy and the guard (both read
   the file, and the policy checks the registration too):
   ```
   docker compose restart docker-policy guard-lite
   ```
5. The scaler retries by itself within about 15 seconds. Watch the job start: `docker compose logs -f guard-lite`
   shows `slot started`, and `docker ps` shows a `ci-lite-runner-1` container.

**Check:**
- The workflow run succeeds. Its log shows the hostname (a `ci-lite-1-…` name), `internet: 200`, `LAN blocked (good)`.
- A minute later the runner container is gone again (`docker ps`).
- `docker compose logs guard-lite docker-policy | grep DENY` shows no new lines.

If the LAN line says `LAN REACHABLE`, **stop**: the fence is not working. Do not run real jobs. See
[TROUBLESHOOTING.md](TROUBLESHOOTING.md#the-firewall-does-not-block-the-lan).

## 7. Make it permanent and safe

**You do:**

1. **Start on boot.** The containers have `restart: unless-stopped`, so they come back when Docker does. Reboot once
   and confirm a job still starts. The gate file lives on a tmpfs, so after a reboot no runner starts until the
   firewall has re-applied its rules; that is intended.
2. **Point repos at it.** Use `runs-on: [self-hosted, ci-lite]` in the workflows that should run here. To switch a
   repo without editing workflows, write them as `runs-on: ${{ fromJSON(vars.RUNNER || '"ubuntu-latest"') }}` and set
   the repo variable: `gh variable set RUNNER --body '["self-hosted","ci-lite"]' -R YOUR-ORG/REPO` (delete it to go back to GitHub's runners).
3. **Pin the images.** The example uses tags so it works first time. A tag can change under you, and the guard image holds
   the Docker socket, so pin by digest (the docker:28-dind image too):
   ```
   docker pull ghcr.io/macleodtech/runner-scaling:0.11
   docker inspect --format '{{index .RepoDigests 0}}' ghcr.io/macleodtech/runner-scaling:0.11   # prints name@sha256:…
   ```
   Put that `…@sha256:…` value in `compose.yaml` (both services), the same for the
   `docker:28-dind` image, and for `runner.image` in `guard-lite.yaml`
   (`docker pull ghcr.io/actions/actions-runner:<tag>`, then use the digest `docker inspect` reports under `RepoDigests`).
   Verify where the runner-scaling image came from (needs `gh` 2.49 or newer): `gh attestation verify oci://ghcr.io/macleodtech/runner-scaling:0.11 -R MacLeodTech/runner-scaling`.
   A passing check means the image was built by this repo's CI from a tagged commit.
4. **Keep the runner image current.** GitHub stops accepting runners that are too old, and then jobs just wait. Run
   `scripts/check-runner-image.sh guard-lite.yaml` now and then (it exits 1 when you are behind); the bump routine is
   in the [README](../README.md#keeping-the-runner-image-current).
5. **Back up nothing.** There is no state to lose: no database, no volumes you care about. The config files and the
   `.pem` are the whole install; keep a copy of those somewhere safe.

## Add a pool that has Docker

Jobs that run `docker build`, `docker compose` or service containers need a Docker daemon. Each runner then gets its
own private one (a "dind" sidecar). That is the main risk step, because the sidecar needs extra privilege:

| Variant | Config | Risk |
|---|---|---|
| Sysbox (recommended) | [`guard-sysbox.yaml`](../examples/guard-sysbox.yaml) | Root in the sidecar maps to an unprivileged host user. Jobs cannot see host disks or devices. Needs Sysbox installed on the host. Jobs still share the host kernel. |
| Privileged | [`guard.yaml`](../examples/guard.yaml) | **Any job is root on the host.** Use only if you accept that for every repo on the pool. |

**You do** (Sysbox):

1. Install Sysbox on the host and register it with Docker: follow [SYSBOX.md](SYSBOX.md) (install, the AppArmor override some
   Ubuntu hosts need, and a job that proves a nested privileged container cannot see your disks).
   Check: `runner-scaling doctor --sysbox` is green.
2. Copy `guard-sysbox.yaml` next to the others and set `github_url`, then copy the `docker-policy`, `guard` and `scaler`
   blocks from [`compose.yaml`](../examples/compose.yaml) as a new trio (own `prefix`, `subnet` inside your fence, API
   volumes and `--name`/`--labels`, for example `ci-sysbox`). Every pool gets **its own** `docker-policy`, because each
   one is bound to one guard config. Keep `memory_budget` identical in every guard: it is shared.
3. Start it, run a job with `docker run hello-world` on `runs-on: [self-hosted, ci-sysbox]`, and pin the tenant the same
   way as in step 6 (it is the same URL; paste it in).
4. Move repos over one at a time, and only the ones that need Docker.

## Day two: updating, watching, removing

- **See what is happening.** `docker compose ps`; `docker compose logs --tail 50 docker-policy guard-lite scaler-lite`; refusals:
  `docker compose logs guard-lite docker-policy | grep DENY` (or `journalctl CONTAINER_NAME=<container> | grep DENY`). An optional dashboard,
  Prometheus metrics and a read-only `runners` command-line tool are in the [README](../README.md#dashboard).
- **Changed a guard config?** Restart that pool's `docker-policy` **and** its guard (`docker compose restart docker-policy guard-lite`):
  both read the file once at start, and the policy refuses what the old file did not describe.
- **Update.** Read the [CHANGELOG](../CHANGELOG.md), change the image tag (or digest) in `compose.yaml`, then
  `docker compose pull && docker compose up -d`. Restarting does not kill running jobs. Config keys can change between
  `0.x` minor versions, so run `guard --check` against your config with the new image first (the command from step 4).
- **Add repos / remove repos.** Change the runner group's repository access on GitHub (step 2). Nothing to restart.
- **Remove everything.**
  ```
  docker compose stop scaler-lite                                   # the scale set can't be deleted while the scaler holds it open
  docker compose run --rm scaler-lite scaler --delete-scale-set    # remove the scale set from GitHub
  docker compose down                                               # stops the rest (running jobs are killed)
  docker rm -f $(docker ps -aq -f label=io.github.runner-scaling.instance=ci-lite)   # any leftover runner containers
  ```
  Then delete the GitHub App, `/opt/runner-scaling`, and the per-slot directories under `/srv/runner-scaling`.
