#!/usr/bin/env bash
# Shared helpers for bin/verify/*.sh and bin/run.sh. Each verifier's whole
# job is to re-check ground truth itself — querying the lokihub admin API
# or the agent container directly — rather than trust the agent's own
# self-report. The self-report is still read (self_report_field) so a
# verifier can cross-check "does what it claimed match what's actually
# true", not to take the claim at face value.
set -uo pipefail

CHECKS_JSON="[]"

# add_check <name> <true|false> <detail>
add_check() {
  local name="$1" pass="$2" detail="$3"
  CHECKS_JSON="$(jq -c --arg n "${name}" --argjson p "${pass}" --arg d "${detail}" \
    '. + [{"name":$n,"pass":$p,"detail":$d}]' <<<"${CHECKS_JSON}")"
  if [ "${pass}" = "true" ]; then
    echo "  [pass] ${name}: ${detail}"
  else
    echo "  [FAIL] ${name}: ${detail}"
  fi
}

# write_verify <round> <run_dir>
write_verify() {
  local round="$1" run_dir="$2"
  local verified
  verified="$(jq '(length > 0) and all(.[]; .pass)' <<<"${CHECKS_JSON}")"
  jq -n --arg round "${round}" --argjson checks "${CHECKS_JSON}" --argjson verified "${verified}" \
    '{round:$round, verified:$verified, checks:$checks}' > "${run_dir}/${round}.verify.json"
  echo "verify(${round}): $([ "${verified}" = true ] && echo PASS || echo FAIL)"
  [ "${verified}" = true ]
}

# self_report_field <run_dir> <round> <jq filter>
self_report_field() {
  local run_dir="$1" round="$2" filter="$3"
  jq -r "${filter} // empty" "${run_dir}/${round}.self-report.json" 2>/dev/null
}

self_report_exists() {
  local run_dir="$1" round="$2"
  [ -s "${run_dir}/${round}.self-report.json" ]
}

# agent_exec <command...> — runs inside the agent container, cashctl already on PATH
# Tagged CASHCTL_AGENT=harness so the logging shim's coverage counts only the
# agent's own calls.
agent_exec() {
  docker compose exec -T -e CASHCTL_AGENT=harness agent bash -lc "export PATH=\"\$HOME/.local/bin:\$PATH\"; $*"
}

# --- lokihub admin API (fixture provisioning + independent ground truth) ---
# Reuses the exact same ../config.local.yaml the Go integration suite
# reads (integration/config.go) — one setup step covers both suites.

ADMIN_BASE_URL=""
ADMIN_TOKEN=""

# load_admin_config — populates ADMIN_BASE_URL/ADMIN_TOKEN from
# ../config.local.yaml. Returns non-zero (callers should skip, not fail)
# if the file is missing or incomplete.
load_admin_config() {
  local cfg
  cfg="$(dirname "${BASH_SOURCE[0]}")/../../config.local.yaml"
  [ -f "${cfg}" ] || return 1
  ADMIN_BASE_URL="$(grep -m1 'base_url:' "${cfg}" | sed -E 's/.*base_url:\s*"?([^"[:space:]]*)"?.*/\1/')"
  ADMIN_TOKEN="$(grep -m1 'token:' "${cfg}" | sed -E 's/.*token:\s*"?([^"[:space:]]*)"?.*/\1/')"
  [ -n "${ADMIN_BASE_URL}" ] && [ -n "${ADMIN_TOKEN}" ]
}

# admin_api <method> <path> [json-body]
admin_api() {
  local method="$1" path="$2" body="${3:-}"
  if [ -n "${body}" ]; then
    curl -fsS -X "${method}" "${ADMIN_BASE_URL}${path}" \
      -H "Authorization: Bearer ${ADMIN_TOKEN}" -H "Content-Type: application/json" -d "${body}"
  else
    curl -fsS -X "${method}" "${ADMIN_BASE_URL}${path}" \
      -H "Authorization: Bearer ${ADMIN_TOKEN}"
  fi
}

# decode_npub_hex <npub1...> — via the tiny dev-only Go helper (see
# ../decode-npub/main.go); run from the repo root so `go run` resolves
# this module's own go.mod/dependencies.
decode_npub_hex() {
  (cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && GOWORK=off go run ./integration/agent-eval/decode-npub "$1")
}

# --- fixtures and ground truth for the goal-phrased rounds -----------------

# harness_dir <round> — harness-only state for a round (fixture ids, expected
# values). Never mounted into the agent container, unlike fixtures/ and report/.
harness_dir() {
  local d
  d="$(dirname "${BASH_SOURCE[0]}")/../.harness/$1"
  mkdir -p "${d}"
  echo "${d}"
}

# hub_slices <hub_app_id> — every recipient slice under a cash hub, live or
# archived, with the spec's state names in .status (unclaimed, redeemed, split,
# expired, reclaimed, written-off). The operator's view: independent of anything
# the agent or cashctl reports.
hub_slices() {
  admin_api GET "/api/apps/$1/cash-wallets?limit=0" 2>/dev/null | jq -c '.claims // []'
}

# agent_npub — the agent's identity, creating one if no round has yet.
agent_npub() {
  local npub
  npub="$(agent_exec "cashctl wallet show --json 2>/dev/null" | jq -r '.npub // empty')"
  if [ -z "${npub}" ]; then
    npub="$(agent_exec "cashctl init --json 2>/dev/null" | jq -r '.npub // empty')"
  fi
  echo "${npub}"
}
