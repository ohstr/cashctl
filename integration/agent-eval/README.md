# Agent-capability eval

A different kind of test from everything else in this repo: instead of
asserting cashctl's own code is correct, this harness measures whether a
**real, unmodified Claude Code agent** — with no source checkout, no
special knowledge of cashctl, and only what it can fetch at runtime
(`README.md`/`AGENTS.md`/`skills/`) — can actually pick up cashctl cold and
use it correctly. It's slow, it costs real money (genuine Claude API
usage per round), and it's not deterministic the way `go test` is — so
it's not run in CI. Run it by hand before a release, or whenever
`AGENTS.md`/the skills change, to catch documentation gaps a unit test
can't see.

## How it works

```
compose.yaml   -- one throwaway `agent` container: Debian, curl, jq, and
                  the Claude Code CLI. No cashctl, no Go, no source checkout.
rounds/*.md    -- one task per round, each a fresh `claude -p` invocation
bin/run.sh     -- orchestrates a full run: provisions fixtures, drives
                  each round, verifies, judges, reports
bin/verify/*.sh -- one ground-truth check per round, re-derived
                  independently (against lokihub's admin API or by
                  re-running the exact probe itself) -- never trusts the
                  agent's own self-report
bin/judge.sh   -- a *separate* claude call scoring process quality
                  (did it check docs, did it verify its own claims) --
                  supplementary, not the verdict
bin/report.sh  -- merges self-report + verify + judge into one report
```

Each round ends by having the agent write a structured self-report to
`/report/<round>.self-report.json` (schema: `rounds/_report-schema.json`).
That claim is **never** taken as ground truth on its own — `bin/verify/
<round>.sh` re-derives the real outcome independently, either by querying
the same lokihub instance's admin API directly, or (for the error-contract
round) by re-running the exact same probes itself and checking the exit
code/JSON shape against `AGENTS.md`'s own table.

## Setup

1. Log into Claude Code on this host at least once (`claude` needs to have
   worked here before — `bin/run.sh` copies `~/.claude/.credentials.json`
   into the container, never bind-mounts your real one).
2. `r1-cash-lifecycle` and `r2-circle-join` need a real lokihub admin API
   to mint their fixtures — the **same** `../config.local.yaml` the Go
   integration suite uses (see `../README.md`). `r0-bootstrap` and
   `r3-error-contract` don't need it at all.
3. `docker` + `docker compose`, and a Go toolchain on this host (used to
   decode an npub and mint a cash-mode token — see `decode-npub/` and
   `mint-fixture/`; neither ships with, or is imported by, cashctl itself).

```sh
bin/run.sh                              # every round
bin/run.sh r0-bootstrap r3-error-contract   # just these
```

## Rounds

- **r0-bootstrap** — install cashctl from scratch (following
  `PROMPT.md`/`README.md`), confirm `cashctl init --json`, pull in the
  matching skill.
- **r1-cash-lifecycle** — receive a real, pre-minted cash-mode token,
  verify it, redeem it into a real invoice. Verified by checking the
  minted cash_wallet child's server-side `claimed` state directly, not by
  trusting `cashctl redeem`'s own reported success.
- **r2-circle-join** — join a real, ephemeral circle_hub (the agent's own
  freshly generated identity gets authorized mid-round, once it exists).
  Verified by checking a circle_wallet child actually exists under that
  hub afterward.
- **r3-error-contract** — deliberately misuse the CLI seven different ways
  (unknown command, missing required flag, bad input, nothing to act on,
  ...) and check the exit code/JSON shape against `AGENTS.md`'s table.
  Needs no fixtures — verified by the harness re-running the exact same
  seven commands itself.

This is a smaller, more curated set than exhaustively walking every
command (transfer/consolidate/the full inspect trio are already covered
by `../`'s Go integration suite) — these four rounds are chosen to cover
the highest-value, most-likely-to-go-wrong paths: first contact, the two
real money-moving flows, and the contract an agent leans on to recover
from its own mistakes.

## Interpreting a report

`report/<run-id>/report.md` has one row per round: what the agent
self-reported, whether the harness's independent check actually verified
it, and a separate process-quality judgment. A round can self-report
"pass" and still fail verification (the agent was wrong, or lied) — treat
`verified` as the real answer. `report/history.jsonl` accumulates one line
per run so results are diffable across releases.
