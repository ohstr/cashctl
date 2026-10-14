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
2. Every round except r0 needs a real lokihub admin API to mint its
   fixtures — the **same** `../config.local.yaml` the Go integration suite
   uses (see `../README.md`).
3. `docker` + `docker compose`, and a Go toolchain on this host (used to
   build cashctl, decode an npub, and mint fixtures through `mint/`, the
   campaign's quota-enforcing fixture service, swept at teardown; none of
   these ship with, or are imported by, cashctl itself).

```sh
bin/run.sh                              # every round
bin/run.sh r0-bootstrap r3-error-contract   # just these
```

## What cashctl the agent gets

`EVAL_CASHCTL=local` (the default) builds this working tree and installs it in
the container behind the logging shim (`campaign/cashctl-shim`), so a run tests
the code you are about to ship and the report can say which commands the agents
reached. `EVAL_CASHCTL=release` leaves cashctl out entirely: r0 installs the
latest release the way `PROMPT.md` says, and there is no coverage table.

## Rounds

Each round states a goal, not a command list: it tests whether an agent can
find its way with `--help`, AGENTS.md and the skills, not whether it can
copy-paste. Each verifier re-derives the outcome itself, mostly from the
operator's view of the minting hub's slices (`status`: unclaimed, redeemed,
split, reclaimed, ...), never from the agent's report.

| Round | Goal | Verified by |
|---|---|---|
| r0-bootstrap | cashctl runs, an identity exists, the skills are installed | binary runs; npub; the three skill dirs on disk |
| r1-cash-note | get a cash note's money into a wallet | no slice left `unclaimed`, at least one `redeemed` |
| r2-circle-join | join a circle with a 500 cap, then try again | one circle wallet; the Hub reports a 500-loki budget; a second join fails as `exists` (exit 9) |
| r3-error-contract | nine misuses, checked against AGENTS.md's table | the harness re-runs each probe: exit code, `code`, `retryable`, empty stdout |
| r4-split-send | send 30 of a 90 note to someone, keep the rest as a note | a 30 slice for P's pubkey, a 60 cash slice, and the reported note is takeable |
| r5-tidy-and-secure | take four notes, secure them, hold as few tokens as possible | one live slice per hub, none answering to a handed-out secret |
| r6-pay-and-cash-out | pay a 60 invoice, cash out exactly 40 | 60 and 24 + 16 `redeemed`, 30 still `unclaimed` |
| r7-wallet-ops | invoice on a wallet, pay it from the circle wallet, switch default | a settled 21-loki payment on `a`; `a` is the default |
| r8-partial-redeem | cash out two bills when the operator reclaims one mid-round | one `redeemed`, one `reclaimed`; no blind resend; `cash status` before concluding |

Each round's prompt names the spec clauses it exercises on its first line; the
report lists them.

What a single-node lokihub cannot show: every redeem is same-node and so
fee-free, which means a nonzero redeem fee and the zero-payout refusal are not
exercised here. The unit and integration suites cover them.

## Interpreting a report

`report/<run-id>/report.md` has one row per round: what the agent
self-reported, whether the harness's independent check actually verified
it, a separate process-quality judgment, and the spec clauses it exercised.
Below that, a UX critique (the agents' `ux_friction` notes and the judge's
critique of cashctl itself) and, for a local run, coverage: the commands each
round reached and what no round exercised. A round can self-report
"pass" and still fail verification (the agent was wrong, or lied) — treat
`verified` as the real answer. `report/history.jsonl` accumulates one line
per run so results are diffable across releases.
