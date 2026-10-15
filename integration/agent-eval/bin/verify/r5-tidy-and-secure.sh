#!/usr/bin/env bash
# Ground truth for R5, from the operator's view of both hubs: every note was
# taken (the live slices still hold 90 loki on A and 20 on B) and no live
# slice still answers to a secret from the handed-out notes, so nobody who saw
# those strings can spend the money. Not checked: merging to one token per hub,
# which cashctl cannot do for cash-mode holdings today (protect re-keys to a
# fresh cash secret, and cash_consolidate takes only pubkey-mode sources).
ROUND="r5-tidy-and-secure"
source "$(dirname "$0")/_head.sh" "$1"
need_admin
hub_a="$(harness_file "${ROUND}" hub-a-app-id.txt)"
hub_b="$(harness_file "${ROUND}" hub-b-app-id.txt)"
secrets="$(harness_file "${ROUND}" secrets.txt)"
# A cash slice's identity_value is the hex SHA-256 of its secret's bytes.
commitments="$(python3 -c '
import hashlib, sys
for line in sys.stdin:
    line = line.strip()
    if line:
        print(hashlib.sha256(bytes.fromhex(line)).hexdigest())' <<<"${secrets}" | jq -R . | jq -sc .)"

check_hub() {
  local label="$1" hub="$2" want="$3" slices live
  slices="$(hub_slices "${hub}")"
  live="$(jq -c '[.[] | select(.status=="unclaimed")]' <<<"${slices}")"
  if [ "$(jq '[.[].amount_mloki] | add // 0' <<<"${live}")" = "${want}" ]; then
    add_check "hub_${label}_all_taken" true "live slices hold ${want} mloki in $(jq length <<<"${live}") token(s)"
  else
    add_check "hub_${label}_all_taken" false "want ${want} mloki live: ${live}"
  fi
  local exposed
  exposed="$(jq --argjson c "${commitments}" '[.[] | select(.identity_type=="cash" and ((.identity_value|ascii_downcase) as $v | $c | index($v)))] | length' <<<"${live}")"
  [ "${exposed}" = 0 ] && add_check "hub_${label}_secured" true "no live slice answers to a handed-out secret" ||
    add_check "hub_${label}_secured" false "${exposed} live slice(s) still spendable with a handed-out secret"
}
check_hub a "${hub_a}" 90000
check_hub b "${hub_b}" 20000
write_verify "${ROUND}" "${RUN_DIR}"
