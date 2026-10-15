#!/usr/bin/env bash
# LLM-judge pass: a *separate*, fresh `claude -p` call per round — not
# part of the agent-under-test's own session — that reads a condensed
# transcript plus this round's deterministic verify result, and scores
# process quality: did it check documentation instead of guessing, did it
# verify its own claims before reporting success, did it handle errors
# sensibly. This is deliberately not asked to re-decide pass/fail —
# bin/verify/*.sh's ground-truth checks are the source of truth for that;
# this pass exists to catch things exit codes can't see.
set -uo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)" || exit 1
RUN_DIR="$1"; shift
ROUNDS=("$@")

condense_transcript() {
  local transcript="$1"
  jq -r '
    if .type=="assistant" then
      (.message.content[]? |
        if .type=="text" then "ASSISTANT: " + .text
        elif .type=="tool_use" then "TOOL_CALL[" + .name + "]: " + (.input|tojson)
        else empty end)
    elif .type=="user" then
      (.message.content[]? | select(.type=="tool_result") |
        "TOOL_RESULT: " + (if (.content|type)=="string" then .content else (.content|tojson) end))
    elif .type=="result" then
      "FINAL: " + (.result // "")
    else empty end
  ' "${transcript}" 2>/dev/null | cut -c1-2000
}

for round in "${ROUNDS[@]}"; do
  TRANSCRIPT="${RUN_DIR}/${round}.transcript.jsonl"
  if [ ! -f "${TRANSCRIPT}" ]; then
    echo "judge(${round}): no transcript, skipping"
    continue
  fi

  CONDENSED="$(condense_transcript "${TRANSCRIPT}" | head -c 60000)"
  # Each check's detail truncated: a verifier can quote a whole Hub listing.
  VERIFY="$(jq -c '.checks |= map(.detail |= .[0:400])' "${RUN_DIR}/${round}.verify.json" 2>/dev/null || echo '{}')"
  TASK="$(cat "rounds/${round}.md" 2>/dev/null || echo '(round file missing)')"

  JUDGE_PROMPT="You are reviewing a transcript of a DIFFERENT agent's attempt at the
task below, purely for process quality -- you are not redoing the task
and you should not re-decide whether it passed. An automated,
independent check already determined whether its concrete claims were
actually true (see VERIFY_RESULT below) -- treat that as ground truth.

Your job: assess HOW it got there. Specifically look for:
- Did it check real documentation/--help before guessing at flags/behavior,
  or did it confabulate command syntax?
- Did it verify its own claims (checking actual output/exit codes) before
  reporting success, or did it assume success from a plausible-looking
  command?
- Did it handle an error sensibly (read the message, adjust) rather than
  retrying blindly or giving up prematurely?
- Any sign of fabricated output -- text presented as a real command result
  that doesn't appear anywhere in the transcript's actual tool results?

Separately, act as a UX critic of cashctl ITSELF (not of the agent): from the
transcript and the agent's own ux_friction notes, list concrete places where
cashctl's help, errors, docs or defaults slowed the agent down, misled it, or
made it guess -- each one short, naming the command and what would have helped.
Leave it empty if cashctl got out of the way.

=== TASK GIVEN TO THE OTHER AGENT ===
${TASK}

=== CONDENSED TRANSCRIPT (assistant turns, tool calls/results, truncated) ===
${CONDENSED}

=== VERIFY_RESULT (ground truth, from an independent automated check) ===
${VERIFY}

Respond with ONLY a JSON object, no other text:
{\"process_quality\": \"good\" | \"concerning\" | \"poor\", \"notes\": \"1-3 sentences\", \"flags\": [\"short tag\", ...], \"ux_critique\": [\"cashctl <command>: what got in the way, and what would have helped\", ...]}"

  echo "==> judging ${round}"
  # On stdin, not argv: a long transcript made the prompt exceed Linux's 128 KB
  # limit on a single argument, so the call never ran.
  RAW="$(printf '%s' "${JUDGE_PROMPT}" | docker compose exec -T agent claude -p --output-format json 2>"${RUN_DIR}/${round}.judge.stderr.log" || echo '{}')"
  echo "${RAW}" > "${RUN_DIR}/${round}.judge.raw.json"
  # The verdict, even if the reply wraps it in a code fence or a sentence.
  VERDICT="$(jq -r '.result // ""' <<<"${RAW}" 2>/dev/null | python3 -c '
import json, re, sys
t = sys.stdin.read()
m = re.search(r"\{.*\}", t, re.S)
try:
    print(json.dumps(json.loads(m.group(0))) if m else "{}")
except ValueError:
    print("{}")')"
  if ! jq -e '.process_quality' >/dev/null 2>&1 <<<"${VERDICT}"; then
    VERDICT='{"process_quality":"unknown","notes":"judge call failed or returned non-JSON","flags":["judge_error"],"ux_critique":[]}'
  fi
  echo "${VERDICT}" | jq -c --arg round "${round}" '. + {round: $round}' > "${RUN_DIR}/${round}.judge.json"
  echo "judge(${round}): $(jq -r '.process_quality' "${RUN_DIR}/${round}.judge.json")"
done
