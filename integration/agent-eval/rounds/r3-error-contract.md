# Round R3 — Error contract

`cashctl` should already be installed. Fetch
`https://raw.githubusercontent.com/ohstr/cashctl/main/AGENTS.md` and read
its "Output conventions" section carefully before starting — it documents
exactly what every command's failure should look like.

This round needs no network access and no fixtures. For **each** of the
commands below, run it exactly as given (all under `--json`), and record
its exit code and its exact stderr JSON:

1. `cashctl bogus-command --json` — a command that doesn't exist
2. `cashctl wallet show --bogus-flag --json` — a flag that doesn't exist
3. `cashctl join --json` — a required flag (`--hub`) missing
4. `cashctl transfer --json` — a required flag (`--to`) missing
5. `cashctl decode not-a-valid-token --json` — a value that fails to parse
6. `cashctl redeem --json` — a legitimate "nothing to act on" case. Do not
   assume the wallet is empty: check with `cashctl wallet show --json` first,
   and if you do hold a cash token, say so in `issues` and report what
   `redeem --json` actually returned instead of forcing the expected answer
7. `cashctl wallet balance --from does-not-exist --json` — a named
   thing that doesn't exist

For each one, check: does the exit code match AGENTS.md's table for the
`code` field in the JSON output? Is the JSON shape exactly `{"error",
"code", "retryable", "input"?}` (no extra top-level keys, nothing on
stdout)? Does `retryable` match the table? Flag any mismatch as an issue
in your self-report — don't just record the raw output uncritically.

Write your self-report to `/report/r3-error-contract.self-report.json`
(schema: `/rounds/_report-schema.json` — include its top-level `outcome`
and `summary` keys),
with one entry in `steps` per command above (put the exit code in
`exit_code` and the raw stderr JSON in `result`), and list every contract
mismatch you found in `issues` (empty array if none).
