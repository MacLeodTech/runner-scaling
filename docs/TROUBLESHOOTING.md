# Troubleshooting

Find your symptom, read the cause, do the fix. First, the three commands that answer most questions (run them from the
directory that holds your `compose.yaml`; with the example file the services are `firewall`, `docker-policy`, `guard` and `scaler`):

```
docker compose ps                                          # is everything up and healthy?
docker compose logs --tail 80 docker-policy guard scaler   # what did they last say?
docker compose logs guard docker-policy | grep -E 'DENY|ERROR'  # what was refused, and why?
```

If you log to journald (the example does), logs also survive a `docker compose down`:
`journalctl CONTAINER_NAME=<container> | grep DENY`.

- [Nothing starts](#nothing-starts)
- [A job waits forever](#a-job-waits-forever)
- [Runners start and fail](#runners-start-and-fail)
- [The firewall does not block the LAN](#the-firewall-does-not-block-the-lan)
- [Memory and disk](#memory-and-disk)
- [Message reference](#message-reference)
- [Still stuck](#still-stuck)

First, run `runner-scaling doctor --dir <your stack directory>` (add `--github` to also check the App). It checks the host, the files and the
runner group, and prints the fix beside every problem; most entries below are what it finds.

## Nothing starts

**`docker compose up` fails to pull the image (`denied` or `unauthorized`).** The image is public at
`ghcr.io/macleodtech/runner-scaling`. Check the tag exists (`0.10` or newer) and that you are not logged in to ghcr with an
expired token (`docker logout ghcr.io`, then retry).

**The guard exits at start with `field … not found in type …` (an unknown config key).** Your config uses a key this image version
does not know. The guard rejects unknown keys on purpose. Use the example files from the same version as your image, or
update the image. Validate any config without starting anything:
`docker run --rm -v "$PWD/guard.yaml:/c.yaml:ro" ghcr.io/macleodtech/runner-scaling:0.12 guard --check --config /c.yaml`.

**The guard exits with `unset environment variables: NAME`.** The config contains `${NAME}` and the guard container has
no such variable. Add it to the guard's `environment:` or remove the reference.

**The guard says `work_dir must not be under /tmp when dind is used`.** `docker:dind` mounts a tmpfs over `/tmp`, which
would hide the shared work directory from the job. Use another path, such as `/home/runner/_work`.

**The guard says `network "…" exists but is not on subnet …; remove it so it can be recreated`.** A Docker network
with the guard's name already exists on a different range, so it would sit outside the firewall's fence. Remove it
(`docker network rm <name>`, after removing any container on it) and let the guard recreate it.

**`docker-policy` exits or is unhealthy: `permission denied` on `/var/run/docker.sock`.** It runs as an unprivileged user and
reaches the socket through the group in `DOCKER_GID`. Set it in `.env` to the socket's real group: `stat -c %g /var/run/docker.sock`,
then `docker compose up -d`. (A rootless or remapped Docker puts the socket elsewhere; mount that path instead.)

**The guard says `docker …: 403 … runner-scaling docker policy: …`, or `docker-policy` logs `DENY`.** The policy refused a call.
The text after `why=` says which field or call. Normal operation never triggers it, so first check what changed:
- *you edited `guard.yaml` (or pinned `tenant_url`) and only restarted one container.* Both read the file once at start:
  `docker compose restart docker-policy guard`.
- *the guard and the policy see different files.* Both must mount the same config.
- *the guard image is newer than the policy's image,* so it sends something the old policy does not know. Run both at the same version.
- otherwise something other than the guard is talking to the policy socket, or the guard is misbehaving: **investigate before changing anything.**

**The scaler repeats `waiting for guard`.** The guard is still pulling images (normal on first start; the runner image is
about 2.4 GB) or it is unhealthy. Watch `docker compose logs -f guard`.

**The scaler exits with `no GitHub credentials: set GITHUB_APP_* or GITHUB_TOKEN`.** It found neither the App settings
nor a token. Check `.env` has `GITHUB_APP_CLIENT_ID` and `GITHUB_APP_INSTALLATION_ID`, that the key file exists inside the
container (`docker compose exec scaler-lite ls` will not work in a distroless image; check the file on the host and its
mount instead), and that the key file is readable by uid 65532: `sudo ls -ln secrets/github_app.pem` shows owner `65532`, mode `-r--------`.

**The scaler fails with 401 or 404 from GitHub.** Common causes: the App is not installed on the organization, the
installation ID is wrong, the App lacks **Organization permissions > Self-hosted runners: Read and write**, the
private key is for a different App, or the host clock is wrong (`timedatectl`; the App's signed tokens are rejected when
the clock is off by more than a few minutes).

**The scaler says `another session is open for this scale set; retrying in 30s`.** A previous scaler's connection has not
timed out yet. It resolves itself within a minute or two. If it never does, you are running two scalers with the same
`--name`: stop one.

## A job waits forever

Work through these in order; each is a real cause of "Queued" that never starts.

1. **Labels do not match.** The workflow's `runs-on` must contain every label the scale set has, and the scale set must
   exist. Compare `runs-on: [self-hosted, ci-lite]` with the scaler's `scale set ready … labels=[…]` log line.
2. **The repo cannot use the runners.** Organization settings > Actions > Runner groups > Default: the repository must
   be in the selected list, and public repos are blocked by design.
3. **The first job was refused for the tenant.** `docker compose logs guard | grep DENY` shows
   `no tenant_url pinned: this config's ServerUrl is "…"`. This is expected once: copy that URL into `tenant_url:` in
   `guard.yaml`, then `docker compose restart guard`. See [QUICKSTART step 6](QUICKSTART.md#6-run-a-first-job-and-pin-the-tenant).
4. **The tenant was pinned wrongly.** The DENY says `.runner ServerUrl "…" is not under the pinned tenant "…"`. The URL changed
   (or you pinned the wrong one). If the log's URL is still your own organization's, update `tenant_url`.
5. **The firewall gate is missing.** The DENY or 503 says `…/firewall-ok is missing (is the firewall up?)`. The firewall
   container must be running and healthy: `docker compose logs firewall`. See [below](#the-firewall-does-not-block-the-lan).
6. **The memory budget is full.** The 503 says `memory budget: X in use of Y, need Z reserve`. New runners wait until
   running ones finish (they are retried automatically). If it is always full, lower the per-runner `memory` or `max_slots`, or raise
   `memory_budget`. If it says "the host has … free and keeps … for other services", something else on the host is using the memory
   (`memory_host_headroom`).
7. **The runner image is too old.** GitHub refuses runners past a version cutoff, and jobs just wait. Run
   `scripts/check-runner-image.sh guard.yaml` and bump the image (README, "Keeping the runner image current").
8. **All slots are busy.** `docker ps` shows `max_slots` runners. That is the limit; jobs queue behind them.

## Runners start and fail

**The runner container exits immediately.** Look at its log while it exists:
`docker logs ci-lite-runner-1`. Common causes: the runner image failed to pull (check the guard log), a bind-mount path
is not writable (the guard creates missing host directories as root; make sure the parent exists on a filesystem with space),
or a limit in the config is too low (`memory` under about 1 GB, `pids` too small).

**`slot start failed, tearing down` in the guard log.** The text after `err=` is the reason. For a dind pool it is usually
dind failing its health check: wrong `runtime:` name (`docker info --format '{{json .Runtimes}}'` must list it), Sysbox not running
(`systemctl status sysbox`), or the daemon refusing a `privileged` or `runtime` combination.

**Jobs fail pulling images, or cannot resolve names.** The fence blocks private ranges, so a LAN DNS server (for example a
Pi-hole at 192.168.x.x) is unreachable from runners. The lite example sets public resolvers (`dns: [1.1.1.1, 9.9.9.9]`);
do the same for yours, or run the pool with a resolver on a public address.

**A job needs something on your LAN (an artifact server, NAS, internal registry).** By design it cannot reach it. Put the
service on a public address. Decide this deliberately: every hole you open is reachable by every job.

**Docker commands inside a job fail on the no-Docker pool.** That pool has no Docker. Use a Docker pool
([QUICKSTART](QUICKSTART.md#add-a-pool-that-has-docker)) for those repos.

**`caches of slot N could not be emptied`.** The guard refuses a slot until its cache directories could be wiped (a stale
file it cannot delete). Check the directory's ownership and free space under `/srv/runner-scaling/…`, and the guard log line above it.

## The firewall does not block the LAN

Do not run real jobs until this is fixed: it is the control that keeps a job away from your LAN and the host.

1. Is the container up? `docker compose ps firewall` and `docker compose logs firewall`. A healthy run logs
   `firewall in place; runners allowed` or `firewall applied; runners allowed`.
2. `fence apply FAILED` in its log: the container needs `network_mode: host` and `NET_ADMIN`, and a kernel with nf_tables
   (`sudo nft list tables`). On a host with only legacy iptables use `examples/firewall-iptables.sh`.
3. The rules exist but the test still reaches the LAN: check the fence covers your slot networks. `FENCE_SRC` in
   `compose.yaml` must contain every guard's `subnet`. `docker network inspect ci-lite-slot-1` shows the real range.
   `docker run --rm --net host --cap-add NET_ADMIN <image> firewall --src <your range> --check` says whether the installed rules match.
4. Show what is installed: `sudo nft list table inet gh_runners` (counters show what was dropped).
5. IPv6: if you enabled IPv6 on the runner networks (`ipv6: true`), set `FENCE_SRC6` to their range; otherwise runners may reach IPv6 LAN
   addresses. The sidecar warns when the host forwards IPv6 and `FENCE_SRC6` is empty.
6. If the rules are deleted while jobs run (a firewall reload, `nft flush ruleset`), the sidecar notices within its interval
   (3 seconds) and puts them back; `runner_fence_reapplied_total` counts it. The guard logs `GATE WITHDRAWN` once and counts
   `gate_withdrawn_total`. Alert on both. Set `gate_loss: stop` in the guard config to remove running slots instead of only warning.
   Note `nft flush ruleset` and `iptables-restore` of a whole ruleset delete our table too; the sidecar restores it.

## Memory and disk

**The host is slow or swapping.** Lower `memory_budget` and the per-runner `memory`, and set `memory_host_headroom` (for example
`3G`) so new runners wait while something else on the host uses its memory. See the README's
[Operations](../README.md#operations).

**A runner was killed in the middle of a job.** Either it passed its own `memory` limit (the job log ends abruptly; the
kernel OOM-killed it: raise `memory`), or the guard reaped the newest runner because total use stayed over `memory_budget`
(`reason=memory_budget` in the guard log), or it hit `max_runtime`. Add `memory_high` (README, "Slow a job down instead of killing
it") to slow a job instead of killing it.

**The disk fills up.** Runner and dind images, the work volumes and the warm caches all live under Docker's data directory and
`/srv/runner-scaling`. Set `cache_max` (for example `10G`) so kept caches are capped, and keep the dind prune settings
(`prune_on_stop`) from the Sysbox example. `docker system df` shows what Docker holds.

## Message reference

| You see | It means | Do |
|---|---|---|
| `DENY … no tenant_url pinned` | first run: the guard shows the URL it will trust | confirm it is yours, pin it, restart the guard |
| `DENY … is not under the pinned tenant` | the registration points somewhere other than the pinned tenant | if the URL is your org's, update `tenant_url`; if not, **investigate**: something other than your scaler is talking to the guard |
| `DENY … AgentName is …, want …` | the registration's runner name differs from the one requested | a bug or tampering; report it if it persists |
| `503 … is missing (is the firewall up?)` | the firewall gate file is absent | start or fix the firewall container |
| `503 … memory budget: … in use of …` | the memory budget is full | wait, or raise the budget / lower limits |
| `409 … slot N is in use / starting` | a race between the scaler and the guard | harmless; it retries |
| `docker-policy … DENY … differs from the guard's template at /…` | a create request differs from what the config builds, at the named field | see [above](#nothing-starts); if you changed nothing, investigate |
| `docker-policy … DENY … is not an API call the guard makes` | something asked Docker for a call outside the guard's list | investigate who can reach the policy socket |
| `DENY … no such endpoint` | something called the guard with an unknown request | investigate who has access to the guard socket |
| `GATE WITHDRAWN` | the firewall gate vanished while runners ran | check the firewall container immediately |
| `warning: … image … is not pinned by digest` | you use a tag | fine at first; pin digests ([QUICKSTART step 7](QUICKSTART.md#7-make-it-permanent-and-safe)) |
| `notice: N slots may use up to X together against memory_budget Y` | the slot limits add up to more than the shared budget | normal: the guard holds new slots back when it is full |

## Still stuck

Open an issue at <https://github.com/MacLeodTech/runner-scaling/issues> with your image tag (`docker compose images`), the
guard and scaler logs from the time of the problem **with secrets removed**, and your `guard.yaml` with
the organization name replaced. Do not post `.env`, the `.pem`, or the full JIT config. For a suspected security problem use the
private route in [SECURITY.md](../SECURITY.md) instead.
