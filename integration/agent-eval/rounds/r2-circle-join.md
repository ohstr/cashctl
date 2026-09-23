# Round R2 — Joining a circle

`cashctl` should already be installed. Use `cashctl --help` / `cashctl <cmd>
--help`, and re-fetch
`https://raw.githubusercontent.com/ohstr/cashctl/main/AGENTS.md` if needed.

Joining a circle needs the Hub operator to authorize your specific
identity first — so this round has two parts, in order:

1. Make sure you have an identity: `cashctl init --json`. Whatever npub it
   reports (or, if it says you're already configured, the npub from
   `cashctl wallet show --json`), write **just that npub string, nothing
   else** to `/report/r2-npub.txt`.
2. Now wait for `/fixtures/r2-hub.txt` to appear — it won't exist yet.
   Poll for it (e.g. `sleep 3` in a loop, checking with `[ -f ... ]`) for
   up to a few minutes. Once it exists, it contains a Circle Hub
   connection string that has just been authorized for the npub you wrote
   in step 1.
3. Join it: `cashctl join --hub "$(cat /fixtures/r2-hub.txt)" --max-amount
   500 --yes`. (The hub caps a wallet at 1000 loki and is funded to match,
   so this is comfortably inside both.)
4. Confirm the resulting wallet is live: `cashctl wallet get-info` and
   `cashctl wallet show`.

Write your self-report to `/report/r2-circle-join.self-report.json`. Follow `/rounds/_report-schema.json` exactly — in particular the
top-level `outcome` ("pass" | "partial" | "fail") and `summary` keys, which
the harness reads to compare your own account against its independent check.
Include how long you had to wait in step 2, and the exact `join` output.
