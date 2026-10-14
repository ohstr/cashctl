# shellcheck shell=bash
# Sourced by every verifier: cd to the eval root, load helpers, check the
# self-report exists. Sets RUN_DIR and ROUND.
set -uo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)" || exit 1
source bin/lib.sh
RUN_DIR="$1"
if self_report_exists "${RUN_DIR}" "${ROUND}"; then
  add_check "self_report_written" true "present"
else
  add_check "self_report_written" false "missing — agent never wrote it"
fi

# need_admin — stops the verifier (recorded as a failure) when there is no admin API.
need_admin() {
  if ! load_admin_config; then
    add_check "admin_api_configured" false "../config.local.yaml missing/incomplete — cannot verify ground truth"
    write_verify "${ROUND}" "${RUN_DIR}"
    exit 0
  fi
}

# harness_file <round> <name> — a value prepare_* recorded, or stop if missing.
harness_file() {
  local f
  f="$(harness_dir "$1")/$2"
  if [ ! -s "${f}" ]; then
    add_check "fixture_provisioned" false "${f} missing — the round's prepare step never ran or failed"
    write_verify "${ROUND}" "${RUN_DIR}"
    exit 0
  fi
  cat "${f}"
}
