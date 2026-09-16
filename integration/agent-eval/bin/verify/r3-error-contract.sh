#!/usr/bin/env bash
# Ground truth for R3: re-runs each probe itself, inside the agent
# container, and checks the exit code + JSON shape directly against
# AGENTS.md's own table — independent of whatever the agent claimed in
# its self-report (this round's whole point is a contract an agent could
# get subtly wrong or gloss over).
set -uo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source bin/lib.sh
RUN_DIR="$1"
ROUND="r3-error-contract"

if self_report_exists "${RUN_DIR}" "${ROUND}"; then
  add_check "self_report_written" true "present"
else
  add_check "self_report_written" false "missing — agent never wrote it"
fi

# check_probe <name> <want_exit> <want_code> <command...>
check_probe() {
  local name="$1" want_exit="$2" want_code="$3"; shift 3
  local out exit_code
  out="$(agent_exec "$* 2>&1")"
  exit_code=$?
  local got_code
  got_code="$(jq -r '.code // empty' <<<"${out}" 2>/dev/null)"
  if [ "${exit_code}" = "${want_exit}" ] && [ "${got_code}" = "${want_code}" ]; then
    add_check "${name}" true "exit=${exit_code} code=${got_code}"
  else
    add_check "${name}" false "want exit=${want_exit} code=${want_code}, got exit=${exit_code} code=${got_code:-<none>}: ${out}"
  fi
}

check_probe "unknown_command_is_usage"  2 usage         cashctl bogus-command --json
check_probe "unknown_flag_is_usage"     2 usage         cashctl wallet show --bogus-flag --json
check_probe "missing_required_hub_is_usage" 2 usage     cashctl join --json
check_probe "missing_required_to_is_usage"  2 usage     cashctl transfer --json
check_probe "bad_token_is_invalid_input" 3 invalid_input cashctl decode not-a-valid-token --json
check_probe "no_held_tokens_is_not_found" 4 not_found   cashctl redeem --json
check_probe "unknown_wallet_is_not_found" 4 not_found   cashctl wallet balance --from does-not-exist --json

write_verify "${ROUND}" "${RUN_DIR}"
