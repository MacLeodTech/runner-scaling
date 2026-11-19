#!/usr/bin/env bash
# Is the CI runner image stale? Exits 0 when current, 1 when something needs a rebuild, 2 on error.
#
#   images/check.sh --versions ci/tool-versions [guard-lite.yaml guard-sysbox.yaml]
#
# 1. each built image (runner-ci:lite, runner-ci:sysbox) was built from the current versions file and Dockerfile;
# 2. each guard config named runs the image it should (lite -> :lite, sysbox -> :sysbox tag family, or a digest of it);
# 3. newer upstream releases exist for Go, gosec and gitleaks (information only: bumping is a deliberate edit of the
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
  [ -n "$cur" ] && [ -n "$new" ] && [ "$cur" != "$new" ] && echo "newer   $name $new is out (versions file: $cur)"
done
go_new=$(curl -fsS 'https://go.dev/dl/?mode=json' 2>/dev/null | sed -nE 's/.*"version": *"go([0-9.]+)".*/\1/p' | head -1 || true)
go_cur=$(get GO_VERSION); [ -n "$go_new" ] && [ -n "$go_cur" ] && [ "$go_new" != "$go_cur" ] && echo "newer   go $go_new is out (versions file: $go_cur)"
exit "$status"
