<!-- clauses: CASH-splitting-a-slice, CASH-presenting-a-cash-mode-slice-as-one-string -->
# Round R4 — Send part of a note

`cashctl` should already be installed and set up.

You were given a 90-loki cash note: `/fixtures/r4-note.txt`. Send 30 loki of it
to the person whose npub is in `/fixtures/r4-npub.txt`, and turn the rest into a
cash note you could hand to anyone. Write that note (the full string someone
would need to take it) to `/report/r4-note.txt`.

Write your self-report to `/report/r4-split-send.self-report.json` (schema:
`/rounds/_report-schema.json`): what you did, and what you now still hold.
