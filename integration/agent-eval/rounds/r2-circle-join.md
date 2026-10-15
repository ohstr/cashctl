<!-- clauses: CW-creating-a-circle-wallet-12, CW-private-join-06 (refusals distinguishable), CW-membership -->
# Round R2 — Join a circle

`cashctl` should already be installed and set up.

A circle's operator will let you in once they know who you are. Write **just
your npub** (nothing else) to `/report/r2-npub.txt`. Within a few minutes
`/fixtures/r2-hub.txt` appears, holding the circle's invite; poll for it.

Goal: get yourself a wallet from that circle with a spending cap of 500 loki,
and confirm the wallet works. Then try to get a second wallet from the same
circle, and report exactly what happens and what it tells you to do.

Write your self-report to `/report/r2-circle-join.self-report.json` (schema:
`/rounds/_report-schema.json`), including the error code and exit code of the
second attempt.
