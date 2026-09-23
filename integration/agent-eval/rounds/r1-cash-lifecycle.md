# Round R1 — Cash lifecycle

`cashctl` should already be installed (an earlier round did this; if it
somehow isn't, install it the same way
`https://ohstr.github.io/cashctl/PROMPT.md` describes before continuing).
Use `cashctl --help` / `cashctl <cmd> --help`, and re-fetch
`https://raw.githubusercontent.com/ohstr/cashctl/main/AGENTS.md` if you need
the command/flag contract again.

You've been handed two fixtures, mounted read-only:
- `/fixtures/r1-cash-token.txt` — a real NIP-CASH token
- `/fixtures/r1-wallet-uri.txt` — a plain Lightning wallet connection (NWC)
  you can redeem into

1. Make sure you have an identity: `cashctl init --json` (fine to re-run if
   an earlier round already did this — it should say so, not error).
2. Inspect the token **without** touching your wallet yet:
   `cashctl decode "$(cat /fixtures/r1-cash-token.txt)"`.
3. Now actually receive it into your wallet (this always cross-checks
   against the Hub before saving anything):
   `cashctl receive "$(cat /fixtures/r1-cash-token.txt)"`.
4. Check its co-recipients: `cashctl cash list-recipients`.
5. Register the wallet fixture as a named connection called `payout`:
   `cashctl connect add payout "$(cat /fixtures/r1-wallet-uri.txt)"`.
6. Redeem the token you received in step 3 into `payout`:
   `cashctl redeem --into payout --yes`.
7. Confirm it: `cashctl wallet balance --breakdown` and `cashctl wallet
   history` — the token should no longer show up as a held token, and the
   history should record the redeem.

Write your self-report to `/report/r1-cash-lifecycle.self-report.json`. Follow `/rounds/_report-schema.json` exactly — in particular the
top-level `outcome` ("pass" | "partial" | "fail") and `summary` keys, which
the harness reads to compare your own account against its independent check.
Include the exact `redeem` output (it should contain a Lightning payment
preimage on success) and whether `wallet balance --breakdown` matched what
you expected after redeeming.
