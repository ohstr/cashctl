#!/usr/bin/env bash
# Merges each round's self-report (the agent's claim), verify result (the
# harness's independent ground-truth check) and judge verdict (process quality
# and a UX critique of cashctl) into one report for this run, adds a coverage
# section built from the logging shim's records (EVAL_CASHCTL=local), and
# appends a one-line summary to report/history.jsonl so runs are diffable.
set -uo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)" || exit 1
RUN_DIR="$1"; shift
ROUNDS=("$@")
CALLS="${RUN_DIR}/cashctl-calls.jsonl"

# clauses_of <round> — the spec clauses a round exercises, from its prompt's
# leading "<!-- clauses: ... -->" line.
clauses_of() {
  sed -n '1s/^<!-- clauses: \(.*\) -->$/\1/p' "rounds/$1.md" 2>/dev/null
}

# commands_of <round> — distinct cashctl command paths the round's agent ran.
commands_of() {
  [ -s "${CALLS}" ] || return 0
  jq -rs --arg r "$1" '[.[] | select(.agent==$r) | (if .path=="" then "(root)" else .path end)] | unique | join(", ")' "${CALLS}"
}

ROWS="[]"
for round in "${ROUNDS[@]}"; do
  SELF="$(cat "${RUN_DIR}/${round}.self-report.json" 2>/dev/null || echo 'null')"
  jq -e . >/dev/null 2>&1 <<<"${SELF}" || SELF='null'
  VERIFY="$(cat "${RUN_DIR}/${round}.verify.json" 2>/dev/null || echo 'null')"
  JUDGE="$(cat "${RUN_DIR}/${round}.judge.json" 2>/dev/null || echo 'null')"
  ROW="$(jq -n --arg round "${round}" --arg clauses "$(clauses_of "${round}")" --arg commands "$(commands_of "${round}")" \
    --argjson self "${SELF}" --argjson verify "${VERIFY}" --argjson judge "${JUDGE}" \
    '{round: $round, self_reported_outcome: ($self.outcome // "missing"), verified: ($verify.verified // false),
      process_quality: ($judge.process_quality // "unknown"), clauses: $clauses, commands: $commands,
      ux: ((($self.ux_friction // []) | map("agent: " + .)) + (($judge.ux_critique // []) | map("judge: " + .))),
      self_report: $self, verify: $verify, judge: $judge}')"
  ROWS="$(jq -c --argjson row "${ROW}" '. + [$row]' <<<"${ROWS}")"
done
echo "${ROWS}" | jq '.' > "${RUN_DIR}/report.json"

# Coverage: the agents' calls against the whole CLI surface. Waived: shell
# completion, help, and the vault password (no round uses an ncli vault).
COVERAGE=""
if [ -s "${CALLS}" ] && [ -s "${RUN_DIR}/surface.json" ]; then
  jq -c 'select(.agent != "harness")' "${CALLS}" > "${RUN_DIR}/agent-calls.jsonl"
  cat > "${RUN_DIR}/waivers.json" <<'EOF'
[{"group": "completion", "req": "ran_bare", "reason": "shell completion is not an agent task"},
 {"group": "<global>", "req": "ncli_vault_password_env", "reason": "no round uses an ncli vault"}]
EOF
  COVERAGE="$(python3 campaign/gate.py --surface "${RUN_DIR}/surface.json" --log "${RUN_DIR}/agent-calls.jsonl" \
    --waivers "${RUN_DIR}/waivers.json" --out "${RUN_DIR}/coverage.json" 2>&1 | grep -v '^  MISSING completion\|^  MISSING help' || true)"
fi

{
  echo "# cashctl agent-capability eval — $(basename "${RUN_DIR}") ($(cat "${RUN_DIR}/eval-cashctl.txt" 2>/dev/null || echo unknown) cashctl)"
  echo
  echo "| Round | Self-reported | Verified (ground truth) | Process quality | Spec clauses exercised |"
  echo "|---|---|---|---|---|"
  jq -r '.[] | "| \(.round) | \(.self_reported_outcome) | \(.verified) | \(.process_quality) | \(.clauses) |"' <<<"${ROWS}"
  echo
  echo "\`verified\` comes from bin/verify/*.sh re-checking ground truth directly"
  echo "(the operator's view of each hub's slices, exit codes, JSON shapes, ...),"
  echo "independent of what the agent claimed. \`process_quality\` is a separate"
  echo "judge pass over each transcript."
  echo
  echo "## UX critique"
  echo
  echo "What got in the agents' way, from their own \`ux_friction\` notes and the judge's"
  echo "critique of cashctl. Each is a candidate doc or UX fix, not a verdict."
  echo
  jq -r '.[] | select((.ux | length) > 0) | "### \(.round)\n\n" + (.ux | map("- " + .) | join("\n")) + "\n"' <<<"${ROWS}"
  [ "$(jq '[.[] | .ux | length] | add // 0' <<<"${ROWS}")" = 0 ] && echo "(nothing reported)"
  echo
  echo "## Coverage"
  echo
  if [ -n "${COVERAGE}" ]; then
    echo "Commands each round's agent ran:"
    echo
    jq -r '.[] | "- **\(.round)**: \(if .commands == "" then "(none)" else .commands end)"' <<<"${ROWS}"
    echo
    echo "Against the whole CLI surface (\`campaign/gate.py\`; a round set is not expected to"
    echo "reach every flag and prompt answer — this lists what no round exercised):"
    echo
    echo '```'
    echo "${COVERAGE}"
    echo '```'
  else
    echo "(no shim log: run with EVAL_CASHCTL=local for coverage)"
  fi
  echo
  echo "## Details"
  echo
  jq -r '.[] |
    "### \(.round)\n\n" +
    "- self-reported outcome: **\(.self_reported_outcome)** -- \(.self_report.summary // "(no summary)")\n" +
    "- verified: **\(.verify.verified // false)**\n" +
    ( (.verify.checks // []) | map("  - " + (if .pass then "[pass] " else "[FAIL] " end) + .name + ": " + .detail) | join("\n") ) + "\n" +
    "- process quality: **\(.process_quality)** -- \(.judge.notes // "(no notes)")\n" +
    ( if ((.judge.flags // []) | length) > 0 then "  - flags: " + ((.judge.flags // []) | join(", ")) else "" end ) +
    "\n"
  ' <<<"${ROWS}"
} > "${RUN_DIR}/report.md"

# REPORT_NO_HISTORY=1 regenerates a run's report (e.g. after re-judging)
# without recording the run a second time.
[ "${REPORT_NO_HISTORY:-}" = 1 ] && { echo "report regenerated: ${RUN_DIR}/report.md"; exit 0; }
mkdir -p report
TOTAL="$(jq 'length' <<<"${ROWS}")"
VERIFIED_COUNT="$(jq '[.[] | select(.verified)] | length' <<<"${ROWS}")"
GAPS="$(jq '[.gaps[] | select((.group | startswith("completion") or startswith("help")) | not)] | length' "${RUN_DIR}/coverage.json" 2>/dev/null || echo null)"
jq -nc --arg run_dir "${RUN_DIR}" --arg date "$(date -u +%FT%TZ)" --arg mode "$(cat "${RUN_DIR}/eval-cashctl.txt" 2>/dev/null || echo unknown)" \
  --argjson total "${TOTAL}" --argjson verified "${VERIFIED_COUNT}" --argjson coverage_gaps "${GAPS}" \
  '{date:$date, run_dir:$run_dir, cashctl:$mode, total:$total, verified:$verified, coverage_gaps:$coverage_gaps}' >> report/history.jsonl

echo "verified ${VERIFIED_COUNT}/${TOTAL} rounds — see ${RUN_DIR}/report.md"
