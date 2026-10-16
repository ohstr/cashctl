# shellcheck shell=bash
# Fixture provisioning shared by bin/run.sh and bin/selftest.sh: the mint
# wrapper and teardown, the agent stack, cashctl in the container, and each
# round's prepare step plus the operator actions some rounds need mid-round.
# Callers set RUN_ID, REPO_ROOT and EVAL_CASHCTL and source bin/lib.sh first.

# --- fixtures: one mint "agent" per round, swept per agent at teardown -----
export GOWORK=off
(cd "${REPO_ROOT}" && go build -o integration/agent-eval/.bin/mint ./integration/agent-eval/mint)
export MINT_CONFIG="${REPO_ROOT}/integration/config.local.yaml"
export MINT_STATE="${PWD}/.mint-state/${RUN_ID}"
export MINT_RUN="agent-eval-${RUN_ID}"
mkdir -p "${MINT_STATE}"
# The fixture agents the rounds mint under. A fixed list, not one recorded as
# mint runs: mint is almost always called inside $(...), a subshell, where an
# appended array would be lost and the fixtures never swept.
MINT_AGENTS=(r1 r3 r4 r5 r6 r7 r8)
# mint <round-agent> <subcommand> [args...] — --agent last so it always wins.
mint() {
  local agent="$1" sub="$2"
  shift 2
  .bin/mint "${sub}" "$@" --agent "${agent}"
}

# Apps created straight through the admin API (wallets with scopes mint does
# not offer, r2's circle hub): "<kind>:<app-id>", kind circle or wallet.
FIXTURE_APPS=()
cleanup() {
  echo "==> tearing down"
  restore_r3
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

# start_stack — a fresh agent container, responsive.
start_stack() {
  echo "==> ensuring a clean slate"
  restore_r3 # a run that died mid-r3 left the Hub's relays pointing nowhere
  docker compose down -v >/dev/null 2>&1 || true
  docker compose build agent
  docker compose up -d
  for _ in $(seq 1 30); do
    docker compose exec -T agent true >/dev/null 2>&1 && break
    sleep 1
  done
}

# --- cashctl in front of the agent ---------------------------------------
SHIM_LOG=/home/evaluser/.cashctl-shim.jsonl
# install_cashctl — for EVAL_CASHCTL=local, this working tree behind the shim.
install_cashctl() {
  [ "${EVAL_CASHCTL}" = local ] || return 0
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

# authorize_r2 <npub> — allowlist the agent under r2's circle hub and hand it
# the invite.
authorize_r2() {
  local hex
  hex="$(decode_npub_hex "$1")"
  admin_api PUT "/api/apps/${R2_HUB_APP_ID}/circle/allowlist" "{\"pubkeys\":[\"${hex}\"]}" >/dev/null
  echo "${R2_HUB_TOKEN}" > fixtures/r2-hub.txt
}

# r3: offline fakes — a well-formed token and a circle-hub invite, never funded,
# and a cash bill pasted with its secret — plus one live circle the Hub cannot
# check membership for yet.
prepare_r3() {
  mint r3 fake --kind token | jq -r .value > fixtures/r3-token.txt
  mint r3 fake --kind circle-hub | jq -r .value > fixtures/r3-circlehub.txt
  echo "$(mint r3 fake --kind token-cash | jq -r .value)#$(openssl rand -hex 32)" > fixtures/r3-bill.txt
  prepare_r3_unready
}

# prepare_r3_unready — for r3 only, the Hub's general relays point nowhere, so a
# following-policy circle's first contact-list fetch cannot resolve and its join
# is NOT_READY (NIP-CW: an unresolved list is never "not a member"). The original
# value is kept in .general-relay.restore, outside .harness (which a run wipes),
# so restore_r3 can put it back after r3's verifier, at teardown, or at the start
# of the next run if this one died.
prepare_r3_unready() {
  local hub provider
  restore_r3
  admin_api GET /api/info | jq -r '.generalRelay // empty' > .general-relay.restore
  if [ ! -s .general-relay.restore ]; then
    rm -f .general-relay.restore
    echo "r3: could not read the Hub's generalRelay" >&2
    return 1
  fi
  admin_api PATCH /api/settings '{"generalRelay":"ws://127.0.0.1:9"}' >/dev/null
  provider="$(cd "${REPO_ROOT}" && go run ./integration/agent-eval/decode-npub --new | jq -r .hex)"
  hub="$(admin_api POST /api/apps '{"name":"cashctl agent-eval '"${RUN_ID}"' r3 unready circle","kind":"circle_hub","scopes":["circle_wallet"],"circleIdentityName":"cashctl agent-eval r3 unready","circlePolicy":"following","providerPubkey":"'"${provider}"'","circleMaxExpSecs":86400,"circlePerWalletMaxMloki":1000000}')"
  FIXTURE_APPS+=("circle:$(echo "${hub}" | jq -r '.id')")
  echo "${hub}" | jq -r '.circleHubToken' > fixtures/r3-unready-hub.txt
}

# restore_r3 — puts back the generalRelay prepare_r3_unready replaced, if any.
restore_r3() {
  [ -s .general-relay.restore ] || return 0
  admin_api PATCH /api/settings "{\"generalRelay\":$(jq -Rs 'rtrimstr("\n")' < .general-relay.restore)}" >/dev/null &&
    rm -f .general-relay.restore
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

# r5: three cash notes from one hub, one from another, and 50 one-loki notes from
# a third — more than a Hub takes in one merge, so tidying them up has to merge in
# rounds (NIP-CASH §Ceilings).
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
  for _ in $(seq 1 50); do
    out="$(mint r5 token --hub c --cash --amount 1000)"
    echo "${out}" | jq -r '.gift' >> fixtures/r5-notes.txt
    echo "${out}" | jq -r '.cash_secret' >> "${h}/secrets.txt"
    echo "${out}" | jq -r '.hub_app_id' > "${h}/hub-c-app-id.txt"
  done
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

# r7: a wallet A to invoice on; the agent pays it from the circle wallet it
# joined in r2. That wallet starts empty and lokihub only moves a circle
# wallet's balance by payment, so the harness funds it with one: the circle
# wallet invoices 50 loki and a funded payer wallet pays it.
prepare_r7() {
  provision_wallet r7-wallet-ops a make_invoice get_balance get_info list_transactions lookup_invoice
  local payer circle inv
  payer="$(mint r7 nwc-wallet --kind pay | jq -r '.uri')"
  circle="$(agent_exec "cashctl connect list --json" | jq -r '[.connections[]?.name | select(startswith("circle"))][0] // empty')"
  if [ -z "${circle}" ]; then
    echo "WARNING: [r7-wallet-ops] no circle wallet to fund — run r2 first" >&2
    return 0
  fi
  inv="$(agent_exec "cashctl -c '${circle}' wallet invoice 50 --json" | jq -r '.invoice // empty')"
  agent_exec "cashctl -c '${payer}' wallet pay '${inv}' --json" >/dev/null ||
    echo "WARNING: [r7-wallet-ops] could not fund the circle wallet" >&2
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

# reclaim_r8 — the operator deletes one of r8's live bills (it becomes
# `reclaimed`), then lets the agent continue.
reclaim_r8() {
  local h hub victim
  h="$(harness_dir r8-partial-redeem)"
  hub="$(cat "${h}/hub-app-id.txt")"
  victim="$(hub_slices "${hub}" | jq -r '[.[] | select(.status=="unclaimed")][0].wallet_app_id // empty')"
  if [ -n "${victim}" ]; then
    admin_api DELETE "/api/apps/${hub}/cash-wallets/${victim}" >/dev/null 2>&1 || true
    echo "${victim}" > "${h}/reclaimed-app-id.txt"
  fi
  echo go > fixtures/r8-go.txt
}

