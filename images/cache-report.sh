#!/usr/bin/env bash
# Read-only: size of each kind of cache per slot under the cache root (default ~/runner-state/runner-scaling/cache).
# Run with sudo to see root-owned directories you cannot read (e.g. a sysbox slot's docker store).
set -u
root=${1:-$HOME/runner-state/runner-scaling/cache}
[ -d "$root" ] || { echo "no cache root at $root (the cache layout in guard-image.diff is not applied yet)"; exit 0; }
printf '%-10s %-8s %s\n' SLOT KIND SIZE
for slot in "$root"/*/; do
  s=$(basename "$slot")
  for k in "$slot"files/* "$slot"docker; do
    [ -e "$k" ] || continue
    printf '%-10s %-8s %s\n' "$s" "$(basename "$k")" "$(du -sh "$k" 2>/dev/null | cut -f1)"
  done
  printf '%-10s %-8s %s\n' "$s" TOTAL "$(du -sh "$slot" 2>/dev/null | cut -f1)"
done
