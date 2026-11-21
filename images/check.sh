#!/usr/bin/env bash
# Is the CI runner image stale? Exits 0 when current, 1 when something needs a rebuild, 2 on error.
#
#   images/check.sh --versions ci/tool-versions [guard.yaml]
#
# 1. each built image (runner-ci:lite, runner-ci:sysbox) was built from the current versions file and Dockerfile;
# 2. each guard config named runs the image it should (lite -> :lite, sysbox -> :sysbox tag family, or a digest of it);
# 3. newer upstream releases exist for Go (within the pinned minor), Node (within each pinned major), gosec and
#    gitleaks (information only: bumping is a deliberate edit of the
#    versions file, then a rebuild). The runner-version check stays in scripts/check-runner-image.sh.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
versions="" repo=${CI_IMAGE_REPO:-runner-ci}
while [ $# -gt 0 ] && [ "$1" = --versions ]; do versions=$2; shift 2; done
[ -f "$versions" ] || { sed -n '2,9p' "$0" >&2; exit 2; }
want=$(cat "$versions" "$here/Dockerfile" | sha256sum | cut -c1-8)
status=0
for t in lite sysbox; do
  have=$(docker image inspect -f '{{index .Config.Labels "ci.versions-hash"}}' "$repo:$t" 2>/dev/null || true)
  if [ -z "$have" ]; then echo "MISSING $repo:$t not built (images/build.sh --versions $versions)"; status=1
  elif [ "$have" != "$want" ]; then echo "STALE   $repo:$t built from $have, versions file now $want"; status=1
  else echo "ok      $repo:$t matches the versions file ($want)"; fi
done
for cfg in "$@"; do
  img=$(sed -nE 's/^[[:space:]]+image:[[:space:]]*([^[:space:]#]+).*/\1/p' "$cfg" | head -1)
  case $(basename "$cfg") in *lite*) t=lite ;; *sysbox*) t=sysbox ;; *) echo "$cfg: cannot tell which pool this is" >&2; exit 2 ;; esac
  if [[ $img == $repo:$t* || $img == */$repo:$t* ]]; then echo "ok      $cfg runs $img"
  else echo "OLD     $cfg runs $img, not the $t image"; status=1; fi
done
get() { grep -E "^$1=" "$versions" | head -1 | cut -d= -f2-; }
latest_gh() { gh api "repos/$1/releases/latest" --jq .tag_name 2>/dev/null | sed 's/^v//' || true; }
for pair in "gosec:securego/gosec:GOSEC_VERSION" "gitleaks:gitleaks/gitleaks:GITLEAKS_VERSION"; do
  IFS=: read -r name repo_gh key <<<"$pair"; cur=$(get "$key"); new=$(latest_gh "$repo_gh")
  [ -n "$cur" ] && [ -n "$new" ] && { [ "$cur" != "$new" ] && echo "newer   $name $new is out (versions file: $cur)" || echo "ok      $name $cur is the newest release"; }
done
# Go: newest release inside the pinned minor (1.26.x), not the newest overall; a new minor is reported separately.
go_cur=$(get GO_VERSION)
if [ -n "$go_cur" ]; then
  minor=${go_cur%.*}
  all=$(curl -fsS 'https://go.dev/dl/?mode=json&include=all' 2>/dev/null | grep -oE '"version": *"go[0-9]+\.[0-9]+(\.[0-9]+)?"' | sed -E 's/.*"go([^"]+)"/\1/' || true)
  in_minor=$(printf '%s\n' "$all" | grep -E "^${minor//./\\.}\.[0-9]+$" | sort -V | tail -1 || true)
  newest=$(printf '%s\n' "$all" | grep -E '^[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -1 || true)
  [ -n "$in_minor" ] && [ "$in_minor" != "$go_cur" ] && echo "newer   go $in_minor is out in the pinned $minor line (versions file: $go_cur)"
  [ -n "$newest" ] && [ "${newest%.*}" != "$minor" ] && echo "info    go $newest starts a newer minor than the pinned $minor (not a staleness problem)"
  [ -n "$in_minor" ] && [ "$in_minor" = "$go_cur" ] && echo "ok      go $go_cur is the newest $minor release"
fi
# Node: newest release within each pinned major.
idx=$(curl -fsS https://nodejs.org/dist/index.json 2>/dev/null || true)
for key in NODE_VERSION NODE_OLD_VERSION; do
  cur=$(get $key); [ -n "$cur" ] || continue; maj=${cur%%.*}
  new=$(printf '%s' "$idx" | grep -oE "\"version\": *\"v$maj\.[0-9]+\.[0-9]+\"" | sed -E 's/.*"v([^"]+)"/\1/' | sort -V | tail -1 || true)
  if [ -n "$new" ] && [ "$new" != "$cur" ]; then echo "newer   node $new is out in the pinned $maj line (versions file: $cur)"
  elif [ -n "$new" ]; then echo "ok      node $cur is the newest $maj release"; fi
done
exit "$status"
