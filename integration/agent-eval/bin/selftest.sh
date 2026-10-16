#!/usr/bin/env bash
# Proves each round's verifier both ways against the live lokihub, without an
# agent: with the round's fixtures provisioned but nothing done, the verifier
# must FAIL; after a scripted solution run inside the container, it must PASS.
# The self-report check is ignored (no agent writes one). r3's probes are
# re-run by its verifier itself, so it is checked on the pass side only.
#
# Usage: bin/selftest.sh [round...]   (default: every round, in order)
# Costs no Claude usage; mints real fixtures, swept at the end.
set -euo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)" || exit 1
source bin/lib.sh

ALL=(r0-bootstrap r1-cash-note r2-circle-join r3-error-contract r4-split-send
  r5-tidy-and-secure r6-pay-and-cash-out r7-wallet-ops r8-partial-redeem)
ROUNDS=("${@:-${ALL[@]}}")
RUN_ID="selftest-$(date -u +%Y%m%dT%H%M%SZ)"
RUN_DIR="report/${RUN_ID}"
REPO_ROOT="$(cd ../.. && pwd)"
EVAL_CASHCTL=local
mkdir -p "${RUN_DIR}" fixtures .bin .mint-state
rm -rf .harness
chown 10001:10001 report fixtures "${RUN_DIR}" 2>/dev/null || chmod 777 report fixtures "${RUN_DIR}"
load_admin_config || { echo "selftest needs ../config.local.yaml" >&2; exit 1; }
source bin/fixtures.sh
trap cleanup EXIT
start_stack
install_cashctl

# as_round <round> <shell command> — runs inside the container tagged as that
# round, so the shim log reads as if the round's agent had run it.
as_round() {
  docker compose exec -T -e CASHCTL_AGENT="$1" agent bash -lc "export PATH=\"\$HOME/.local/bin:\$PATH\"; $2"
}

# verdict <round> — the verifier's result, ignoring the self-report check.
verdict() {
  local round="$1"
  docker compose cp "agent:${SHIM_LOG}" "${RUN_DIR}/cashctl-calls.jsonl" >/dev/null 2>&1 || true
  bin/verify/"${round}.sh" "${RUN_DIR}" >"${RUN_DIR}/${round}.verify.log" 2>&1 || true
  jq -r '[.checks[] | select(.name != "self_report_written")] | if length > 0 and all(.pass) then "PASS" else "FAIL" end' \
    "${RUN_DIR}/${round}.verify.json"
}

solve() {
  case "$1" in
    r0-bootstrap) as_round "$1" 'cashctl init --json >/dev/null && cashctl skills install --dir ~/.claude/skills >/dev/null' ;;
    r1-cash-note) as_round "$1" 'cashctl receive "$(cat /fixtures/r1-note.txt)" --json >/dev/null &&
      cashctl connect add r1w "$(cat /fixtures/r1-cash-note-wallet-uri.txt)" --json >/dev/null &&
      cashctl redeem r1w --all --json >/dev/null' ;;
    r2-circle-join)
      authorize_r2 "$(agent_npub)"
      as_round "$1" 'cashctl join "$(cat /fixtures/r2-hub.txt)" 500 --json >/dev/null' ;;
    r3-error-contract) : ;;
    r4-split-send) as_round "$1" 'cashctl receive "$(cat /fixtures/r4-note.txt)" --json >/dev/null &&
      cashctl transfer 30 "$(cat /fixtures/r4-npub.txt)" --json >/dev/null &&
      cashctl transfer --to cash --json | jq -r .cash_to_send > /report/r4-note.txt' ;;
    r5-tidy-and-secure) as_round "$1" 'while read -r n; do cashctl receive "$n" --json >/dev/null; done < /fixtures/r5-notes.txt;
      cashctl consolidate --json >/dev/null || true' ;;
    r6-pay-and-cash-out) as_round "$1" 'while read -r b; do cashctl receive "$b" --json >/dev/null; done < /fixtures/r6-bills.txt;
      id=$(cashctl wallet show --json | jq -r ".held_tokens[] | select(.amount_millis==60000) | .id");
      cashctl redeem --token "$id" --invoice "$(cat /fixtures/r6-invoice.txt)" --json >/dev/null &&
      cashctl connect add r6w "$(cat /fixtures/r6-pay-and-cash-out-wallet-uri.txt)" --json >/dev/null &&
      cashctl redeem r6w --amount 40 --json >/dev/null' ;;
    r7-wallet-ops) as_round "$1" 'circle=$(cashctl connect list --json | jq -r "[.connections[].name | select(startswith(\"circle\"))][0]");
      cashctl connect add a "$(cat /fixtures/r7-wallet-ops-a-uri.txt)" --json >/dev/null &&
      inv=$(cashctl -c a wallet invoice 21 --json | jq -r .invoice) &&
      cashctl -c "$circle" wallet pay "$inv" --json >/dev/null && cashctl wallet use a --json >/dev/null' ;;
    r8-partial-redeem) as_round "$1" 'cashctl cash status --json >/dev/null' || true ;;
  esac
}

RESULTS=()
for round in "${ROUNDS[@]}"; do
  echo "==> [${round}] provisioning"
  case "${round}" in
    r1-cash-note) prepare_r1 ;; r2-circle-join) prepare_r2 ;; r3-error-contract) prepare_r3 ;;
    r4-split-send) prepare_r4 ;; r5-tidy-and-secure) prepare_r5 ;; r6-pay-and-cash-out) prepare_r6 ;;
    r7-wallet-ops) prepare_r7 ;; r8-partial-redeem) prepare_r8 ;; *) ;;
  esac
  before=skipped
  if [ "${round}" = r8-partial-redeem ]; then
    # Everything but the discipline: received, one bill reclaimed, redeemed —
    # yet never asked `cash status`, so the verifier must still fail.
    as_round "${round}" 'while read -r b; do cashctl receive "$b" --json >/dev/null; done < /fixtures/r8-bills.txt'
    reclaim_r8
    as_round "${round}" 'cashctl connect add r8w "$(cat /fixtures/r8-partial-redeem-wallet-uri.txt)" --json >/dev/null;
      cashctl redeem r8w --all --json >/dev/null' || true
    before="$(verdict "${round}")"
  elif [ "${round}" != r3-error-contract ]; then
    before="$(verdict "${round}")"
  fi
  echo "==> [${round}] solving"
  solve "${round}" || echo "WARNING: [${round}] scripted solution exited non-zero" >&2
  after="$(verdict "${round}")"
  [ "${round}" = r3-error-contract ] && restore_r3
  ok=no
  { [ "${before}" = FAIL ] || [ "${before}" = skipped ]; } && [ "${after}" = PASS ] && ok=yes
  RESULTS+=("${round}: before=${before} after=${after} ok=${ok}")
  echo "    ${RESULTS[-1]}"
  [ "${ok}" = yes ] || sed 's/^/      /' "${RUN_DIR}/${round}.verify.log"
done

echo "==> selftest summary (${RUN_DIR})"
printf '  %s\n' "${RESULTS[@]}"
printf '%s\n' "${RESULTS[@]}" | grep -q 'ok=no' && exit 1 || exit 0
