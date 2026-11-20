#!/usr/bin/env bash
# Compare the runner version inside each pinned runner image against the newest actions/runner release.
#
# The scale set is created with DisableUpdate: true, so a runner never updates itself. GitHub refuses runners that
# fall too far behind, and then jobs simply wait with no obvious error. Run this now and then (or from cron) and
# bump the image when it warns.
#
#   scripts/check-runner-image.sh guard.yaml [other-stack.yaml ...]
#
# Needs docker and either gh or curl. Exits 0 when current, 1 when behind, 2 on error.
set -euo pipefail
[ $# -ge 1 ] || { sed -n '2,12p' "$0" >&2; exit 2; }

latest=$(gh api repos/actions/runner/releases/latest --jq .tag_name 2>/dev/null ||
  curl -fsS https://api.github.com/repos/actions/runner/releases/latest | sed -nE 's/.*"tag_name": *"([^"]+)".*/\1/p') || true
latest=${latest#v}
[ -n "$latest" ] || { echo "cannot find the latest actions/runner release" >&2; exit 2; }

status=0
for cfg in "$@"; do
  # The runner image is the first `image:` under the container template (the dind sidecar comes later).
  image=$(sed -nE 's/^[[:space:]]+image:[[:space:]]*([^[:space:]#]+).*/\1/p' "$cfg" | head -1)
  [ -n "$image" ] || { echo "$cfg: no image found" >&2; exit 2; }
  have=$(docker run --rm --network none --entrypoint sh "$image" -c \
    '/actions-runner/bin/Runner.Listener --version 2>/dev/null || /home/runner/bin/Runner.Listener --version' | tail -1) ||
    { echo "$cfg: could not run Runner.Listener in $image" >&2; exit 2; }
  if [ "$have" = "$latest" ]; then
    echo "ok      $cfg: runner $have (latest)"
  else
    echo "BEHIND  $cfg: runner $have, latest is $latest ($image)"
    status=1
  fi
done
[ "$status" -eq 0 ] || echo "Bump: pick a newer image tag, \`docker pull\` it, copy its digest into every guard file's runner image, then \`autoscale.sh check\` and restart the guards." >&2
exit "$status"
