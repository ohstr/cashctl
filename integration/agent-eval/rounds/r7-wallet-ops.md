<!-- clauses: CW-in-circle-transfers, (ordinary NIP-47 calls) -->
# Round R7 — Everyday wallet work

`cashctl` should already be installed, with the circle wallet you joined in an
earlier round.

1. Register the wallet in `/fixtures/r7-wallet-ops-a-uri.txt` under the name `a`.
2. Create a 21-loki invoice on `a`, and pay it from your circle wallet.
3. Show that `a` received it.
4. Sign the message `cashctl eval r7` with your circle wallet, if it can.
5. Make `a` your default wallet.

Write your self-report to `/report/r7-wallet-ops.self-report.json` (schema:
`/rounds/_report-schema.json`), including the invoice, the payment's outcome and
the signature (or why there is none).
