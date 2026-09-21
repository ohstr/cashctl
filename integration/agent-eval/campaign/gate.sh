#!/usr/bin/env bash
# Coverage verdict for a campaign dir built by setup.sh. Extra args (e.g.
# --waivers waivers.json --out gaps.json) pass through to gate.py.
set -euo pipefail
CAMP=$(cd "${1:?usage: gate.sh <campaign-dir> [gate.py args]}" && pwd)
shift
HERE=$(cd "$(dirname "$0")" && pwd)
logs=()
for f in "$CAMP"/logs/*.jsonl; do [ -e "$f" ] && logs+=(--log "$f"); done
[ ${#logs[@]} -gt 0 ] || { echo "no logs under $CAMP/logs yet" >&2; exit 2; }
exec python3 "$HERE/gate.py" --surface "$CAMP/surface.json" "${logs[@]}" "$@"
