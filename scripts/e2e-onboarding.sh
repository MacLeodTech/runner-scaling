#!/bin/sh
# What a newcomer's first ten minutes produce, checked without GitHub: init writes a stack, and every tool that will
# read it accepts it. Needs Docker (for the socket's group and `docker compose config`) and a runner-scaling binary.
#   scripts/e2e-onboarding.sh ./runner-scaling
set -eu
RS=${1:?usage: e2e-onboarding.sh /path/to/runner-scaling}
D=$(mktemp -d); trap 'rm -rf "$D"' EXIT
openssl genrsa -out "$D/key.pem" 2048 2>/dev/null
"$RS" init --org example-org --client-id Iv1.e2etest --installation-id 4242 --key-file "$D/key.pem" --pools lite,sysbox --dir "$D/stack"

cd "$D/stack"
# 1. compose accepts the files as written 
docker compose config -q
# 2. the guard and the policy proxy accept the config, under the example's own environment
export USERDIR=/srv
"$RS" guard --check --config guard.yaml
"$RS" docker-policy --check --config guard.yaml
# 3. the fence flags the example uses parse, and the range is what compose says
fence=$(sed -n 's/^ *FENCE_SRC: *\([0-9./]*\).*/\1/p' compose.yaml)
test -n "$fence"
out=$("$RS" firewall --src "$fence" --check 2>&1 || true)   # without NET_ADMIN or the table it fails, but not on the flags
case "$out" in *"want an IPv4 network"*|*invalid*|*"no source range"*) echo "fence range rejected: $out"; exit 1;; esac
# 4. doctor: the only findings allowed are the key's owner (a non-root init cannot chown to 65532) and Sysbox (the sysbox pool
#    is in the stack on purpose, and a CI host has none)
"$RS" doctor --offline --dir . --json > "$D/doctor.json" || true
python3 - "$D/doctor.json" <<'PY'
import json, sys
bad = [c for c in json.load(open(sys.argv[1])) if c["status"] == "FAIL" and c["name"] not in ("app key owner", "sysbox runtime")]
if bad:
    print("unexpected doctor failures:", json.dumps(bad, indent=1)); sys.exit(1)
PY
# 5. init refuses to overwrite
if "$RS" init --org example-org --client-id Iv1.e2etest --installation-id 4242 --key-file "$D/key.pem" --dir "$D/stack" 2>/dev/null; then
  echo "init overwrote an existing stack"; exit 1
fi
echo "onboarding e2e: ok"
