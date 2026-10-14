#!/usr/bin/env bash
# Ground truth for R1, from the operator's view of the minting hub: no slice is
# left unclaimed and at least one ended `redeemed`. Slice status rather than
# the first claim row, because receive's auto-protect re-keys the note and can
# leave the original row `split` (I-4).
ROUND="r1-cash-note"
source "$(dirname "$0")/_head.sh" "$1"
need_admin
hub="$(harness_file "${ROUND}" hub-app-id.txt)"
slices="$(hub_slices "${hub}")"
unclaimed="$(jq '[.[] | select(.status=="unclaimed")] | length' <<<"${slices}")"
redeemed="$(jq '[.[] | select(.status=="redeemed")] | length' <<<"${slices}")"
[ "${unclaimed}" = 0 ] && add_check "nothing_left_unclaimed" true "0 unclaimed slices" ||
  add_check "nothing_left_unclaimed" false "${unclaimed} unclaimed: ${slices}"
[ "${redeemed}" -ge 1 ] && add_check "money_redeemed" true "${redeemed} redeemed slice(s)" ||
  add_check "money_redeemed" false "no redeemed slice: ${slices}"
write_verify "${ROUND}" "${RUN_DIR}"
