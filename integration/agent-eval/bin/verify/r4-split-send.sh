#!/usr/bin/env bash
# Ground truth for R4, from the operator's view: a 30-loki slice now belongs to
# P's pubkey, and the 60-loki remainder is an unclaimed cash-mode slice. The
# note the agent wrote must actually be takeable: the harness receives it into
# a throwaway config, which only succeeds if the Hub accepts its secret.
ROUND="r4-split-send"
source "$(dirname "$0")/_head.sh" "$1"
need_admin
hub="$(harness_file "${ROUND}" hub-app-id.txt)"
p_hex="$(jq -r '.hex' <<<"$(harness_file "${ROUND}" p.json)")"
slices="$(hub_slices "${hub}")"
to_p="$(jq --arg p "${p_hex}" '[.[] | select(.status=="unclaimed" and .identity_type=="pubkey" and .identity_value==$p and .amount_mloki==30000)] | length' <<<"${slices}")"
[ "${to_p}" = 1 ] && add_check "thirty_loki_to_p" true "P holds an unclaimed 30-loki slice" ||
  add_check "thirty_loki_to_p" false "no unclaimed 30-loki pubkey slice for P: ${slices}"
cash="$(jq '[.[] | select(.status=="unclaimed" and .identity_type=="cash" and .amount_mloki==60000)] | length' <<<"${slices}")"
[ "${cash}" = 1 ] && add_check "remainder_is_a_cash_note" true "one unclaimed 60-loki cash slice" ||
  add_check "remainder_is_a_cash_note" false "want one unclaimed 60-loki cash slice: ${slices}"
if [ -s report/r4-note.txt ]; then
  out="$(agent_exec "cashctl --config-dir /tmp/r4-verify receive \"\$(tr -d '[:space:]' < /report/r4-note.txt)\" --json --yes 2>&1; echo exit=\$?")"
  grep -q 'exit=0' <<<"${out}" && add_check "note_is_takeable" true "a fresh wallet received the reported note" ||
    add_check "note_is_takeable" false "receiving the reported note failed: ${out}"
else
  add_check "note_is_takeable" false "agent never wrote /report/r4-note.txt"
fi
write_verify "${ROUND}" "${RUN_DIR}"
