#!/usr/bin/env bash
# Ground truth for R0: is cashctl *actually* installed and runnable, verified
# by the harness invoking it directly — not by trusting the agent's claim.
set -uo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source bin/lib.sh
RUN_DIR="$1"
ROUND="r0-bootstrap"

if self_report_exists "${RUN_DIR}" "${ROUND}"; then
  add_check "self_report_written" true "present"
else
  add_check "self_report_written" false "missing — agent never wrote it"
fi

VERSION_JSON="$(agent_exec 'cashctl version --json' 2>/dev/null)"
if [ -n "${VERSION_JSON}" ] && jq -e '.version' >/dev/null 2>&1 <<<"${VERSION_JSON}"; then
  add_check "cashctl_installed_and_runnable" true "cashctl version --json -> version=$(jq -r '.version' <<<"${VERSION_JSON}")"
else
  add_check "cashctl_installed_and_runnable" false "cashctl version --json did not return valid JSON with a version field"
fi

# The agent has already run `init` in this container, so re-running it here
# returns {"already_configured": true} with no npub — which used to fail this
# check unless the agent had FAILED to set up an identity. Probe under a
# throwaway config dir instead, so this exercises a genuine first run.
INIT_JSON="$(agent_exec 'cashctl init --json --config-dir "$(mktemp -d)"' 2>/dev/null)"
if jq -e '.npub | test("^npub1")' >/dev/null 2>&1 <<<"${INIT_JSON}"; then
  add_check "init_json_sane_shape" true "cashctl init --json returns a well-formed npub"
else
  add_check "init_json_sane_shape" false "cashctl init --json output missing/malformed npub: ${INIT_JSON}"
fi

# And confirm the agent's own run actually produced an identity, which is
# what the round asked of it — the throwaway probe above cannot show that.
AGENT_NPUB="$(jq -r '.. | strings | select(test("^npub1"))' "${RUN_DIR}/r0-bootstrap.self-report.json" 2>/dev/null | head -1)"
if [ -n "${AGENT_NPUB}" ]; then
  add_check "agent_identity_created" true "agent reported ${AGENT_NPUB}"
else
  add_check "agent_identity_created" false "agent's self-report contains no npub1... identity"
fi

write_verify "${ROUND}" "${RUN_DIR}"
