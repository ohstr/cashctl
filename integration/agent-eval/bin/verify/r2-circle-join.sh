#!/usr/bin/env bash
# Ground truth for R2: exactly one circle wallet exists under the hub, the Hub
# reports its budget as 500 loki, and a second join is refused as `exists`
# (exit 9) — the harness re-runs that join itself.
ROUND="r2-circle-join"
source "$(dirname "$0")/_head.sh" "$1"
need_admin
hub="$(harness_file "${ROUND}" hub-app-id.txt)"
token="$(harness_file "${ROUND}" hub-token.txt)"
kids="$(admin_api GET "/api/apps/${hub}/circle/children?limit=0" 2>/dev/null | jq '.children // [] | length')"
[ "${kids}" = 1 ] && add_check "one_circle_wallet" true "1 child under hub ${hub}" ||
  add_check "one_circle_wallet" false "${kids:-?} children under hub ${hub}, want exactly 1"
name="$(agent_exec "cashctl connect list --json" | jq -r '[.connections[]?.name | select(startswith("circle"))][0] // empty')"
budget="$(agent_exec "cashctl -c '${name}' wallet budget --json 2>/dev/null" | jq -r '.total_budget // empty')"
case "${budget}" in
  500000) add_check "cap_is_500" true "${name}: budget ${budget}" ;;
  *) add_check "cap_is_500" false "${name:-<no circle wallet>}: budget '${budget}', want 500 loki" ;;
esac
second="$(agent_exec "cashctl join '${token}' 500 --json 2>&1; echo exit=\$?")"
if grep -q 'exit=9' <<<"${second}" && grep -q '"code": *"exists"' <<<"${second}"; then
  add_check "second_join_refused_as_exists" true "exit 9, code exists"
else
  add_check "second_join_refused_as_exists" false "want exit 9 code exists: ${second}"
fi
write_verify "${ROUND}" "${RUN_DIR}"
