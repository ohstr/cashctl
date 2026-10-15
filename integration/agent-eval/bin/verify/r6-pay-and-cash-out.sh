#!/usr/bin/env bash
# Ground truth for R6, from the operator's view: a redeem pays a whole slice, so
# the 60-loki bill paid the 60-loki invoice, 24 + 16 went to the wallet, and the
# 30-loki bill is still held.
ROUND="r6-pay-and-cash-out"
source "$(dirname "$0")/_head.sh" "$1"
need_admin
hub="$(harness_file "${ROUND}" hub-app-id.txt)"
slices="$(hub_slices "${hub}")"
status_of() { jq -r --argjson a "$1" '[.[] | select(.amount_mloki==$a)][0].status // "missing"' <<<"${slices}"; }
for want in "60000 redeemed" "24000 redeemed" "16000 redeemed" "30000 unclaimed"; do
  set -- ${want}
  got="$(status_of "$1")"
  [ "${got}" = "$2" ] && add_check "bill_$1_$2" true "${got}" ||
    add_check "bill_$1_$2" false "the $1-mloki bill is ${got}, want $2"
done
write_verify "${ROUND}" "${RUN_DIR}"
