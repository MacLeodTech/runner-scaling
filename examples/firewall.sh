#!/bin/sh
# Firewall sidecar: fences the runner networks so a job can reach the internet but not your LAN, other Docker
# networks or the host itself, then tells the guards it is safe to start runners.
#
#   - Rules live in their own chain, GH_RUNNERS, hooked into DOCKER-USER (traffic forwarded from the runners) and
#     INPUT (traffic aimed at the host). Everything from the fenced range to a private or link-local address is
#     dropped; the rest is returned to Docker's normal handling. Replies to connections the HOST opened still work.
#   - It then writes the gate file (GATE). Point every guard's `require_file` at it: no slot starts without it.
#   - Every 30s it re-checks the rules. Missing rules are re-applied; if that fails, the gate file is removed, so no
#     NEW slot starts unfenced. (Runners already running are not stopped: they are unfenced until the rules return.)
#   - The gate lives on a tmpfs volume, so after a reboot it is gone until this script has applied the rules again.
#
# Needs: host networking, NET_ADMIN and NET_RAW, and an iptables binary (the nf_tables variant is fine). With Docker's
# iptables backend the forwarded-traffic hook is DOCKER-USER. With Docker's nftables backend there is no DOCKER-USER, so
# the script hooks the FORWARD chain instead (Docker's rules are in a separate table; a drop here still applies).
# FENCE_SRC must contain every guard's `subnet` range.
#
# Remove the fence by stopping this container, then:
#   iptables -D DOCKER-USER -s "$FENCE_SRC" -j GH_RUNNERS   # or FORWARD, on the nftables backend
#   iptables -D INPUT -s "$FENCE_SRC" -m conntrack ! --ctstate ESTABLISHED,RELATED -j DROP
#   iptables -F GH_RUNNERS && iptables -X GH_RUNNERS
set -u
SRC=${FENCE_SRC:-172.30.0.0/16}
GATE=${GATE:-/gate/firewall-ok}
PRIVATE="10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 169.254.0.0/16 100.64.0.0/10"
# The fenced range sits inside 172.16.0.0/12, so this also cuts runners off from each other and from every other
# Docker network on 172.16/12. If you fence a range outside the private ranges, add your LAN and other ranges here.

# Forwarded traffic is hooked into DOCKER-USER when Docker's iptables backend provides it. Docker's nftables backend
# has no such chain, but its rules live in their own table, so a drop in the iptables-nft FORWARD chain still applies.
hook() { if iptables -n -L DOCKER-USER >/dev/null 2>&1; then echo DOCKER-USER; else echo FORWARD; fi; }

input_rule() { iptables "$1" INPUT ${2:-} -s "$SRC" -m conntrack ! --ctstate ESTABLISHED,RELATED -j DROP; }

ok() {
  iptables -C "$(hook)" -s "$SRC" -j GH_RUNNERS 2>/dev/null || return 1
  input_rule -C 2>/dev/null || return 1
  for d in $PRIVATE; do
    iptables -C GH_RUNNERS -s "$SRC" -d "$d" -j DROP 2>/dev/null || return 1
  done
}

apply() {
  iptables -N GH_RUNNERS 2>/dev/null || true
  iptables -F GH_RUNNERS || return 1
  for d in $PRIVATE; do
    iptables -A GH_RUNNERS -s "$SRC" -d "$d" -j DROP || return 1
  done
  iptables -A GH_RUNNERS -j RETURN || return 1
  H=$(hook)
  iptables -C "$H" -s "$SRC" -j GH_RUNNERS 2>/dev/null || iptables -I "$H" 1 -s "$SRC" -j GH_RUNNERS || return 1
  input_rule -C 2>/dev/null || input_rule -I 1 || return 1
}

while true; do
  if ok; then
    [ -e "$GATE" ] || { touch "$GATE" && echo "$(date -Is) firewall in place; runners allowed"; }
  else
    rm -f "$GATE"
    echo "$(date -Is) firewall rules missing; applying"
    if apply && ok; then
      touch "$GATE" && echo "$(date -Is) firewall applied; runners allowed"
    else
      echo "$(date -Is) firewall apply FAILED; runners blocked" >&2
    fi
  fi
  sleep 30
done
