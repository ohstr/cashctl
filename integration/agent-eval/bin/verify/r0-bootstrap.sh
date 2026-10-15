#!/usr/bin/env bash
# Ground truth for R0: the binary runs, an identity exists, and the skills are
# on disk where Claude Code reads them — all checked inside the container.
ROUND="r0-bootstrap"
source "$(dirname "$0")/_head.sh" "$1"

if agent_exec "cashctl version --json" >/dev/null 2>&1; then
  add_check "cashctl_runs" true "cashctl version --json exits 0"
else
  add_check "cashctl_runs" false "cashctl version --json failed"
fi
npub="$(agent_exec "cashctl wallet show --json 2>/dev/null" | jq -r '.npub // empty')"
if [ -n "${npub}" ]; then
  add_check "identity_exists" true "${npub}"
else
  add_check "identity_exists" false "wallet show --json reports no npub"
fi
skills="$(agent_exec 'ls -d ~/.claude/skills/*/SKILL.md 2>/dev/null' | xargs -n1 dirname 2>/dev/null | xargs -n1 basename 2>/dev/null | sort | tr '\n' ' ')"
missing=""
for s in cashctl-wallet cashctl-cash cashctl-circle; do
  case " ${skills} " in *" ${s} "*) ;; *) missing="${missing} ${s}" ;; esac
done
if [ -z "${missing}" ]; then
  add_check "skills_installed" true "${skills}"
else
  add_check "skills_installed" false "missing under ~/.claude/skills:${missing} (found: ${skills:-none})"
fi
write_verify "${ROUND}" "${RUN_DIR}"
