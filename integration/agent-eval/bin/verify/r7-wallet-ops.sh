#!/usr/bin/env bash
# Ground truth for R7, asked of wallet `a` itself and of the agent's config: `a`
# received a settled 21-loki payment, and is now the default wallet.
ROUND="r7-wallet-ops"
source "$(dirname "$0")/_head.sh" "$1"
txs="$(agent_exec 'cashctl -c "$(cat /fixtures/r7-wallet-ops-a-uri.txt)" wallet list-tx --json 2>/dev/null')"
paid="$(jq '[(.transactions // .)[]? | select((.type=="incoming") and ((.state // "settled")=="settled") and ((.amount // .amount_mloki // 0)==21000))] | length' <<<"${txs}" 2>/dev/null)"
[ "${paid:-0}" -ge 1 ] && add_check "a_received_21_loki" true "settled incoming 21-loki transaction on a" ||
  add_check "a_received_21_loki" false "no settled incoming 21-loki transaction on a: ${txs}"
default="$(agent_exec "cashctl wallet show --json" | jq -r '.default_wallet // empty')"
[ "${default}" = a ] && add_check "a_is_default" true "default_wallet=a" ||
  add_check "a_is_default" false "default_wallet=${default:-<none>}, want a"
write_verify "${ROUND}" "${RUN_DIR}"
