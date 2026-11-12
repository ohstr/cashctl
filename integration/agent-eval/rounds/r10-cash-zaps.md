<!-- clauses: CZ-sending-a-cash-zap, CZ-countable-zap-of-an-event, CZ-connection-key-zap, CZ-zap-info-show-dry-run, CZ-zap-info-publish -->
# Round R10 — Send a Cash Zap and publish what you accept

`cashctl` should already be installed and set up, with an identity. A **Cash
Zap** (NIP-CZ) pays someone from a held, pubkey-bound cash token and publishes a
kind-9522. The sending side (`cashctl cash zap send`) and your acceptance notice
(`cashctl cash zap-info`) are newer than AGENTS.md and the skills — which only
cover *receiving* a zap — so lean on `--help` (start at
`cashctl cash zap send --help` and `cashctl cash zap-info --help`) and the
`CHANGELOG.md`.

A few bills addressed to you are in `/fixtures/r10-bills.txt`, one per line — the
tokens you will zap from. The person to zap is the npub in
`/fixtures/r10-recipient-npub.txt`. A relay you can reach is in
`/fixtures/r10-relay.txt`. For each command record the exact exit code and what
it printed.

1. First, run `cashctl cash zap` with no subcommand, and then `cashctl cash
   zap-info` with no subcommand, to see what each offers.
2. Receive every bill in `/fixtures/r10-bills.txt` so you have tokens to zap
   from.
3. Send a Cash Zap of **5 loki** to the recipient in
   `/fixtures/r10-recipient-npub.txt`, naming the amount and the recipient with
   the `--amount` and `--to` flags. Do it once showing normal text output, and
   once with `--json`. Record the zap event id and whether the zap was
   `countable`.
4. Send some more zaps from your held tokens, each one exercising a **different**
   option, and record what each does — several may be refused, which is a
   perfectly good thing to record as long as you say what the tool reported:
   - add a comment to the zap with `--content`;
   - also publish the 9522 to the relay in `/fixtures/r10-relay.txt` using
     `--relay`;
   - pick exactly which held token to zap from with `--token <id>` (find a token
     id from `cashctl cash pocket entries` or `cashctl cash status`);
   - make it a *countable* zap of a specific nostr event with `--event <file>`
     (write a tiny event JSON to a file first, or pass one you have);
   - override the credential used with `--as`;
   - zap a **connection-key** recipient instead of a pubkey, with `--platform`
     (e.g. `discord`) and `--ia` (the Identity Authority vouching for it).
5. Make `cash zap send` fail on purpose at least once and record the exit code
   and `code` — for example, a non-numeric amount, or no recipient at all.
6. Now your **Cash Zap Info** (kind 10522 — the chains, minters and relays you
   accept zaps on):
   - **Show** what `publish` would sign without sending anything, once as text
     and once with `--json`. Then show it again adding a chain with `--chain`
     and a relay with `--relay`, and say how the output changes.
   - Also make `show` fail once (e.g. a stray argument) and record it.
   - **Publish** it for real to the relay in `/fixtures/r10-relay.txt` with
     `--relay` and a `--chain`, once as text and once with `--json`. It is public
     and asks first, so you may pass `--yes`.
   - Make `publish` fail once and record it — for instance, publishing with no
     relay for it to go to.

Write your self-report to `/report/r10-cash-zaps.self-report.json` (schema:
`/rounds/_report-schema.json`): which zaps you sent, which were countable and
which were refused and why, and what your published Cash Zap Info ended up
saying.
