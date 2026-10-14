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

# --- fixtures: one mint "agent" per round, swept per agent at teardown -----
export GOWORK=off
(cd "${REPO_ROOT}" && go build -o integration/agent-eval/.bin/mint ./integration/agent-eval/mint)
export MINT_CONFIG="${REPO_ROOT}/integration/config.local.yaml"
export MINT_STATE="${PWD}/.mint-state/${RUN_ID}"
export MINT_RUN="agent-eval-${RUN_ID}"
mkdir -p "${MINT_STATE}"
MINT_AGENTS=()
# mint <round-agent> <subcommand> [args...] — --agent last so it always wins.
mint() {
  local agent="$1" sub="$2"
  shift 2
  case " ${MINT_AGENTS[*]:-} " in *" ${agent} "*) ;; *) MINT_AGENTS+=("${agent}") ;; esac
  .bin/mint "${sub}" "$@" --agent "${agent}"
}

# Apps created straight through the admin API (wallets with scopes mint does
# not offer, r2's circle hub): "<kind>:<app-id>", kind circle or wallet.
FIXTURE_APPS=()
cleanup() {
  echo "==> tearing down"
  for agent in "${MINT_AGENTS[@]:-}"; do
    [ -n "${agent}" ] && .bin/mint sweep --agent "${agent}" >/dev/null 2>&1 || true
  done
  for entry in "${FIXTURE_APPS[@]:-}"; do
    [ -n "${entry}" ] || continue
    local kind="${entry%%:*}" id="${entry##*:}"
    if [ "${kind}" = circle ]; then
      admin_api GET "/api/apps/${id}/circle/children?limit=0" 2>/dev/null |
        jq -r '.children[]?.appId' 2>/dev/null |
        while read -r child; do
          [ -n "${child}" ] && admin_api DELETE "/api/apps/${id}/circle/children/${child}" >/dev/null 2>&1 || true
        done
    fi
    admin_api DELETE "/api/apps/${id}" >/dev/null 2>&1 || true
  done
  docker compose down -v >/dev/null 2>&1 || true
  rm -rf .creds-seed fixtures/*
}

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

echo "==> ensuring a clean slate"
docker compose down -v >/dev/null 2>&1 || true
docker compose build agent
docker compose up -d
for _ in $(seq 1 30); do
  docker compose exec -T agent true >/dev/null 2>&1 && break
  sleep 1
done

# --- cashctl in front of the agent ---------------------------------------
SHIM_LOG=/home/evaluser/.cashctl-shim.jsonl
if [ "${EVAL_CASHCTL}" = local ]; then
  echo "==> building cashctl from this working tree"
  (cd "${REPO_ROOT}" && CGO_ENABLED=0 GOOS=linux go build -o integration/agent-eval/.bin/cashctl . &&
    go run ./integration/agent-eval/surface-dump > integration/agent-eval/.bin/surface.json)
  docker compose exec -T agent mkdir -p /home/evaluser/.local/bin /home/evaluser/.local/libexec/cashctl
  docker compose cp .bin/cashctl agent:/home/evaluser/.local/libexec/cashctl/cashctl-real
  docker compose cp campaign/cashctl-shim agent:/home/evaluser/.local/libexec/cashctl/cashctl-shim
  docker compose cp .bin/surface.json agent:/home/evaluser/.local/libexec/cashctl/surface.json
  # CASHCTL_AGENT comes from the exec that runs each round (the round's name),
  # or "harness" from agent_exec, so coverage counts only the agents' calls.
  docker compose exec -T agent bash -c "cat > /home/evaluser/.local/bin/cashctl <<'EOF'
#!/usr/bin/env bash
L=/home/evaluser/.local/libexec/cashctl
export CASHCTL_REAL=\$L/cashctl-real CASHCTL_SURFACE=\$L/surface.json CASHCTL_LOG=${SHIM_LOG}
export CASHCTL_AGENT=\${CASHCTL_AGENT:-unknown}
exec python3 \$L/cashctl-shim \"\$@\"
EOF
chmod +x /home/evaluser/.local/bin/cashctl"
  docker compose exec -T -u 0 agent chown -R 10001:10001 /home/evaluser/.local
fi

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
  return ${status}
}

# provision_wallet <round> <name> <scope...> — a plain NWC wallet with exactly
# these scopes, written to fixtures/<round>-<name>-uri.txt.
provision_wallet() {
  local round="$1" name="$2"
  shift 2
  local scopes w
  scopes="$(printf '%s\n' "$@" | jq -R . | jq -sc .)"
  w="$(admin_api POST /api/apps "{\"name\":\"cashctl agent-eval ${RUN_ID} ${round} ${name}\",\"scopes\":${scopes}}")"
  echo "${w}" | jq -r '.pairingUri' > "fixtures/${round}-${name}-uri.txt"
  echo "${w}" | jq -r '.id' > "$(harness_dir "${round}")/${name}-app-id.txt"
  FIXTURE_APPS+=("wallet:$(echo "${w}" | jq -r '.id')")
}

# r1: one cash note (cash mode, so receive's auto-protect runs) and a wallet.
prepare_r1() {
  local h out
  h="$(harness_dir r1-cash-note)"
  out="$(mint r1 token --hub r1 --cash --amount 60000)"
  echo "${out}" | jq -r '.gift' > fixtures/r1-note.txt
  echo "${out}" | jq -r '.hub_app_id' > "${h}/hub-app-id.txt"
  provision_wallet r1-cash-note wallet make_invoice get_balance get_info
}

# r2: an allowlist circle hub; the agent's npub is authorized mid-round.
prepare_r2() {
  local hub h
  h="$(harness_dir r2-circle-join)"
  hub="$(admin_api POST /api/apps '{"name":"cashctl agent-eval '"${RUN_ID}"' r2 circle","kind":"circle_hub","scopes":["circle_wallet"],"circleIdentityName":"cashctl agent-eval r2","circlePolicy":"allowlist","circleMaxExpSecs":86400,"circlePerWalletMaxMloki":1000000}')"
  R2_HUB_APP_ID="$(echo "${hub}" | jq -r '.id')"
  R2_HUB_TOKEN="$(echo "${hub}" | jq -r '.circleHubToken')"
  echo "${R2_HUB_APP_ID}" > "${h}/hub-app-id.txt"
  echo "${R2_HUB_TOKEN}" > "${h}/hub-token.txt"
  FIXTURE_APPS+=("circle:${R2_HUB_APP_ID}")
  # Funded to the per-wallet cap it advertises, so a join at the cap succeeds.
  admin_api POST /api/transfers "{\"toAppId\":${R2_HUB_APP_ID},\"amountLoki\":1000}" >/dev/null
}

# run_r2 authorizes the agent's identity as soon as it writes it, then drops
# the circle hub string for the round to pick up.
run_r2() {
  rm -f report/r2-npub.txt fixtures/r2-hub.txt
  run_round r2-circle-join &
  local pid=$! authorized=0
  for _ in $(seq 1 60); do
    if [ -s report/r2-npub.txt ]; then
      local npub hex
      npub="$(tr -d '[:space:]' < report/r2-npub.txt)"
      hex="$(decode_npub_hex "${npub}")"
      admin_api PUT "/api/apps/${R2_HUB_APP_ID}/circle/allowlist" "{\"pubkeys\":[\"${hex}\"]}" >/dev/null
      echo "${R2_HUB_TOKEN}" > fixtures/r2-hub.txt
      authorized=1
      break
    fi
    sleep 3
  done
  [ "${authorized}" -eq 1 ] || echo "WARNING: [r2-circle-join] agent never wrote report/r2-npub.txt" >&2
  wait "${pid}" || true
}

# r3: offline fakes — a well-formed token and a circle-hub invite, never funded.
prepare_r3() {
  mint r3 fake --kind token | jq -r .value > fixtures/r3-token.txt
  mint r3 fake --kind circle-hub | jq -r .value > fixtures/r3-circlehub.txt
}

# r4: one 90-loki cash note to split; P is a fresh third-party npub.
prepare_r4() {
  local h out
  h="$(harness_dir r4-split-send)"
  out="$(mint r4 token --hub r4 --cash --amount 90000)"
  echo "${out}" | jq -r '.gift' > fixtures/r4-note.txt
  echo "${out}" | jq -r '.hub_app_id' > "${h}/hub-app-id.txt"
  (cd "${REPO_ROOT}" && go run ./integration/agent-eval/decode-npub --new) > "${h}/p.json"
  jq -r '.npub' "${h}/p.json" > fixtures/r4-npub.txt
}

# r5: three cash notes from one hub and one from another.
prepare_r5() {
  local h out
  h="$(harness_dir r5-tidy-and-secure)"
  : > fixtures/r5-notes.txt
  : > "${h}/secrets.txt"
  for _ in 1 2 3; do
    out="$(mint r5 token --hub a --cash --amount 30000)"
    echo "${out}" | jq -r '.gift' >> fixtures/r5-notes.txt
    echo "${out}" | jq -r '.cash_secret' >> "${h}/secrets.txt"
    echo "${out}" | jq -r '.hub_app_id' > "${h}/hub-a-app-id.txt"
  done
  out="$(mint r5 token --hub b --cash --amount 20000)"
  echo "${out}" | jq -r '.gift' >> fixtures/r5-notes.txt
  echo "${out}" | jq -r '.cash_secret' >> "${h}/secrets.txt"
  echo "${out}" | jq -r '.hub_app_id' > "${h}/hub-b-app-id.txt"
}

# r6: four bills to the agent's own identity on one hub, an invoice that hub
# made (so paying it is same-node and fee-free), and a destination wallet.
prepare_r6() {
  local h npub out amt
  h="$(harness_dir r6-pay-and-cash-out)"
  npub="$(agent_npub)"
  for amt in 60000 24000 16000 30000; do
    out="$(mint r6 token --hub r6 --for "${npub}" --amount "${amt}")"
    echo "${out}" | jq -r '.cash_token' >> "${h}/tokens.txt"
    echo "${out}" | jq -r '.hub_app_id' > "${h}/hub-app-id.txt"
  done
  cp "${h}/tokens.txt" fixtures/r6-bills.txt
  mint r6 invoice --hub r6 --amount 60 | jq -r '.invoice' > fixtures/r6-invoice.txt
  provision_wallet r6-pay-and-cash-out wallet make_invoice get_balance get_info
}

# r7: a funded wallet A to invoice from; the agent pays it from its circle wallet.
prepare_r7() {
  provision_wallet r7-wallet-ops a make_invoice get_balance get_info list_transactions lookup_invoice
}

# r8: two bills to the agent; after it signals, the harness reclaims one.
prepare_r8() {
  local h npub out
  h="$(harness_dir r8-partial-redeem)"
  npub="$(agent_npub)"
  : > fixtures/r8-bills.txt
  for _ in 1 2; do
    out="$(mint r8 token --hub r8 --for "${npub}" --amount 30000)"
    echo "${out}" | jq -r '.cash_token' >> fixtures/r8-bills.txt
    echo "${out}" | jq -r '.hub_app_id' > "${h}/hub-app-id.txt"
  done
  provision_wallet r8-partial-redeem wallet make_invoice get_balance get_info
}

# run_r8 waits for the agent's signal that both bills are received, then has
# the operator delete one of them (it becomes `reclaimed`), and only then lets
# the agent continue.
run_r8() {
  local h pid
  h="$(harness_dir r8-partial-redeem)"
  rm -f report/r8-received.txt fixtures/r8-go.txt
  run_round r8-partial-redeem &
  pid=$!
  for _ in $(seq 1 80); do
    if [ -s report/r8-received.txt ]; then
      local hub victim
      hub="$(cat "${h}/hub-app-id.txt")"
      victim="$(hub_slices "${hub}" | jq -r '[.[] | select(.status=="unclaimed")][0].wallet_app_id // empty')"
      if [ -n "${victim}" ]; then
        admin_api DELETE "/api/apps/${hub}/cash-wallets/${victim}" >/dev/null 2>&1 || true
        echo "${victim}" > "${h}/reclaimed-app-id.txt"
      fi
      echo go > fixtures/r8-go.txt
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
