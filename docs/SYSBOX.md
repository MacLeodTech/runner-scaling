# Installing Sysbox for the Docker pool

A pool whose jobs run `docker build` or service containers needs a Docker daemon per runner. Run it under
[Sysbox](https://github.com/nestybox/sysbox) and it is **not privileged**: root inside the job's Docker maps to an
unprivileged user on the host, and the job cannot see your disks or devices. The alternative (`privileged: true`) makes
every job root on the host. Read [SECURITY-POSTURE.md](SECURITY-POSTURE.md) for what Sysbox does and does not protect.

> **How tested this is.** The steps below were done by hand on one host: Ubuntu 26.04 (kernel 7.0), Docker 29, Sysbox CE
> 0.7.1. Other distributions and versions are expected to work if Sysbox supports them, but nobody has checked here. If a
> step differs for you, [TROUBLESHOOTING.md](TROUBLESHOOTING.md) has the symptoms, and a pull request with the difference is welcome.

## 1. Check your host can run it

> **Ubuntu 22.04 LTS.** Its `docker.io` package is Docker 24 (API 1.43), which is too old for the guard (it needs API 1.44, Docker 25
> or newer): install `docker-ce` and the Compose plugin from [Docker's apt repository](https://docs.docker.com/engine/install/ubuntu/)
> first, and `doctor` will say so if you did not. 22.04's kernel (5.15, or 6.x with the HWE stack), cgroup v2 and nftables are all
> enough for the fence, and 22.04 is on Sysbox's supported list. This tool's own steps have not been run on 22.04 by the author: tell
> us what differs.

```
runner-scaling doctor --sysbox
```

Sysbox needs a supported distribution and kernel (see its [distro compatibility list](https://github.com/nestybox/sysbox/blob/master/docs/distro-compat.md)),
a normal rootful Docker, and no other container runtime hooks fighting it. Until Sysbox is installed, `doctor` reports
`FAIL sysbox runtime`: that is the line this guide turns green.

## 2. Install it

**You do** (needs sudo; it restarts Docker, which stops running containers, so do it at a quiet time):

1. Download the `.deb` for your architecture from Sysbox's [releases page](https://github.com/nestybox/sysbox/releases) and verify its checksum
   as that page describes (other distributions: [their install guide](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/install-package.md)).
2. `sudo apt-get install ./sysbox-ce_*.deb`. The package registers the `sysbox-runc` runtime with Docker and starts two host
   services, `sysbox-mgr` and `sysbox-fs`. Docker now depends on them for Sysbox containers.
3. Check:
   ```
   docker info --format '{{json .Runtimes}}'     # lists sysbox-runc
   systemctl is-active sysbox-mgr sysbox-fs       # active, active
   docker run --rm --runtime sysbox-runc alpine:3 cat /proc/self/uid_map   # a non-identity map, e.g. 0 165536 65536
   ```

### If Sysbox containers fail to start on Ubuntu

On the reference host (a recent Ubuntu with AppArmor), `sysbox-fs` could not mount its per-container directories until
this local override was added. You may not need it; add it only if starting a Sysbox container fails with an AppArmor
denial in `journalctl -k | grep -i apparmor`:

```
# /etc/apparmor.d/local/fusermount3
mount fstype=@{fuse_types} options=(nosuid,nodev) options in (ro,rw,noatime,dirsync,nodiratime,noexec,sync) -> /var/lib/sysboxfs/**/,
umount /var/lib/sysboxfs/**/,
```

then `sudo apparmor_parser -r /etc/apparmor.d/fusermount3` (or reload AppArmor) and restart `sysbox-fs`.

### Address pools

Docker hands out `172.17.0.0/16` and the following ranges to networks it creates itself. The guard gives every slot a
fixed subnet inside the fenced range (`init` picks a free one), so a clash only happens if something else already owns it.
`runner-scaling doctor --dir <stack>` reports such an overlap. If you must keep Docker's own networks away from a range, set
`default-address-pools` (and `bip`) in `/etc/docker/daemon.json` and restart Docker, as the reference host does:

```json
{ "bip": "172.17.0.1/16", "default-address-pools": [{ "base": "172.24.0.0/14", "size": 24 }] }
```

## 3. Add the pool

Either set the stack up with it from the start (`runner-scaling init --pools lite,sysbox`), or add the `sysbox` block of
[`examples/guard-pools.yaml`](../examples/guard-pools.yaml) to the `pools:` of your `guard.yaml` and run
`docker compose restart docker-policy guard scaler` (QUICKSTART, "Add a pool that has Docker"). No new containers are needed: the
one guard, one `docker-policy` and one scaler serve every pool. The pool's dind section
already says `runtime: sysbox-runc` and is not privileged. The policy proxy refuses any container that deviates, so a
config that quietly asks for `privileged: true` is the one thing to review.

## 4. Prove it works, and that it contains a job

Run a job on the new pool (`runs-on: [self-hosted, sysbox]`) with these steps. The last one must **fail** to touch the host:

```yaml
- run: |
    docker run --rm hello-world
    printf 'FROM alpine:3\nRUN echo built > /x\nCMD cat /x\n' > Dockerfile
    docker build -q -t t:1 . && docker run --rm t:1          # prints "built"
    # an inner container that asks for everything still has no view of the host:
    docker run --rm --privileged -v /:/host alpine:3 sh -c \
      'cat /proc/self/uid_map; ls /host/home; ls /dev | grep -cE "^(sd|nvme|dm-)"'
```

Expected: `hello-world` and the build work; the last command shows a non-identity `uid_map`, an empty `/host/home` (the
*runner's* root, not yours) and **0** block devices. If it lists your real `/home` or disks, the pool is not running under
Sysbox: check `runtime:` in the guard config and `docker inspect <dind container> --format '{{.HostConfig.Runtime}}'`.

## Known limits

- Sysbox narrows the common escapes; the **kernel is still shared**. A kernel bug is a host compromise. The guard's
  `module_blocklist` stops jobs making the host autoload kernel modules (a path that exists with or without Sysbox).
- Jobs that need KVM, kernel modules or writes to `/proc/sys` cannot run here. Use GitHub-hosted runners for them.
- If `sysbox-mgr` or `sysbox-fs` stops, only this pool breaks (its slots fail to start); the no-Docker pool does not use them.
- Behaviour across a **reboot** has not been tested on the reference host: after one, check `systemctl is-active sysbox-mgr sysbox-fs` before blaming the guard.
