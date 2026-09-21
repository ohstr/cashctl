# Changelog

## [0.3.0] ([#7](https://github.com/ohstr/cashctl/pull/7))

- New `wallet protect [id]` re-keys a still-shared bearer holding when `receive`'s auto-protect was declined or failed.
- A whole-token bearer `transfer` now hands back the recipient's token and secret.
- A ledger write that fails after the Hub moved money is reported with a recovery hint (`transfer`, `consolidate`, `redeem`).
- A bearer secret is saved before it's used and retried at spend time, so an interrupted protect can't lose it.
- An ambiguous `consolidate` delivery is checked against the Hub instead of guessed.
- `receive` bounds the Hub-reported amount and checks it against the token's signed amount; oversized amounts are rejected.
- Wallet connection values never appear in `--json` output or as display labels, and secret redaction in errors is stronger.
- `pay` now confirms and previews the amount before sending.
- `decode` no longer goes online by default; its `--check` prompt defaults to no.
- Pasting a Nostr key (`npub`, `nsec`, ...) into `decode`/`receive` gets a clear message instead of a TLV error.
- `wallet balance` leaves expired held tokens out of the total (still listed, marked expired) and names unreachable wallets instead of dropping them.
- `wallet balance --from` on a spent token is `not_found`, not its old amount.
- Expired-token errors say the token expired, not the wallet; human-mode errors keep the Hub's actual reason.
- Concurrent `init` and config saves no longer overwrite each other; the losing `init` reports `already_configured`.
- A bare `cash`/`wallet`/`connect`/`circle` or a mistyped subcommand exits 2.
- `-c/--connection` on commands it can't apply to is a usage error; an explicit `0` amount is a usage error.
- Token IDs match case-insensitively; `wallet show` works without an identity; `init --json` never adopts an ncli vault.
- `--yes` no longer replaces an existing default wallet (`connect add`, `join`).
- Prompts, pick-lists and the spinner go to stderr; results stay on stdout.
- Hub/wallet text is sanitized more thoroughly (control and bidi characters), including in `list-tx`, `budget` and `join`.
- Bumped `nmilat` to v0.3.1.

## [0.2.0] ([#2](https://github.com/ohstr/cashctl/pull/2))

- All amounts (`transfer`, `join --max-amount`, `consolidate --sources`,
  `invoice`) are now in loki, not mloki.
- `transfer` no longer narrates the split remainder ("keep X") — it just
  confirms and reports the amount sent.
- `transfer`'s target/amount positional args work in either order.
- `join` takes its spend cap positionally too, in either order; a cap is
  now required instead of silently failing at the Hub.
- `consolidate` with no args auto-groups held tokens by minter and merges
  each group, instead of only doing so as a single call across everything held.
- `circle create` renamed to `circle join`, matching the top-level `join`.
- Trimmed verbose help text and prompts across most commands.
- Fixed a stale hint pointing at `wallet show` instead of `wallet show --json` for a held token's ID.

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
