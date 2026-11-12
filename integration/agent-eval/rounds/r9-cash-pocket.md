<!-- clauses: CP-the-cash-pocket-ledger, CP-balance-and-entries-no-relay, CP-export-import-bundle, CP-sync-opt-in, CP-serve-is-a-daemon -->
# Round R9 — Keep and sync your Cash Pocket

`cashctl` should already be installed and set up, with an identity. The **Cash
Pocket** (NIP-CP) is cashctl's local ledger of the bills you hold. Its commands
live under `cashctl cash pocket`. They are newer than AGENTS.md, so don't expect
to find them there — lean on `--help` (start at `cashctl cash pocket --help`),
the `CHANGELOG.md`, and the skills you installed.

A couple of bills addressed to you are in `/fixtures/r9-bills.txt`, one per
line. A relay you can reach is in `/fixtures/r9-relay.txt`.

Work through all of this, and for each command record the exact exit code and
what it printed:

1. First, run `cashctl cash pocket` with no subcommand to see what the pocket
   offers.
2. Receive every bill in `/fixtures/r9-bills.txt` so your pocket is not empty.
3. Show your pocket **balance** — once as normal text, once with `--json` — then
   list the held **entries** the same two ways. Note what each one reports (and
   whether either touches the network — one of these commands is not supposed
   to).
4. **Export** your pocket to a file (e.g. `/home/evaluser/work/pocket.json`),
   once as text and once with `--json`; then **import** that same file back,
   again once as text and once with `--json`. Say how many entries each move
   reported.
5. **Sync** your pocket to the relay in `/fixtures/r9-relay.txt` — once as text,
   once with `--json`. This is the only pocket command that is meant to contact
   a relay; say what it reported publishing and pulling back.
6. Now make each of the five runnable pocket commands fail **once**, on purpose,
   and record the exit code, the `code`, and whether the message tells you what
   to do instead:
   - give `balance` a stray positional argument it doesn't accept;
   - give `entries` a stray positional argument it doesn't accept;
   - run `export` with no file path;
   - `import` a path that does not exist;
   - run `sync` without naming any relay.
7. Do **not** run `cashctl cash pocket serve` to completion: it is a
   long-running "be your own relay" daemon that opens a socket and never
   returns. If you are curious, read only its `--help`.

Write your self-report to `/report/r9-cash-pocket.self-report.json` (schema:
`/rounds/_report-schema.json`): what you now hold in the pocket, what sync did,
and any command whose behaviour or error wasn't what `--help` led you to expect.
