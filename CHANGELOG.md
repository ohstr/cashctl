# Changelog

## [0.6.0]

- **Breaking:** `cash list-recipients` is now `cash status`, following the Hub dropping the alias.
- **Breaking:** `redeem --transport` is gone. Redeems travel over the private transport only and batch automatically — one relay event per Hub, no flag and no fallback. The flag had already stopped being read, so it was accepted and ignored; it is now rejected. ([#24](https://github.com/ohstr/cashctl/pull/24))
- **Breaking:** bill methods (`redeem`, `consolidate`, `cash status`) are served over the private transport only and never fall back, so this release needs a Hub that serves them that way.
- `transfer --to cash` and `consolidate --to cash` write the destination bill's cash secret to the ledger *before* placing the call, not after the reply. That secret is generated locally and only a one-way commitment of it ever reaches the Hub, so a Ctrl-C or crash mid-flight used to destroy the only copy in existence and leave a bill funded and unspendable by anyone. `wallet show` now lists any such unfinished send, with the secret, so an interrupted one can still be recovered with the Hub's help. (Audit finding D-CLI-1.)
- `redeem` takes several tokens in one command: `--token` is repeatable and comma-separated, and `--all` redeems every held token. Each token is paid into its own invoice, so `--invoice` still accepts only one.
- `redeem` and `consolidate` attempt and report every selected token or group instead of stopping at the first failure, and exit nonzero if any failed. A nonzero exit from either no longer means nothing happened — stdout still carries the complete result.
- Redeems against the same Hub, fee quotes included, go out in one relay event rather than one per bill, so a holder's bills are no longer published as a linkable set of individually tagged events.
- `consolidate` groups bills by issuing Hub instead of by minter. One node can run several Hubs, so an ordinary `consolidate` could be refused outright with "all sources must belong to the same Cash Hub".
- `transfer` reports cross-Hub sources as fragmented funds, naming the held total and the requested amount, instead of passing the Hub's internal grouping rule through as if the input were malformed.
- `receive` asks the Hub before refusing a cash-mode bill on the token's `identity_required` hint. The hint goes stale when a bill is reassigned in place, which left a bill its rightful holder could not receive at all.
- `receive`'s merge path writes the destination secret down before the call, as `wallet protect` and the no-merge path already did.
- Auto-protect keeps the wallet the Hub carves for a re-key. It used to discard that result, leaving the ledger pointing at a wallet the carve had drained and deleted, and every later command addressing a bill that no longer existed.
- `cash status` on a spent bill prints the Hub's tombstone instead of nothing at exit 0 — this is the recovery step cashctl itself prescribes after a redeem whose reply never arrived.
- `consolidate` no longer writes a live source off as consumed. "This token does not name you" was being read as proof the bill was gone, which hid still-spendable bills from `redeem`, `wallet balance` and `--token` resolution.
- Every Hub-supplied amount is bounded before it reaches the ledger, so whoever answers `cash_status` on a token's own relays cannot persist an arbitrary "verified" balance.
- `decode --check` on a `<token>#<secret>` string can verify the bill it was handed. Bill methods authorize per item now, so even a read has to say who is asking.
- A Hub that answers nothing for a bill now gets text naming every possibility rather than asserting one. The most likely is that the bill simply doesn't name you, and the Hub cannot tell that apart from "no such bill" without confirming a guess.
- `wallet protect` rejects `-c/--connection` instead of silently ignoring it. It re-keys through the holding's own Hub and never dials a registered wallet. ([#24](https://github.com/ohstr/cashctl/pull/24))
- Two `cashctl` processes that open a pre-0.6.0 ledger at the same instant no longer race on this release's new column: both read it as missing, both added it, and the loser failed with `duplicate column name` for a ledger the winner had just migrated correctly. The column add now takes the same write lock the 0.4.0 column rename already took.
- The local ledger runs in SQLite's WAL mode, so a read and a write no longer block each other — the contention this wallet's Load/network-call/Save shape invites. Durability is unchanged (`synchronous` stays at its default), and a filesystem that cannot support WAL, which includes most network mounts, still opens exactly as before. ([#32](https://github.com/ohstr/cashctl/pull/32))
- The ledger's journal files (`cashctl.db-wal`, `-shm`, `-journal`) are restricted to 0600 like the database itself. They hold the same plaintext spending secrets, and `-wal` is persistent where `-journal` was transient, so a backup tool globbing `cashctl.db*` could have carried a readable copy off the machine. ([#32](https://github.com/ohstr/cashctl/pull/32))
- A second `cashctl` process can no longer silently undo a change the first one made. Each held-token row carries a version, and a write whose row moved underneath it is refused — with nothing overwritten and a message saying to re-run — instead of reverting the other process's work. Only a row both processes touched is affected; a row just one of them changed was already safe. An existing ledger picks up the new column on first open, with its stored secrets untouched.
- Bumped `nmilat` to v0.5.0-rc.2.

## [0.5.0]

- `--yes` no longer turns on `decode`'s and `receive`'s "verify online" check. That prompt is an opt-in to a network call you didn't ask for, not a confirmation, so skipping it now takes its default (no). `--check` still opts in. ([#20](https://github.com/ohstr/cashctl/pull/20))
- `cashctl join <hub> 0` says 0 isn't a spend cap instead of claiming the amount is missing. There is no "0 means unlimited" convention here. ([#20](https://github.com/ohstr/cashctl/pull/20))

## [0.4.1]

- `consolidate` given an already-spent source (redeemed, transferred or consolidated) says so at once instead of dialling a Hub that never answers and timing out after 30s as a retryable `network` failure. `redeem --token` already did this. ([#18](https://github.com/ohstr/cashctl/pull/18))
- A failed auto-protect on a received cash gift now points at `cashctl wallet protect`. It named `consolidate --to cash`, which cannot re-key a single holding. ([#18](https://github.com/ohstr/cashctl/pull/18))
- `cashctl --help`'s `join` example now shows the required max amount, so copying it works. ([#18](https://github.com/ohstr/cashctl/pull/18))

## [0.4.0]

- **Breaking:** NIP-CASH's "bearer" mode is now "cash mode" on the wire, so this release needs a Hub that speaks the renamed protocol (lokihub 0.5.0-rc.6 or later). Older Hubs reject its cash-mode requests. ([#8](https://github.com/ohstr/cashctl/pull/8))
- **Breaking:** two `--json` keys are renamed: `bearer_protection` becomes `cash_protection` (`wallet show`, `receive`, `transfer`, `consolidate`), and `decode`'s `embedded_bearer_secret_present` becomes `embedded_cash_secret_present`. ([#8](https://github.com/ohstr/cashctl/pull/8))
- **Breaking:** `--as` now takes `cash:<secret>` instead of `bearer:<secret>`, and `transfer`'s literal target keyword is `cash`, not `bearer`. ([#9](https://github.com/ohstr/cashctl/pull/9))
- The ledger migrates itself the first time this version opens it: the `bearer_secret`, `pending_bearer_secret` and `bearer_protection` columns are renamed to `cash_secret`, `pending_cash_secret` and `cash_protection`. Stored values, including every spending secret, are carried across untouched, and held tokens stay spendable. ([#8](https://github.com/ohstr/cashctl/pull/8))
- Downgrading afterwards is not supported: an older cashctl stops with "no such column: bearer_secret" rather than reading a half-renamed ledger. It could not reach a renamed Hub anyway. ([#8](https://github.com/ohstr/cashctl/pull/8))
- Bumped `nmilat` to the cash-mode API (`NewCashTarget`, `RekeyCashSlice`, `CashSecret`, `IsCash`). ([#10](https://github.com/ohstr/cashctl/pull/10))
- Bumped `ncli` to v0.6.0 and switched to its new `client/vault` package, dropping 22 indirect modules (tview, tcell, viper and the rest of ncli's CLI stack). ([#15](https://github.com/ohstr/cashctl/pull/15))
- A command given `--token` for a holding already spent (redeemed, transferred or consolidated) says so at once instead of dialling a Hub that will never answer and timing out after 15s. ([#14](https://github.com/ohstr/cashctl/pull/14))
- When a Hub stays silent about a holding, the error now says the bill has most likely been spent or expired rather than reporting a bare network failure. It is still reported as retryable, since an unreachable Hub looks the same from here. ([#14](https://github.com/ohstr/cashctl/pull/14))

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
