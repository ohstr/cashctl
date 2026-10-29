# Changelog

## [0.7.0-rc.2]

- Fixes one bill being received and counted twice under two spellings (entries reordered, an unknown TLV
  added): holdings are keyed by wallet pubkey and stored in one spelling. Schema 4 migrates a 0.7.0-rc.1
  database and marks a second copy `duplicate`, a status scripts must accept.
- Says when a received bill's Hub is not on `mainnet`, as for minting and orders: `receive` reads the bill's
  `get_info` network and prints the test-network line; `--json` adds `network` and `test_network`.
- Refuses an order's Lightning invoice that the Hub's node identity did not sign, or whose prefix is not the
  Hub's network (a `tb` or `bcrt` invoice from a `mainnet` bitcoin Hub), before it is shown to pay.
- Refuses an `--as connection-key` attestation dated more than 15 minutes ahead of the clock, which a
  verifier ignores (NIP-IC), instead of sending a claim that cannot pass.
- Gives a recommendation's `p` and `a` tags a relay hint: the relay its listing was read from. The discovery
  cache now records the relay that first served each event.
- Reads an operator's offers list (11191) from the operator's NIP-65 write relays too, so an operator whose
  list is only on their own relays shows as verified.
- Hides listings of one offer id (`d`) signed by several pubkeys until one passes at `show`, then shows that
  one only; `list --json` counts the rest in `contested`. A copy re-signed under another key no longer sits
  beside the real offer.
- Asks a second time before a withdrawal whose fee is above 25 % of the bill, and shows the block the price
  holds until, what a Lightning redeem would land, and a privacy warning for a received or zap bill.
- Stops looking up a withdrawal at every command when its Hub keeps answering without a valid receipt: two
  such answers leave it `stuck` (`receipt_invalid`), shown as such and looked up hourly.

## [0.7.0-rc.1]

- Adds the `reclaimed` and `expired` holding statuses, so a removed slice
  (`NOT_FOUND` for your own key or cash secret, or a `reclaimed` tombstone) and
  an expired bill leave the balance instead of staying held or showing `spent`.
  Scripts reading `status` must accept both; their errors are `spent` with the
  state as `reason`. ([#147](https://github.com/ohstr/cashctl/pull/147))
- Fixes a withdrawal whose reply was lost waiting for the user
  (`quote_unchecked`) instead of being re-placed automatically while its quote
  holds, as NIP-CP §Lookups allows a wallet with no chain view. ([#147](https://github.com/ohstr/cashctl/pull/147))
- Refuses to quote or withdraw a bill still on a merge's throwaway key
  (`mid_merge`); `cash settle` now turns merge leftovers back into cash notes,
  as every money command does. See `docs/client-rules.md`. ([#147](https://github.com/ohstr/cashctl/pull/147))
- Says when a cancelled payout's re-minted bill is already gone
  (`remint_gone`, with the Hub's reason), instead of closing it silently. ([#147](https://github.com/ohstr/cashctl/pull/147))
- Refuses an `--invoice` redeem of a connection-key bill whose own row its Hub
  won't show (`status_unreadable`), so it can no longer stay `redeeming` for
  ever. ([#147](https://github.com/ohstr/cashctl/pull/147))
- Settles every coded withdrawal refusal in the same run, not only a refused
  quote, and follows another copy's withdrawal named in answer to a
  re-placement. ([#147](https://github.com/ohstr/cashctl/pull/147))
- Fixes a consolidate tombstone marking the wrong bill spent when the first
  candidate could not be dialled or the first source was not held here. ([#147](https://github.com/ohstr/cashctl/pull/147))
- Ends an order `not_placed` when its call closes without placing, writes a
  forgotten closed order and a recovered withdrawal in one save, and looks a
  closed on-chain order up at most hourly. ([#147](https://github.com/ohstr/cashctl/pull/147))
- Adds `circle request rotate|freeze|unfreeze`: a member request signed by the
  key that joined, for the circle's host to submit. A rotate's new key is
  written down before the request is printed, `cash settle` installs it once
  the rotation completes, and using the wallet while it is open asks first. ([#147](https://github.com/ohstr/cashctl/pull/147))
- Adds `wallet delete` for your own circle wallet at or below its dust
  threshold, written down before it is sent; `connect rm` now refuses a
  circle wallet without `--force`. `FROZEN` is `auth` and `ROTATION_PENDING`
  a final `conflict`. ([#147](https://github.com/ohstr/cashctl/pull/147))
- Adds a wait to `transfer`, `consolidate` and `wallet protect` when the Hub
  gives no clear answer: they look the call up after its deadline (at most 62
  s) and finish it, or exit with the refusal's own code; `--no-wait` exits 8 at
  once. ([#147](https://github.com/ohstr/cashctl/pull/147))
- Adds a `redeeming` state for a redeem whose payment may still settle (answered
  `INTERNAL`, omitted or timed out with the bill still claimed): it is never
  resent, counts in no balance, is refused by every other command
  (`redeem_in_flight`), and `cash status` and `cash settle` read it again until
  it ends. ([#147](https://github.com/ohstr/cashctl/pull/147))
- Adds a second try for a join whose lookups all go unanswered: `cash settle`
  asks once over another `circlehub1` held for the same circle, then shows the
  join as `lookup_unanswered`.
  ([#141](https://github.com/ohstr/cashctl/pull/141))
- Adds circle wallet backups to `connect add`: the file `join --backup-file`
  writes registers the wallet again, shown by its `wallet_pubkey` only.
  ([#141](https://github.com/ohstr/cashctl/pull/141))
- Adds claiming a Cash Zap to a connection key (`receive --as
  connection-key:…`), reading your Cash Zap Info for trusted minters, a wider
  `--find-zap`, and `cash minters` to manage the minters you trust.
  ([#142](https://github.com/ohstr/cashctl/pull/142))
- Adds `--pow-max` and `cashctl config set pow_max_difficulty`, beside
  `CASHCTL_POW_MAX`, to cap proof of work. `cash order watch` now reads an
  order when its notices arrive, and an order's invoice is checked locally
  (amount, expiry, chain) before it is shown to pay.
  ([#145](https://github.com/ohstr/cashctl/pull/145))
- Adds `cash withdraw watch`, `show --tx` and `--receipt`, re-quoting when a
  quote expires or the prompt stays open, recovery of a withdrawal this device
  has no record of, and `decode` of withdraw quotes, receipts, payouts and
  on-chain addresses; a final withdrawal keeps no address, quote or secret.
  ([#146](https://github.com/ohstr/cashctl/pull/146))
- Adds `cash discover recommend`, `report`, `publish-due`, `operator` and
  `curators publish|pull`, and verifies a listing's operator; recommendations
  and reports are published after a random delay. `cash discover list
  --offline` reads only the events it cached.
  ([#144](https://github.com/ohstr/cashctl/pull/144))
- Adds `cash discover list|show|open|relays|curators`, which finds order links
  and circles listed on Nostr relays, checks one with its Hub when you show it,
  and orders from it or joins it.
  ([#138](https://github.com/ohstr/cashctl/pull/138))
- Adds `cash withdraw quote|send|show|cancel|settle`, which pays a bill out
  on-chain after the user confirms the exact quote, then follows the payout; a
  payout the Hub cancels comes back as a new bill.
  ([#137](https://github.com/ohstr/cashctl/pull/137))
- Adds `cash order place|show|watch|settle`, which buys cash over a
  `cashorder1` Order connection and collects the bills once paid; `decode`
  reads a `cashorder1` string.
  ([#136](https://github.com/ohstr/cashctl/pull/136))
- Adds Cash Zaps: `decode` shows a kind-9522 zap's checks, and `receive` takes
  one (or a zap bill with `--zap-event` or `--find-zap`) once its Hub confirms
  your slice, asking first about a minter you don't trust.
  ([#135](https://github.com/ohstr/cashctl/pull/135))
- Adds proof of work for Hubs whose relays ask for it, capped by
  `CASHCTL_POW_MAX` (default 28); above the cap nothing is sent.
  ([#134](https://github.com/ohstr/cashctl/pull/134))
- Adds `cash mint`, which mints a bill over a `cashhub1` connection after the
  online check; a mint whose reply is lost is finished by `cash settle`, and one
  the Hub never saw is minted again only with `cash settle --replace`.
  ([#133](https://github.com/ohstr/cashctl/pull/133))
- Changes `join` to take only a `circlehub1` string, check that the Hub answers
  as the identity and chain it names, and go over the private transport every
  time; `--private` is removed. A string whose signature doesn't verify is used
  only after you confirm it once (or `--allow-unverified`).
  ([#132](https://github.com/ohstr/cashctl/pull/132))
- Adds `--max-receive` and `--backup-file` to `join`, and writes every join
  down before it is sent; a join whose reply is lost is looked up, and `cash
  settle` finishes it. A circle wallet is stored as its fields, never a URI.
  ([#132](https://github.com/ohstr/cashctl/pull/132))
- Adds a `reason` to `--json` errors, naming the cause within `code`.
  ([#132](https://github.com/ohstr/cashctl/pull/132))
- Adds `cash settle` (also `settle`), which asks the Hubs about calls whose
  answer never came and finishes them; every money command does this first. A
  call the Hub never saw is placed again under a derived id, never twice.
  ([#131](https://github.com/ohstr/cashctl/pull/131))
- Changes transfers, merges and protects to be written down with their request
  id, deadline and every new secret before they are sent; one that gets no
  clear answer stays open, and its bills are not spent again until the Hub says
  what happened. `wallet show` lists open calls and their new codes.
  ([#130](https://github.com/ohstr/cashctl/pull/130))
- Changes a partial split of a cash note to give the remainder a fresh secret of
  its own.
  ([#130](https://github.com/ohstr/cashctl/pull/130))
- Changes the local database to a new format. A `cashctl.db` from an earlier
  version is refused, naming the file to move aside; nothing in it is read.
  ([#129](https://github.com/ohstr/cashctl/pull/129))
- Changes `connect add` to refuse a cash bill: a bill is never a wallet.
  ([#129](https://github.com/ohstr/cashctl/pull/129))
- Changes the cached wallet balance in `--json` to `last_known_balance_millis`.
  ([#129](https://github.com/ohstr/cashctl/pull/129))
- Changes every command that contacts a bill to check its mint signature first:
  a bill whose signature does not cover its own relays and secret, or that has
  none, is refused before any relay is contacted.
  ([#128](https://github.com/ohstr/cashctl/pull/128))
- Adds `signature_scheme`, `provenance` and `relay_hash` to `decode`, and decodes
  a `cashhub1` connection with its signature verdict and `--check` online check.
  ([#128](https://github.com/ohstr/cashctl/pull/128))
- Fixes a batched redeem whose Hub reply contradicts itself, or whose result
  can't be read, being reported as an ordinary failure; it is now
  `indeterminate`, never resent, and the other bills keep their outcomes.
  ([#127](https://github.com/ohstr/cashctl/pull/127))
- Fixes protecting a holding twice when the first answer was lost, which could
  overwrite the only secret that spends it. `wallet protect` now settles the
  first protect with the Hub, refuses while it can't tell, and `wallet show`
  lists the new code meanwhile.
  ([#124](https://github.com/ohstr/cashctl/pull/124))
- Fixes a bill lost when a protect or merge moved it on the Hub but saving that
  locally failed; cashctl now prints its `<token>#<secret>` to keep.
  ([#124](https://github.com/ohstr/cashctl/pull/124))
- Updates `nmilat` to v0.5.0-rc.44 and builds with Go 1.26.10. ([#147](https://github.com/ohstr/cashctl/pull/147))

## [0.6.0-rc.17]

Refuses a Hub that raises a bill's redeem fee after you received it, and keeps
secrets off the command line with `-`. Fixes receiving a bill split between
several people and spending with `--as pubkey:`.

- Adds `-` in place of `receive`'s token, `connect add`'s connection and
  `join`'s hub connection, read from stdin so the secret stays out of `ps` and
  shell history.
  ([#116](https://github.com/ohstr/cashctl/pull/116))
- Fixes `skills install` writing through a symlink planted where a skill file
  goes; it now replaces the link, and refuses a skill directory that is one.
  ([#116](https://github.com/ohstr/cashctl/pull/116))
- Fixes `receive` refusing a signed bill split between several recipients: it
  compared the holder's own share against the whole bill's attested amount.
  ([#116](https://github.com/ohstr/cashctl/pull/116))
- Fixes `redeem --as pubkey:<key>` (and `CASHCTL_AS`), and redeeming or sending
  a cash note left `pending_merge`, failing with "couldn't determine this
  token's amount": the Hub's reply was matched against your stored identity.
  ([#116](https://github.com/ohstr/cashctl/pull/116))
- Fixes an existing config directory open to group or others (e.g. 0755) being
  left that way; cashctl now makes it 0700 on first use and says so.
  ([#118](https://github.com/ohstr/cashctl/pull/118))
- Fixes a Hub being able to charge a larger redeem fee, or quote a larger
  amount, for a bill than it did when you received it; `redeem` now refuses.
  ([#120](https://github.com/ohstr/cashctl/pull/120))
- Adds a note when `receive` is handed a bill whose relay is on your own
  machine or local network, which a bill from someone else has no reason to name.
  ([#120](https://github.com/ohstr/cashctl/pull/120))

## [0.6.0-rc.16]

Joins circles privately when the Hub offers it, so other members can't see who
joined.

- Adds private joins: `circle join` joins privately when the Hub offers it and
  says so when it can't; `--private=require` refuses instead.
  ([#112](https://github.com/ohstr/cashctl/pull/112))
- Fixes a malformed NIP-04 reply from a wallet crashing cashctl; it is now
  reported as an error. Picks up nmilat v0.5.0-rc.31.
  ([#111](https://github.com/ohstr/cashctl/pull/111))

## [0.6.0-rc.15]

Fixes several ways an interrupted or forged reply could lose or misdirect money,
plus error-text and agent-facing UX fixes from the agent evals.

- Adds the `--json` error object's fields to `cashctl --help`; the cash skill
  now points there instead of AGENTS.md, which an installed binary doesn't have.
  ([#94](https://github.com/ohstr/cashctl/pull/94))
- Adds `wallet get-balance` as an alias of `wallet balance`, and an
  `amount_mloki` field to `invoice --json`.
  ([#109](https://github.com/ohstr/cashctl/pull/109))
- Fixes a wallet reply signed by another key being taken as the wallet's own,
  which could fake a payment or point a redeem at someone else's invoice; picks
  up nmilat v0.5.0-rc.27. ([#110](https://github.com/ohstr/cashctl/pull/110))
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
- Fixes a `transfer` that auto-merges your bills losing the merged bill if it
  was interrupted between the merge and the send; the merged bill is now saved
  first, so it stays in your wallet whatever happens to the send.
  ([#105](https://github.com/ohstr/cashctl/pull/105))
- Fixes error text: a Hub's message and cashctl's hint no longer run together,
  `wallet balance -c <unknown>` says where to look, `decode` names npub in its
  list, and `wallet history` says "(pubkey)" instead of "identity cash".
  ([#107](https://github.com/ohstr/cashctl/pull/107))
- Fixes `init --json` reporting an empty `default_wallet` when one is already
  set, and generated circle wallet names cut mid-word.
  ([#109](https://github.com/ohstr/cashctl/pull/109))
- Fixes `decode --check` on a cash note someone else re-keyed: it now says the
  secret no longer answers instead of "no matching recipient".
  ([#109](https://github.com/ohstr/cashctl/pull/109))

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
