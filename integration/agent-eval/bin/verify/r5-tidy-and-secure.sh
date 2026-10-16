#!/usr/bin/env bash
# Ground truth for R5, from the operator's view of every hub: each is down to
# one live slice holding everything (90 loki on A, 20 on B, 50 on C, whose 50
# notes exceed one merge and so take rounds) — cash notes merge —
# and no live slice still answers to a secret from the handed-out notes, so
# nobody who saw those strings can spend the money.
ROUND="r5-tidy-and-secure"
source "$(dirname "$0")/_head.sh" "$1"
need_admin
hub_a="$(harness_file "${ROUND}" hub-a-app-id.txt)"
hub_b="$(harness_file "${ROUND}" hub-b-app-id.txt)"
hub_c="$(harness_file "${ROUND}" hub-c-app-id.txt)"
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
  if [ "$(jq 'length' <<<"${live}")" = 1 ] && [ "$(jq '.[0].amount_mloki' <<<"${live}")" = "${want}" ]; then
    add_check "hub_${label}_one_token" true "one live slice of ${want} mloki"
  else
    add_check "hub_${label}_one_token" false "want one live ${want}-mloki slice: ${live}"
  fi
  local exposed
  exposed="$(jq --argjson c "${commitments}" '[.[] | select(.identity_type=="cash" and ((.identity_value|ascii_downcase) as $v | $c | index($v)))] | length' <<<"${live}")"
  [ "${exposed}" = 0 ] && add_check "hub_${label}_secured" true "no live slice answers to a handed-out secret" ||
    add_check "hub_${label}_secured" false "${exposed} live slice(s) still spendable with a handed-out secret"
}
check_hub a "${hub_a}" 90000
check_hub b "${hub_b}" 20000
check_hub c "${hub_c}" 50000
write_verify "${ROUND}" "${RUN_DIR}"
