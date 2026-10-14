<!-- clauses: CASH-redeeming-a-slice, CASH-security-considerations-for-cash-03 (auto-protect re-key), CASH-cash-status -->
# Round R1 — Cash out a note

`cashctl` should already be installed and set up. Use its `--help`, AGENTS.md
(`https://raw.githubusercontent.com/ohstr/cashctl/main/AGENTS.md`) and the skills
you installed.

Someone handed you a cash note: `/fixtures/r1-note.txt`. Get its money into the
Lightning wallet whose connection is in `/fixtures/r1-cash-note-wallet-uri.txt`, and
show that it arrived.

Write your self-report to `/report/r1-cash-note.self-report.json` (schema:
`/rounds/_report-schema.json`): what you checked before taking the note, how
you moved the money, and the evidence that it landed.
