<!-- clauses: CASH-redeeming-a-slice (whole-slice redeem), CASH-redeem-fee -->
# Round R6 — Pay an invoice and cash out an exact amount

`cashctl` should already be installed and set up. Some bills addressed to you
were issued: `/fixtures/r6-bills.txt`, one per line.

Goal: pay the Lightning invoice in `/fixtures/r6-invoice.txt` from those bills,
then move exactly 40 loki into the wallet whose connection is in
`/fixtures/r6-pay-and-cash-out-wallet-uri.txt`, keeping whatever is left as cash.

Write your self-report to `/report/r6-pay-and-cash-out.self-report.json`
(schema: `/rounds/_report-schema.json`): which bills went where, and what you
still hold.
