# Changelog

## [0.6.0-rc.15]

- Adds the `--json` error object's fields to `cashctl --help`; the cash skill
  now points there instead of AGENTS.md, which an installed binary doesn't have.
  ([#94](https://github.com/ohstr/cashctl/pull/94))
- Fixes `redeem --amount` picking a different Hub's bills from run to run when
  more than one Hub can land the amount exactly; it now always picks the same.
  ([#102](https://github.com/ohstr/cashctl/pull/102))
- Fixes a `cash` target carrying its new note's spending secret in the field
  meant as a public description; the secret is now only in the note handed over.
  ([#103](https://github.com/ohstr/cashctl/pull/103))
- Fixes a protect or cash-note move whose reply was lost leaving the bill shown
  as shared until a later spend: `redeem`, `transfer` and `consolidate` now ask
  the Hub first and record the secret that works.
  ([#104](https://github.com/ohstr/cashctl/pull/104))
- Fixes a cash note whose move off a merge key lost its reply possibly losing
  its only working secret: it is no longer moved again before that is settled.
  ([#104](https://github.com/ohstr/cashctl/pull/104))

## [0.6.0-rc.14]

cashctl gets its own logo; no CLI changes since rc.13.

- Adds the cashctl logo to the README and docs site, with a matching favicon
  and purple accent. ([#89](https://github.com/ohstr/cashctl/pull/89))

## [0.6.0-rc.13]

Fixes from the NIP-CASH / NIP-CW conformance review, for agents above all.

- Removes the `cash list-recipients` alias, following the spec's removal of
  `list_recipients`; use `cash status`.
  ([#70](https://github.com/ohstr/cashctl/pull/70))
- Adds the `indeterminate` error code (exit 8, not retryable) for a redeem,
  transfer or consolidate that got no answer, since resending it may pay
  twice; check with `cash status` first.
  ([#70](https://github.com/ohstr/cashctl/pull/70))
- Fixes a timed-out bill check claiming the bill was most likely spent, and
  an incomplete batch reply being reported as an omission.
  ([#70](https://github.com/ohstr/cashctl/pull/70))
- Fixes `connect add`, `-c`, `--into` and the `init` wallet prompt accepting a
  `circlehub1…`/`cashhub1…` string or a `<token>#<secret>` bill as a wallet;
  each is now refused, naming the command to use instead.
  ([#71](https://github.com/ohstr/cashctl/pull/71))
- Fixes `--as cash:` sending a whole token or `<token>#<secret>` string as the
  secret; it now asks for just the secret.
  ([#71](https://github.com/ohstr/cashctl/pull/71))
- Fixes `join` reporting a Hub that is still starting up (`NOT_READY`) as a
  non-retryable `internal` error; it is now `network`, retryable.
  ([#72](https://github.com/ohstr/cashctl/pull/72))
- Adds the `exists` error code (exit 9, not retryable) for `join` when you
  already hold a wallet from that circle, which was reported like "not a member".
  ([#72](https://github.com/ohstr/cashctl/pull/72))
- Fixes `consolidate`, and `transfer`'s auto-consolidate, failing every time
  with more sources than the Hub takes in one call; they now merge in rounds.
  ([#73](https://github.com/ohstr/cashctl/pull/73))
- Adds `unfinished_cash_sends` to `wallet show --json`, so an agent can
  recover an interrupted cash send's secret; only text mode showed it before.
  ([#74](https://github.com/ohstr/cashctl/pull/74))
- Changes `cash status` to report only your own allocation; `--all` adds every
  co-recipient, which it used to fetch every time.
  ([#75](https://github.com/ohstr/cashctl/pull/75))
- Fixes `transfer` with no arguments not mentioning that an amount alone is
  enough, making a cash note.
  ([#75](https://github.com/ohstr/cashctl/pull/75))
- Adds the `spent` error code (exit 10, not retryable) for a bill its Hub
  reports spent; the entry is marked spent and leaves `wallet balance` and
  `redeem --all`, where it used to look live.
  ([#78](https://github.com/ohstr/cashctl/pull/78))
- Fixes checking a cash-mode bill without its secret, or receiving one with a
  wrong or truncated secret, reporting "no matching recipient"; each now says
  what is wrong with the secret.
  ([#78](https://github.com/ohstr/cashctl/pull/78))
- Updates `nmilat` to v0.5.0-rc.22, which tells a spent bill apart from one
  that does not name you.
  ([#78](https://github.com/ohstr/cashctl/pull/78))
- Adds the split floor to `cash status`'s text output, and says in every merge
  prompt that merging tells the Hub the bills share an owner.
  ([#79](https://github.com/ohstr/cashctl/pull/79))
- Fixes `consolidate` sending a source named twice to the Hub; it is now a
  `usage` error, refused before anything is sent.
  ([#79](https://github.com/ohstr/cashctl/pull/79))

- Adds every cash note and gift you hand out to `wallet history`, kept
  permanently with its claimed/unclaimed/expired status, so a lost one can be
  resent; an unclaimed note's secret shows only with `--show-secrets`.
  ([#82](https://github.com/ohstr/cashctl/pull/82))
- Fixes cash notes never merging: `consolidate` and `transfer`'s auto-combine
  now merge them through a key made for that merge, with no identity needed,
  and `receive` only re-keys, never merges.
  ([#83](https://github.com/ohstr/cashctl/pull/83))
- Fixes a spend the SDK refused before sending it (such as a cash note given
  straight to a merge) being reported as `indeterminate`; it is now
  `invalid_input`, since nothing moved.
  ([#85](https://github.com/ohstr/cashctl/pull/85))
- Fixes `join`'s retry after two joins in the same second reading like the
  cause of the failure that followed; it now says why it asks again.
  ([#85](https://github.com/ohstr/cashctl/pull/85))
- Fixes the `wallet protect --help` description of `--token`.
  ([#85](https://github.com/ohstr/cashctl/pull/85))
- Adds `--version`, and the module version or commit to `version` for a
  build without release flags, which printed only `dev`.
  ([#85](https://github.com/ohstr/cashctl/pull/85))
- Adds the likely flag and the `--help` to read to an unknown flag or
  command error, which `--json` mode showed with no next step.
  ([#85](https://github.com/ohstr/cashctl/pull/85))
- Adds `decode npub1…`, showing the hex public key, which was refused; an
  unrecognised `decode` input now lists the forms it takes.
  ([#85](https://github.com/ohstr/cashctl/pull/85))
- Adds a pointer to `connect list` when `connect use`/`wallet use` or
  `connect rm` names no registered connection, and names `show` in the
  `skills` help.
  ([#85](https://github.com/ohstr/cashctl/pull/85))
- Adds the invoice paid into and its `payment_hash` to `redeem --json`, to
  match the payment on the destination side.
  ([#85](https://github.com/ohstr/cashctl/pull/85))
- Fixes `transfer --to cash` and `consolidate --to cash` repeating the new
  note's secret in `target_resolved`; it is in `cash_to_send` (or saved with
  the note) and `target_resolved` is now empty.
  ([#85](https://github.com/ohstr/cashctl/pull/85))
- Adds each held token's Cash Hub to `wallet show` (`cash_hub` in
  `--json`), so you can tell which tokens can merge.
  ([#85](https://github.com/ohstr/cashctl/pull/85))
- Fixes `--json` output escaping `<`, `>` and `&` as `\u003c`, `\u003e` and
  `\u0026`.
  ([#85](https://github.com/ohstr/cashctl/pull/85))
- Adds the cap the Hub granted and the in-circle fee to `join`'s output; the
  cap can be lower than the one asked for.
  ([#86](https://github.com/ohstr/cashctl/pull/86))
- Changes `join` to generate the wallet's connection key on your device, per
  NIP-CW's member-generated key: the secret is saved before the request and
  never sent, and the connection string is built from the Hub's reply. A join
  the Hub never answers stays under `pending_joins` in `wallet show`. Shows
  the wallet's expiry with its other terms.
  ([#86](https://github.com/ohstr/cashctl/pull/86))
- Changes `cash status`, `redeem` and `consolidate` on a token you already
  redeemed or merged away to fail as `spent` (exit 10), as the Hub's tombstone
  says, instead of `not_found`; a token you transferred away stays `not_found`.
  ([#86](https://github.com/ohstr/cashctl/pull/86))
- Fixes a bare `join` naming only the hub connection; it now names the max
  amount too.
  ([#86](https://github.com/ohstr/cashctl/pull/86))
- Fixes the first `cash status` on a bill its Hub just reported spent exiting
  0 with the raw reply; it now fails as `spent` (exit 10), like later calls.
  A lone failure is also no longer printed on stdout as well as stderr.
  ([#86](https://github.com/ohstr/cashctl/pull/86))
- Fixes a refused `consolidate` or `transfer` to cash leaving its new note's
  secret listed under `unfinished_cash_sends` as if the send were interrupted.
  ([#86](https://github.com/ohstr/cashctl/pull/86))
- Changes `cash status` over several bills to exit 10 when one is spent; every
  row is still reported on stdout.
  ([#87](https://github.com/ohstr/cashctl/pull/87))
- Fixes `decode`, `receive` and `connect add` failing on a pasted Lightning
  invoice with a bech32 error; they now say it is an invoice (with its amount)
  and point to `pay` or `redeem --invoice`.
  ([#87](https://github.com/ohstr/cashctl/pull/87))
- Adds `identity` (`created`, `reused` or `existing`) to `init --json`; a re-run
  now also names the identity it kept.
  ([#87](https://github.com/ohstr/cashctl/pull/87))
- Adds to `join`'s `exists` error the name the circle's wallet is registered
  under here, to use with `-c`.
  ([#87](https://github.com/ohstr/cashctl/pull/87))
- Adds the exit codes to `cashctl --help`, the accepted formats to
  `connect add --help`, and to `skills install --help` that only
  `<dir>/cashctl-*` is written.
  ([#87](https://github.com/ohstr/cashctl/pull/87))
- Adds `input` to `usage` errors: the unknown flag or command, or the first
  extra argument.
  ([#87](https://github.com/ohstr/cashctl/pull/87))
- Adds `outstanding_cash_notes` to `wallet balance`: cash notes you handed out
  that are not claimed yet, outside the total.
  ([#87](https://github.com/ohstr/cashctl/pull/87))
- Adds field guides to `transfer --help` and `wallet show --help`.
  ([#87](https://github.com/ohstr/cashctl/pull/87))
- Adds to a refused merge of named tokens which Hub each one is on, and to
  `wallet show` that tokens on the same hub can be merged.
  ([#87](https://github.com/ohstr/cashctl/pull/87))
- Fixes `--json` errors dropping cashctl's own hint after a wallet's message.
  ([#87](https://github.com/ohstr/cashctl/pull/87))
- Changes the RESTRICTED decline text so it no longer reads like a login
  failure, and caps the label part of a joined wallet's name at 24 characters.
  ([#87](https://github.com/ohstr/cashctl/pull/87))
- Adds `skipped_spent` to `redeem --all`: tokens left out because their Hub
  reported them spent. `receive` now says its issuer can take a bill back
  until it is redeemed or transferred.
  ([#87](https://github.com/ohstr/cashctl/pull/87))
- Fixes `wallet balance -c <token>#<secret>` failing as `not_found`; it now
  says to take the bill with `receive`, like `-c` everywhere else.
  ([#87](https://github.com/ohstr/cashctl/pull/87))
- Adds `skipped` to a bare `consolidate`: tokens left as they are because each
  is the only one from its Cash Hub; text mode says so too.
  ([#87](https://github.com/ohstr/cashctl/pull/87))
- Adds `cash_hub` to `decode`, and makes `decode --check` on a note you
  already took say its value moved to the re-keyed bill rather than that the
  bill is gone.
  ([#87](https://github.com/ohstr/cashctl/pull/87))
- Labels a merged cash note `protected` in `wallet show`, and keeps a split
  remainder's label; both showed no protection state.
  ([#87](https://github.com/ohstr/cashctl/pull/87))
- Fixes an unknown flag or command's `--json` error splitting its `--help`
  pointer onto a second line.
  ([#87](https://github.com/ohstr/cashctl/pull/87))
- Adds to `redeem --help` that a token is redeemed whole, which invoice amount
  it needs, and which tokens `--amount` may pick.
  ([#87](https://github.com/ohstr/cashctl/pull/87))
- Adds each wallet's `kind` (`circle`, `nwc` or `cash_token`) to `connect list`
  and `wallet show`, so the circle wallet is easy to find.
  ([#87](https://github.com/ohstr/cashctl/pull/87))
- Adds to a wallet call refused as RESTRICTED or NOT_IMPLEMENTED that
  `wallet get-info` lists what the connection allows.
  ([#87](https://github.com/ohstr/cashctl/pull/87))
- Adds `max_amount`, the granted cap in loki, to `join --json`, and says in
  `invoice --help` that its `--json` amount is in mloki.
  ([#87](https://github.com/ohstr/cashctl/pull/87))
- Updates `nmilat` to v0.5.0-rc.24, whose private transport listens for a
  reply on the relay it sent the request to and waits for that subscription
  before sending; a reply could otherwise be lost and read as no answer.
  ([#88](https://github.com/ohstr/cashctl/pull/88))

## [0.6.0-rc.12]

A dependency update.

- Updates `nmilat` to v0.5.0-rc.17; cashctl's behavior is unchanged.
  ([#68](https://github.com/ohstr/cashctl/pull/68))

## [0.6.0-rc.11]

`cashctl skills install` needs `--dir`, so skills go wherever your agent reads them.

- Makes `--dir` required for `cashctl skills install`, which no longer
  defaults to `~/.claude/skills`; without it the command exits 2 and writes
  nothing. ([#67](https://github.com/ohstr/cashctl/pull/67))

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
