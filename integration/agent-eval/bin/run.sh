#!/usr/bin/env bash
# Orchestrates one full run of the cashctl agent-capability eval: brings up
# the isolated stack, puts cashctl in front of the agent (EVAL_CASHCTL), mints
# each round's real lokihub fixtures through ./mint, drives each round as a
# fresh, non-interactive `claude -p` invocation inside the agent container,
# runs a harness-side verifier and an LLM-judge pass per round, and assembles
# one report with a UX critique and a coverage table.
#
# Usage:
#   bin/run.sh                          # every round, in order
#   bin/run.sh r0-bootstrap r3-error-contract   # just these, in order given
#
#   EVAL_CASHCTL=local   (default) build this working tree and install it in the
#                        container behind the logging shim (campaign/cashctl-shim),
#                        so the report can say which commands the agents reached
#   EVAL_CASHCTL=release r0 installs the latest release the way PROMPT.md says;
#                        no shim, no coverage table
#
# Requires: docker + docker compose, this host already logged into Claude
# Code, a Go toolchain, and ../config.local.yaml pointing at a real,
# already-running lokihub admin API — see README.md.
set -euo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source bin/lib.sh

ALL_ROUNDS=(r0-bootstrap r1-cash-note r2-circle-join r3-error-contract r4-split-send
  r5-tidy-and-secure r6-pay-and-cash-out r7-wallet-ops r8-partial-redeem)
ROUNDS=("${@:-${ALL_ROUNDS[@]}}")
EVAL_CASHCTL="${EVAL_CASHCTL:-local}"
case "${EVAL_CASHCTL}" in local | release) ;; *)
  echo "EVAL_CASHCTL must be local or release, got ${EVAL_CASHCTL}" >&2
  exit 2
  ;;
esac

RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)"
RUN_DIR="report/${RUN_ID}"
REPO_ROOT="$(cd ../.. && pwd)"
mkdir -p "${RUN_DIR}" report fixtures .bin .mint-state
rm -rf .harness
# The agent container runs as uid 10001 (agent/Dockerfile) and writes its
# self-reports here. Grant exactly that: these directories hold real cash
# tokens, a circle-hub connection string and full agent transcripts.
chown 10001:10001 report fixtures "${RUN_DIR}" 2>/dev/null || chmod 777 report fixtures "${RUN_DIR}"
chmod 770 report fixtures 2>/dev/null || true
echo "${EVAL_CASHCTL}" > "${RUN_DIR}/eval-cashctl.txt"

echo "==> run ${RUN_ID} (${EVAL_CASHCTL}): ${ROUNDS[*]}"

if ! load_admin_config; then
  echo "ERROR: ../config.local.yaml missing or incomplete — see README.md (every round but r0 and r3 mints real fixtures)." >&2
fi

source bin/fixtures.sh

# --- credentials: copy, never bind-mount the host's real ~/.claude -------
HOST_CREDS="${HOME}/.claude/.credentials.json"
HOST_CLAUDE_JSON="${HOME}/.claude.json"
if [ ! -f "${HOST_CREDS}" ]; then
  echo "ERROR: ${HOST_CREDS} not found. Log into Claude Code on this host first (run 'claude' once)." >&2
  exit 1
fi
rm -rf .creds-seed
(umask 077 && mkdir -p .creds-seed)
install -m 600 "${HOST_CREDS}" .creds-seed/credentials.json
[ -f "${HOST_CLAUDE_JSON}" ] && install -m 600 "${HOST_CLAUDE_JSON}" .creds-seed/claude.json || true
chown -R 10001:10001 .creds-seed
chmod 700 .creds-seed
chmod 600 .creds-seed/*.json
trap cleanup EXIT

start_stack
install_cashctl

run_round() {
  local round="$1" prompt
  prompt="$(cat rounds/_preamble.md; echo; cat "rounds/${round}.md")"
  echo "==> [${round}] running agent"
  # report/ is not wiped between runs and the round writes to a fixed path:
  # clear it, or a crashed agent silently inherits an old self-report.
  rm -f "report/${round}.self-report.json"
  docker compose exec -T -e CASHCTL_AGENT="${round}" agent claude -p "${prompt}" \
    --output-format stream-json --verbose \
    --permission-mode bypassPermissions \
    > "${RUN_DIR}/${round}.transcript.jsonl" 2> "${RUN_DIR}/${round}.stderr.log"
  local status=$?
  cp "report/${round}.self-report.json" "${RUN_DIR}/${round}.self-report.json" 2>/dev/null ||
    echo "WARNING: [${round}] agent did not write a self-report" >&2
  # An account limit or API failure ends every later call the same way: stop
  # rather than score rounds no agent could attempt.
  if grep -q '"terminal_reason":"api_error"\|hit your session limit\|usage limit' "${RUN_DIR}/${round}.transcript.jsonl" 2>/dev/null; then
    echo "ERROR: [${round}] claude stopped on an API error or usage limit — aborting the run; nothing is recorded in report/history.jsonl." >&2
    grep -o 'hit your session limit[^"]*' "${RUN_DIR}/${round}.transcript.jsonl" | head -1 >&2
    exit 3
  fi
  return ${status}
}

# run_r2 authorizes the agent's identity as soon as it writes it.
run_r2() {
  rm -f report/r2-npub.txt fixtures/r2-hub.txt
  run_round r2-circle-join &
  local pid=$!
  for _ in $(seq 1 60); do
    if [ -s report/r2-npub.txt ]; then
      authorize_r2 "$(tr -d '[:space:]' < report/r2-npub.txt)"
      break
    fi
    sleep 3
  done
  [ -s fixtures/r2-hub.txt ] || echo "WARNING: [r2-circle-join] agent never wrote report/r2-npub.txt" >&2
  wait "${pid}" || true
}

# run_r8 reclaims one bill once the agent signals it received both.
run_r8() {
  local pid
  rm -f report/r8-received.txt fixtures/r8-go.txt
  run_round r8-partial-redeem &
  pid=$!
  for _ in $(seq 1 80); do
    if [ -s report/r8-received.txt ]; then
      reclaim_r8
      break
    fi
    sleep 3
  done
  [ -s fixtures/r8-go.txt ] || echo "WARNING: [r8-partial-redeem] agent never signalled" >&2
  wait "${pid}" || true
}

for round in "${ROUNDS[@]}"; do
  case "${round}" in
    r1-cash-note) prepare_r1 && run_round "${round}" || echo "WARNING: [${round}] did not run cleanly" >&2 ;;
    r2-circle-join) prepare_r2 && run_r2 || echo "WARNING: [${round}] did not run cleanly" >&2 ;;
    r3-error-contract) prepare_r3 && run_round "${round}" || echo "WARNING: [${round}] did not run cleanly" >&2 ;;
    r4-split-send) prepare_r4 && run_round "${round}" || echo "WARNING: [${round}] did not run cleanly" >&2 ;;
    r5-tidy-and-secure) prepare_r5 && run_round "${round}" || echo "WARNING: [${round}] did not run cleanly" >&2 ;;
    r6-pay-and-cash-out) prepare_r6 && run_round "${round}" || echo "WARNING: [${round}] did not run cleanly" >&2 ;;
    r7-wallet-ops) prepare_r7 && run_round "${round}" || echo "WARNING: [${round}] did not run cleanly" >&2 ;;
    r8-partial-redeem) prepare_r8 && run_r8 || echo "WARNING: [${round}] did not run cleanly" >&2 ;;
    *) run_round "${round}" || echo "WARNING: [${round}] claude invocation exited non-zero" >&2 ;;
  esac
  bin/verify/"${round}.sh" "${RUN_DIR}" || echo "WARNING: [${round}] verifier reported problems" >&2
done

if [ "${EVAL_CASHCTL}" = local ]; then
  docker compose cp "agent:${SHIM_LOG}" "${RUN_DIR}/cashctl-calls.jsonl" 2>/dev/null ||
    echo "WARNING: no shim log — the agents never ran cashctl?" >&2
  cp .bin/surface.json "${RUN_DIR}/surface.json"
fi

echo "==> judging transcripts"
bin/judge.sh "${RUN_DIR}" "${ROUNDS[@]}"

echo "==> assembling report"
bin/report.sh "${RUN_DIR}" "${ROUNDS[@]}"

echo "==> done: ${RUN_DIR}/report.md"
