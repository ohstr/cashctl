# Changelog

## [0.1.0]

- Storage moved to a single SQLite database (`cashctl.db`), replacing three JSON files.
- `transfer --amount` picks which held token(s) to use, auto-consolidating same-minter tokens when none alone covers it.
- `receive` accepts NIP-CASH bearer gift-strings (`<token>#<bearer_secret>`) and auto-secures bearer receipts.
- `decode`/`redeem`/`transfer`/`consolidate` warn before an action that would fail on an expired token.
- `redeem` accounts for the Hub's redeem fee; `--to` renamed to `--into`.
- `nconnection1...` targets and NIP-05 identifiers are supported and validated before use.
- Fixed several bugs: a lost update on concurrent saves, a bearer-target transfer that discarded its own secret, a secret leak in `--json` output, and a duplicate-receive race.
- Wire text from a Hub/wallet is sanitized before printing; error messages are more specific and less generic.
- Circle hub fee terms and forwarding-fee skim are now visible in `wallet get-info`/`wallet pay`.

## [0.0.1] - 2026-09-09

- Initial release: `init`, `join`/`circle create`, `wallet` (show/history/use/get-info/balance/budget/invoice/pay/list-tx/sign-message), `connect` (add/list/use/rm), `receive`, `redeem`, `transfer`, `consolidate`, `cash` (list-recipients/decode/verify-provenance), `version`.
