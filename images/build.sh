#!/usr/bin/env bash
# Build the CI runner image family locally from a versions file.
#
#   images/build.sh --versions ci/tool-versions [--target lite|sysbox|all] [--dry-run]
#
# Tags: runner-ci:<target>-<YYYYMMDD>-<hash> (hash = first 8 hex of sha256 over the versions file and the Dockerfile)
# and the moving runner-ci:<target>. Nothing is pushed and no pool is touched: to use an image, put its tag (and, once
# it is in a registry, its digest) in the guard config's runner `image`, run `autoscale.sh check`, restart that guard.
# Never run this from, or point, runner-scaling's own CI at these pools.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
versions="" target=all dry=0 repo=${CI_IMAGE_REPO:-runner-ci}
while [ $# -gt 0 ]; do
  case $1 in
    --versions) versions=$2; shift 2 ;;
    --target) target=$2; shift 2 ;;
    --dry-run) dry=1; shift ;;
    *) sed -n '2,10p' "$0" >&2; exit 2 ;;
  esac
done
[ -f "$versions" ] || { echo "need --versions <file> (see images/tool-versions.example)" >&2; exit 2; }
case $target in lite|sysbox) targets=$target ;; all) targets="lite sysbox" ;; *) echo "unknown target $target" >&2; exit 2 ;; esac

# The file is data, not shell: accept only KEY=value lines and pass each as a build arg.
okline='^[A-Z][A-Z0-9_]*=[A-Za-z0-9._:@/+,=-]*( [A-Za-z0-9._:@/+,=-]+)*$'
args=()
while IFS= read -r line || [ -n "$line" ]; do
  line=${line%%#*}; line=$(printf '%s' "$line" | sed -E 's/[[:space:]]+$//; s/^[[:space:]]+//')
  [ -n "$line" ] || continue
  [[ $line =~ $okline ]] ||
    { echo "$versions: not a KEY=value line: $line" >&2; exit 2; }
  args+=(--build-arg "$line")
done <"$versions"
printf '%s\n' "${args[@]}" | grep -q '^BASE_IMAGE=' || { echo "$versions: BASE_IMAGE is required" >&2; exit 2; }

hash=$(cat "$versions" "$here/Dockerfile" | sha256sum | cut -c1-8)
stamp=$(date -u +%Y%m%d)
for t in $targets; do
  tag="$repo:$t-$stamp-$hash"
  cmd=(docker build --target "$t" "${args[@]}" --label "ci.versions-hash=$hash" --label "ci.built=$stamp" -t "$tag" -t "$repo:$t" -f "$here/Dockerfile" "$here")
  if [ "$dry" = 1 ]; then printf '%q ' "${cmd[@]}"; echo; continue; fi
  "${cmd[@]}"
  echo "built $tag  $(docker image inspect -f '{{.Id}}  {{.Size}} bytes' "$tag")"
done
