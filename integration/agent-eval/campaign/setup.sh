#!/usr/bin/env bash
# Builds the campaign toolchain and gives each agent its own PATH dir with a
# logging `cashctl` and a quota-enforcing `mint`.
#
#   setup.sh <campaign-dir> <agent> [agent...]
#
# Layout under <campaign-dir>:
#   bin/cashctl-real bin/mint-real   built from the current working tree
#   surface.json                     required-coverage list (surface-dump)
#   logs/<agent>.jsonl               shim log, one line per cashctl call
#   state/                           mint state + lock (quotas, sweep manifest)
#   agents/<agent>/bin/{cashctl,mint}  put this dir first on PATH
#
# Then: gate.sh <campaign-dir>   (coverage verdict), mint sweep to clean up.
set -euo pipefail

[ $# -ge 2 ] || { echo "usage: setup.sh <campaign-dir> <agent> [agent...]" >&2; exit 2; }
CAMP=$(mkdir -p "$1" && cd "$1" && pwd)
shift
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../../.." && pwd) # repo root (integration/agent-eval/campaign -> ..)
export GOWORK=off

mkdir -p "$CAMP/bin" "$CAMP/logs" "$CAMP/state"
( cd "$ROOT" \
  && go build -o "$CAMP/bin/cashctl-real" . \
  && go build -o "$CAMP/bin/mint-real" ./integration/agent-eval/mint \
  && go run ./integration/agent-eval/surface-dump > "$CAMP/surface.json" )

for A in "$@"; do
  [[ "$A" =~ ^[A-Za-z0-9_-]{1,32}$ ]] || { echo "bad agent name: $A" >&2; exit 2; }
  D="$CAMP/agents/$A/bin"
  mkdir -p "$D"
  cat > "$D/cashctl" <<EOF
#!/usr/bin/env bash
export CASHCTL_REAL="$CAMP/bin/cashctl-real" CASHCTL_LOG="$CAMP/logs/$A.jsonl" \\
       CASHCTL_SURFACE="$CAMP/surface.json" CASHCTL_AGENT="$A"
exec python3 "$HERE/cashctl-shim" "\$@"
EOF
  cat > "$D/mint" <<EOF
#!/usr/bin/env bash
# --agent is appended last so it wins over anything the caller passes.
export MINT_CONFIG="$ROOT/integration/config.local.yaml" MINT_STATE="$CAMP/state" MINT_RUN="$(basename "$CAMP")"
sub="\${1:?usage: mint token|invoice|nwc-wallet|circle-hub|status|sweep ...}"; shift
exec "$CAMP/bin/mint-real" "\$sub" "\$@" --agent "$A"
EOF
  chmod +x "$D/cashctl" "$D/mint"
  echo "agent $A: PATH=$D:\$PATH"
done
echo "surface: $(python3 -c "import json;print(len(json.load(open('$CAMP/surface.json'))['commands']))") command paths -> $CAMP/surface.json"
