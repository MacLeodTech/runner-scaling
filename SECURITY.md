# Security policy

## Reporting a vulnerability

Please report it privately through GitHub: the **Security** tab, then **Report a vulnerability**. Don't open a
public issue for it. Include what you found, how to reproduce it, and what it lets an attacker do.

This is a small project maintained by one person, so I can't promise a fixed response time, but a report that shows a
way to get past the guard's checks gets priority.

## What counts

The security design is in the README's [threat model](README.md#threat-model). In short, the guard's job is to
limit what someone who has taken over the *scaler* can do: create containers of their own design, mount host paths,
point a runner at another server or org, or reach the Docker API in any other way. A way around that is a
vulnerability.

These are known and documented, so they are not vulnerabilities on their own:

- With a `dind` sidecar, a job is root on the host.
- Jobs can read their runner's credentials, and a job on a trusted ref can share caches with other jobs.
- Whoever holds the scaler's GitHub credential can register runners and run workflows of their own.
