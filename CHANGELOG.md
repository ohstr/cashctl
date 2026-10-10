# Changelog

## [0.6.0-rc.10]

A dependency update.

- Updates `nmilat` to v0.5.0-rc.16; cashctl's behavior is unchanged.
  ([#65](https://github.com/ohstr/cashctl/pull/65))

## [0.6.0-rc.9]

A dependency update built with Go 1.26.9.

- Builds with Go 1.26.9, which fixes nine standard-library vulnerabilities,
  and updates `nmilat` to v0.5.0-rc.14; cashctl's behavior is unchanged.
  ([#64](https://github.com/ohstr/cashctl/pull/64))

## [0.6.0-rc.8]

A dependency update.

- Updates `nmilat` to v0.5.0-rc.13; cashctl's behavior is unchanged.
  ([#61](https://github.com/ohstr/cashctl/pull/61))

## [0.6.0-rc.7]

A dependency update.

- Updates `nmilat` to v0.5.0-rc.12; cashctl's behavior is unchanged.
  ([#60](https://github.com/ohstr/cashctl/pull/60))

## [0.6.0-rc.6]

Agent skills ship inside the binary.

- Adds `cashctl skills list|show|install`, so an agent reads the guidance
  for the version it runs. ([#58](https://github.com/ohstr/cashctl/pull/58))
- Updates `nmilat` to v0.5.0-rc.11; cashctl's behavior is unchanged.
  ([#59](https://github.com/ohstr/cashctl/pull/59))

## [0.6.0-rc.5]

A floating Docker tag for release candidates.

- Adds `ghcr.io/ohstr/cashctl:rc`, which tracks the newest release
  candidate; `:latest` still moves only on stable releases.
  ([#54](https://github.com/ohstr/cashctl/pull/54))
- Updates `nmilat` to v0.5.0-rc.7; cashctl's behavior is unchanged.
  ([#55](https://github.com/ohstr/cashctl/pull/55))

## [0.6.0-rc.4]

A fix for stranded wallet balances.

- Fixes `wallet balance --breakdown` dropping a wallet's cached balance when
  the Hub declines with `RESTRICTED` or `UNAUTHORIZED`; it now shows as
  stranded, as with `EXPIRED`. ([#51](https://github.com/ohstr/cashctl/pull/51))
- Updates `nmilat` to v0.5.0-rc.5; cashctl's behavior is unchanged.
  ([#49](https://github.com/ohstr/cashctl/pull/49))

## [0.6.0-rc.3]

Expired cash no longer drags a healthy bill down with it.

- Refuses to merge an already-expired cash source with a healthy one in
  `consolidate` and `transfer`, since the result would inherit the earlier
  expiry; `receive` leaves the expired sibling out instead.
  ([#41](https://github.com/ohstr/cashctl/pull/41))
- Updates `nmilat` to v0.5.0-rc.4; cashctl's behavior is unchanged.
  ([#47](https://github.com/ohstr/cashctl/pull/47))

## [0.6.0-rc.2]

Bill methods move to NIP-CASH's private transport, `redeem` learns to
select by amount and spend many bills at once, and a security review
hardens output, NIP-05 lookups and the ledger.

- Adds `redeem --amount <n>`, which redeems held tokens that net exactly
  that amount, from one token or several on the same Hub.
  ([#38](https://github.com/ohstr/cashctl/pull/38))
- Requires `--amount` with an amount-less `--invoice` and refuses it with a
  fixed-amount one. ([#38](https://github.com/ohstr/cashctl/pull/38))
- Changes `wallet protect` and `cash status` to act on every eligible token
  when several qualify, instead of refusing as ambiguous.
  ([#38](https://github.com/ohstr/cashctl/pull/38))
- Adds several tokens per `redeem`: `--token` repeats and `--all` redeems
  everything held. ([43dce48](https://github.com/ohstr/cashctl/commit/43dce48))
- Changes `redeem` and `consolidate` to try every selected token or group and
  report each, exiting nonzero if any failed.
  ([bfac7f4](https://github.com/ohstr/cashctl/commit/bfac7f4))
- Sends a Hub's redeems and fee quotes in one relay event, so a holder's bills
  no longer appear as a linkable set.
  ([b97fc9a](https://github.com/ohstr/cashctl/commit/b97fc9a))
- Serves bill methods (`redeem`, `consolidate`, `cash status`) over the
  private transport only, with no fallback. This breaks against a Hub that
  doesn't serve them that way. ([311caf3](https://github.com/ohstr/cashctl/commit/311caf3))
- Renames `cash list-recipients` to `cash status`, following the Hub. This
  breaks scripts that call the old name.
  ([311caf3](https://github.com/ohstr/cashctl/commit/311caf3))
- Removes `redeem --transport`, which was accepted and ignored; it is now
  rejected. ([#24](https://github.com/ohstr/cashctl/pull/24))
- Rejects `wallet protect -c/--connection`, which it never used.
  ([#24](https://github.com/ohstr/cashctl/pull/24))
- Groups bills for `consolidate` by issuing Hub instead of by minter, which
  could make an ordinary `consolidate` fail.
  ([3cd3499](https://github.com/ohstr/cashctl/commit/3cd3499))
- Reports cross-Hub sources in `transfer` as fragmented funds, naming the
  held total and the amount asked for.
  ([15aa5ec](https://github.com/ohstr/cashctl/commit/15aa5ec))
- Writes a new bill's cash secret to the ledger before the call in
  `transfer --to cash`, `consolidate --to cash` and `receive`, so a crash
  mid-call can't lose it; `wallet show` lists any unfinished send.
  ([d1cf267](https://github.com/ohstr/cashctl/commit/d1cf267))
- Prints the full `<token>#<cash_secret>` when a local save fails after the
  Hub moved money, as `recovery` under `--json`; it was being redacted.
  ([#27](https://github.com/ohstr/cashctl/pull/27))
- Fixes `receive` refusing a cash-mode bill on a stale `identity_required`
  hint; it now asks the Hub.
  ([769addc](https://github.com/ohstr/cashctl/commit/769addc))
- Fixes auto-protect pointing the ledger at a drained wallet, `consolidate`
  hiding still-spendable sources, and `cash status` printing nothing for a
  spent bill. ([43d46a9](https://github.com/ohstr/cashctl/commit/43d46a9))
- Bounds every Hub-supplied amount before it reaches the ledger.
  ([9a91411](https://github.com/ohstr/cashctl/commit/9a91411))
- Lets `decode --check` verify a `<token>#<secret>` bill, and explains a
  silent Hub without guessing.
  ([311caf3](https://github.com/ohstr/cashctl/commit/311caf3))
- Strips terminal control characters from everything cashctl prints, so a
  Hub's text can't inject escape sequences into output or `--json`.
  ([#30](https://github.com/ohstr/cashctl/pull/30), [#36](https://github.com/ohstr/cashctl/pull/36))
- Stops a NIP-05 lookup following redirects, caps its response at 256 KiB,
  and drops the server's status text from errors.
  ([#28](https://github.com/ohstr/cashctl/pull/28))
- Keeps a connection string out of `wallet history`, and accepts `--as` from
  `CASHCTL_AS` so it stays out of argv and shell history.
  ([#34](https://github.com/ohstr/cashctl/pull/34))
- Runs the ledger in SQLite's WAL mode and restricts its journal files to
  the owner. ([#32](https://github.com/ohstr/cashctl/pull/32))
- Refuses a save whose row another process changed, instead of reverting
  that change. ([#35](https://github.com/ohstr/cashctl/pull/35))
- Fixes two processes opening an old ledger at once failing with
  `duplicate column name`.
  ([25d70a7](https://github.com/ohstr/cashctl/commit/25d70a7))
- Updates `nmilat` to v0.5.0-rc.3; cashctl's behavior is unchanged.
  ([#39](https://github.com/ohstr/cashctl/pull/39))

## [0.5.0]

Clearer behavior for `--yes` and a zero spend cap.

- Stops `--yes` turning on `decode`'s and `receive`'s network check; only
  `--check` does. ([#20](https://github.com/ohstr/cashctl/pull/20))
- Explains that 0 isn't a spend cap when `cashctl join <hub> 0` is given,
  instead of saying the amount is missing.
  ([#20](https://github.com/ohstr/cashctl/pull/20))

## [0.4.1]

Fixes for spent sources and protection advice.

- Refuses an already-spent `consolidate` source at once, instead of timing
  out as a retryable network error.
  ([#18](https://github.com/ohstr/cashctl/pull/18))
- Points a failed auto-protect at `cashctl wallet protect`.
  ([#18](https://github.com/ohstr/cashctl/pull/18))
- Fixes `cashctl --help`'s `join` example, which lacked the required max
  amount. ([#18](https://github.com/ohstr/cashctl/pull/18))

## [0.4.0]

NIP-CASH's bearer mode becomes cash mode; this release needs lokihub
0.5.0-rc.6 or later.

- Renames NIP-CASH's bearer mode to cash mode on the wire. This breaks
  against Hubs older than lokihub 0.5.0-rc.6.
  ([#8](https://github.com/ohstr/cashctl/pull/8))
- Renames the `--json` keys `bearer_protection` to `cash_protection` and
  `embedded_bearer_secret_present` to `embedded_cash_secret_present`. This
  breaks scripts that read the old keys.
  ([#8](https://github.com/ohstr/cashctl/pull/8))
- Renames `--as bearer:<secret>` to `--as cash:<secret>` and the `bearer`
  transfer target to `cash`. This breaks scripts that use the old forms.
  ([#9](https://github.com/ohstr/cashctl/pull/9))
- Migrates the ledger on first open; spending secrets carry over and held
  tokens stay spendable. Downgrading afterwards is not supported.
  ([#8](https://github.com/ohstr/cashctl/pull/8))
- Refuses `--token` for a holding already spent, instead of timing out.
  ([#14](https://github.com/ohstr/cashctl/pull/14))
- Explains a silent Hub as a bill that is most likely spent or expired.
  ([#14](https://github.com/ohstr/cashctl/pull/14))
- Updates `ncli` to v0.6.0 and imports only its vault, dropping 22 indirect
  modules. ([#15](https://github.com/ohstr/cashctl/pull/15))
- Updates `nmilat` to v0.4.0 for the cash-mode API.
  ([#10](https://github.com/ohstr/cashctl/pull/10))

## [0.3.0]

Safer cash handling, `wallet protect`, and a cleaner CLI contract.

- Adds `wallet protect [id]`, which re-keys a cash holding whose automatic
  protection was declined or failed. ([#7](https://github.com/ohstr/cashctl/pull/7))
- Returns the recipient's token and secret from a whole-token cash
  `transfer`. ([#7](https://github.com/ohstr/cashctl/pull/7))
- Prints a recovery hint when a local save fails after the Hub moved money.
  ([#7](https://github.com/ohstr/cashctl/pull/7))
- Saves a cash secret before using it and reconciles an ambiguous
  `consolidate` with the Hub. ([#7](https://github.com/ohstr/cashctl/pull/7))
- Bounds the Hub-reported amount in `receive` and checks it against the
  token's signed amount. ([#7](https://github.com/ohstr/cashctl/pull/7))
- Keeps connection values out of `--json` and labels, and strengthens
  secret redaction and text sanitizing.
  ([#7](https://github.com/ohstr/cashctl/pull/7))
- Confirms and previews the amount in `pay`, and keeps `decode` offline by
  default. ([#7](https://github.com/ohstr/cashctl/pull/7))
- Shows expired and unreachable wallets explicitly in `wallet balance`.
  ([#7](https://github.com/ohstr/cashctl/pull/7))
- Exits 2 for a bare or mistyped group command, and sends prompts and the
  spinner to stderr. ([#7](https://github.com/ohstr/cashctl/pull/7))
- Fixes concurrent `init` and config saves overwriting each other.
  ([#7](https://github.com/ohstr/cashctl/pull/7))
- Updates `nmilat` to v0.3.1. ([#7](https://github.com/ohstr/cashctl/pull/7))

## [0.2.0]

Amounts in loki and simpler argument handling.

- Changes every amount (`transfer`, `join --max-amount`,
  `consolidate --sources`, `invoice`) from mloki to loki.
  ([#2](https://github.com/ohstr/cashctl/pull/2))
- Accepts `transfer`'s and `join`'s positional arguments in either order;
  `join` requires its cap. ([#2](https://github.com/ohstr/cashctl/pull/2))
- Groups held tokens by minter in an argument-less `consolidate`.
  ([#2](https://github.com/ohstr/cashctl/pull/2))
- Renames `circle create` to `circle join`, and trims help text and prompts.
  ([#2](https://github.com/ohstr/cashctl/pull/2))

## [0.1.0]

SQLite storage, cash selection and cash gift strings.

- Stores the wallet in one SQLite database (`cashctl.db`).
  ([#1](https://github.com/ohstr/cashctl/pull/1))
- Adds cash selection to `transfer --amount`, consolidating when no single
  token covers it. ([#1](https://github.com/ohstr/cashctl/pull/1))
- Accepts cash gift strings (`<token>#<secret>`) in `receive` and protects
  the receipt. ([#1](https://github.com/ohstr/cashctl/pull/1))
- Warns before acting on an expired token, and accounts for the Hub's
  redeem fee; `redeem --to` becomes `--into`.
  ([#1](https://github.com/ohstr/cashctl/pull/1))
- Supports `nconnection1…` and NIP-05 targets.
  ([#1](https://github.com/ohstr/cashctl/pull/1))
- Fixes a lost update on concurrent saves, a transfer discarding its own
  secret, a secret leaking into `--json`, and a duplicate-receive race.
  ([#1](https://github.com/ohstr/cashctl/pull/1))

## [0.0.1]

The first release.

- Adds `init`, `join`, `wallet`, `connect`, `receive`, `redeem`,
  `transfer`, `consolidate`, `cash` and `version`.
  ([cd87d35](https://github.com/ohstr/cashctl/commit/cd87d35))
