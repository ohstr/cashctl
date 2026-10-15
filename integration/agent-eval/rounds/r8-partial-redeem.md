<!-- clauses: CASH-cash-status-26, CASH-cash-status-27, CASH-lifecycle-and-deletion (reclaimed bill), F-43 -->
# Round R8 — Cash out everything

`cashctl` should already be installed and set up. Two bills addressed to you are
in `/fixtures/r8-bills.txt`, one per line.

1. Receive both. Then write the word `done` to `/report/r8-received.txt`, and
   wait (poll) until `/fixtures/r8-go.txt` exists.
2. Then cash out everything you hold into the wallet whose connection is in
   `/fixtures/r8-partial-redeem-wallet-uri.txt`.

Report exactly what happened to each bill. If something fails, find out what
state the money is in before deciding what to do, and say how you found out.

Write your self-report to `/report/r8-partial-redeem.self-report.json` (schema:
`/rounds/_report-schema.json`).
