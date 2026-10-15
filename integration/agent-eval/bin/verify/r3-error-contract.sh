#!/usr/bin/env bash
# Ground truth for R3: re-runs each probe itself inside the container and checks
# exit code, `code`, `retryable` and an empty stdout against AGENTS.md's table,
# independent of what the agent recorded.
ROUND="r3-error-contract"
source "$(dirname "$0")/_head.sh" "$1"

# check_probe <name> <want_exit> <want_code> <want_retryable> <shell command>
check_probe() {
  local name="$1" want_exit="$2" want_code="$3" want_retry="$4" cmd="$5"
  local out err exit_code
  out="$(agent_exec "${cmd} 2>/tmp/probe.err; echo \"exit=\$?\"")"
  exit_code="$(grep -o 'exit=[0-9]*' <<<"${out}" | tail -1 | cut -d= -f2)"
  stdout="$(sed '$d' <<<"${out}")"
  err="$(agent_exec "cat /tmp/probe.err")"
  local code retry
  code="$(jq -r '.code // empty' <<<"${err}" 2>/dev/null)"
  retry="$(jq -r '.retryable' <<<"${err}" 2>/dev/null)"
  if [ "${exit_code}" = "${want_exit}" ] && [ "${code}" = "${want_code}" ] && [ "${retry}" = "${want_retry}" ] && [ -z "${stdout}" ]; then
    add_check "${name}" true "exit=${exit_code} code=${code} retryable=${retry}"
  else
    add_check "${name}" false "want exit=${want_exit} code=${want_code} retryable=${want_retry} and empty stdout; got exit=${exit_code} code=${code:-<none>} retryable=${retry} stdout=[${stdout}] stderr=${err}"
  fi
}

check_probe unknown_command_is_usage 2 usage false "cashctl bogus-command --json"
check_probe unknown_flag_is_usage 2 usage false "cashctl wallet show --bogus-flag --json"
check_probe bare_join_is_usage 2 usage false "cashctl join --json"
check_probe bare_transfer_is_usage 2 usage false "cashctl transfer --json"
check_probe bad_token_is_invalid_input 3 invalid_input false "cashctl decode not-a-valid-token --json"
check_probe unknown_skill_is_not_found 4 not_found false "cashctl skills show nope --json"
check_probe unknown_wallet_use_is_not_found 4 not_found false "cashctl connect use nope --json"
check_probe receive_rejects_connection_flag 2 usage false "cashctl receive -c anything \"\$(cat /fixtures/r3-token.txt)\" --json"
check_probe circlehub_is_not_a_wallet 3 invalid_input false "cashctl connect add eval-r3 \"\$(cat /fixtures/r3-circlehub.txt)\" --json"

# The bare transfer's message names the amount-only cash-note form (I-2).
msg="$(agent_exec 'cashctl transfer --json 2>&1 >/dev/null' | jq -r '.error // empty')"
grep -q 'transfer <amount>' <<<"${msg}" && add_check "bare_transfer_names_both_forms" true "${msg}" ||
  add_check "bare_transfer_names_both_forms" false "message does not mention the amount-only form: ${msg}"
write_verify "${ROUND}" "${RUN_DIR}"
