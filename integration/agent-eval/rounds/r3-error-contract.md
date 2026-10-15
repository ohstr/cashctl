<!-- clauses: (cashctl's own error contract: AGENTS.md "Output conventions") -->
# Round R3 — Error contract

`cashctl` should already be installed. Read AGENTS.md's "Output conventions"
section first: it says what every failure looks like (stderr JSON shape, `code`,
exit code, `retryable`).

Run each of these under `--json`, record the exit code and the exact stderr, and
check each against that table:

1. a command that does not exist (`cashctl bogus-command`)
2. a flag that does not exist (`cashctl wallet show --bogus-flag`)
3. `cashctl join` with nothing else
4. `cashctl transfer` with nothing else — and note what the message says you can do
5. `cashctl decode not-a-valid-token`
6. `cashctl skills show nope`
7. `cashctl connect use nope`
8. `cashctl receive -c anything <a token>` (use the contents of `/fixtures/r3-token.txt`)
9. registering `/fixtures/r3-circlehub.txt` as an ordinary wallet with `connect add`

For each: does the exit code match the table for its `code`? Is stdout empty?
Does `retryable` match? Does the message tell you what to do instead? Report
every mismatch in `issues`, and anything unclear in `ux_friction`.

Write your self-report to `/report/r3-error-contract.self-report.json` (schema:
`/rounds/_report-schema.json`), one `steps` entry per probe.
