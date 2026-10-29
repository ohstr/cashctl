# cashctl

`cashctl` is a Go CLI wallet for [NIP-CASH](https://github.com/ohstr/nips/blob/main/NIP-CASH.md)
cash tokens and [NIP-CW](https://github.com/ohstr/nips/blob/main/NIP-CW.md)
circle wallets.
Assume the `cashctl` binary is already on `PATH`. State (identity, registered
wallets, held tokens) lives under `$XDG_CONFIG_HOME/cashctl`, overridable with
`--config-dir`. `NCLI_VAULT_PASSWORD` unlocks an ncli vault-sourced identity
non-interactively (no TTY to prompt from — needed for `init`/vault-backed
commands run unattended). `CASHCTL_AS` supplies `--as`'s credential without
putting it in argv — the flag still wins when both are set. Prefer it for
anything scripted: `--as` is always secret-bearing, and a value on the
command line is visible to `ps` while the process runs and is written to
the shell's history file. It is an improvement, not airtight — an
environment variable is inherited by child processes and readable from
`/proc/<pid>/environ` by the same user. For the same reason, `-` in place
of `receive`'s token, `connect add`'s connection or `join`'s hub
connection reads that value from the first line of stdin. `--pow-max <bits>`
(else `CASHCTL_POW_MAX`, else the `pow_max_difficulty` setting of `cashctl
config set`) caps the proof of work cashctl does for a Hub's relays (1–64, default 28); a call
whose relays ask for more is not sent (`internal`, "not placed"). `NO_COLOR`
disables ANSI color on stderr.

## Commands

| Command | Purpose |
|---|---|
| `cashctl init` | Set up your identity (reusing an ncli vault entry if you have one) and optionally a default wallet |
| `cashctl join <hub-connection> <max-amount>` | Join a circle from its `circlehub1...` string (the only form: a `nostr+walletconnect://` URI or a `cashhub1` is refused `invalid_input`, `reason: connection_invalid`) and a requested spend cap in loki — both positional, either order, or `--hub`/`--max-amount`; `--max-receive` is required unless the Hub reports a default. Before anything is sent the Hub must answer as the identity and chain the string names (else `auth`, `reason: connection_mismatch`); a string whose signature doesn't verify is used only after one confirmation per string (the prompt, or `--allow-unverified`; `--yes`/`--json` alone are refused `usage`, `reason: connection_unverified`). Every join goes over NIP-CASH's private transport: no announcement → `network`, `reason: join_unavailable`, nothing sent; a circle not taking members → `auth`, `reason: not_accepting`. The wallet's key is made on your device and the join written down (secret, deadline) before it is sent; a lost reply is looked up, and one still unknown is `indeterminate` with `call.method: create_circle_wallet` — `cash settle` finishes it. A Hub that can't rotate a lost connection gets a warning first; `--backup-file <path>` then writes the wallet's fields (0600). Under `--json`/`--yes` such a join needs `--backup-file` or `--accept-no-rotation`, else `usage`, `reason: no_rotation`, nothing sent. The wallet is stored as its fields, never a URI. `--json`: `wallet`, `default`, `response` (`wallet_pubkey`, `relays`, `connection_pubkey`, `expires_at`, `max_amount_millis`/`max_amount`, `max_receive_millis`, `fees_ppm`, `budget_renewal`, `dust_threshold_millis`, `rotation_delay_secs`), `terms`, `warnings`, `network`, `chain`, `test_network`, `hub_verified` |
| `cashctl circle join <hub-connection> <max-amount>` | Same as `join` — the canonical, fully-namespaced form |
| `cashctl circle request <rotate\|freeze\|unfreeze> [wallet] [--wallet <wallet_pubkey> --hub <circlehub1\|->] [--as pubkey:<privkey>]` | Sign a member request (NIP-CW kind 23199, by the key that joined) for the circle's host to submit; cashctl never sends it, and it is valid for 24 hours. `[wallet]` is a name or `wallet_pubkey` (default: the current wallet); a wallet not held here needs `--wallet` and `--hub`. Refused locally, nothing signed: details without `rotation_delay_secs` → `usage`, `reason: rotation_not_served`; another key → `auth`, `reason: member_mismatch`. `rotate` makes the new connection key here and writes it down before printing (an open one is printed again, `repeated: true`); `cash settle` looks it up by the new key — `pending` until the rotation delay has passed, then the key replaces the old one in the same wallet; never accepted → forgotten, `reason: rotation_not_made`. While it is open, using the wallet asks first (a request by the old key cancels it): `--json` alone → `usage`, `reason: rotation_open`; `--yes` goes ahead. `--json`: `{event, action, wallet_pubkey, hub_pubkey, new_connection_pubkey?, submit_within, next_step, repeated?}` |
| `cashctl wallet delete [name] [--yes]` | Delete your own circle wallet on its Hub (`delete_circle_wallet`, over its own connection), then forget it here. Above its dust threshold → `conflict`, `reason: above_dust`, nothing sent; the dust left goes to the host, which you agree to (the prompt, or `--yes`; `--json` alone → `usage`, `reason: dust_not_accepted`). Written down before it is sent: a lost reply → exit 8, `reason: delete_unknown`, `call.method: delete_circle_wallet`, the wallet kept; run it again. `connect rm` refuses a circle wallet without `--force`. `--json`: `{deleted, wallet_pubkey, dust_left_millis}` |
| `cashctl wallet show` | Your identity, registered wallets, and held cash tokens; also each `join` not finished (`pending_joins` in `--json`: `connection_pubkey`, `hub_pubkey`, `state`, `detail`, `deadline`, `hub_verified` — never the secret; an expired one stays listed), and each transfer, merge or re-key that has no answer yet (`open_calls`: `call`, `method`, `purpose`, `state`, `deadline`, `token_ids`, `amount_millis`, `device`, and each new code it generated under `secrets` — one may be the only code that spends a bill, keep them) |
| `cashctl wallet history [--show-secrets]` | Local action log (receive/redeem/transfer/consolidate), then every cash note and gift you handed out — kept permanently, each `unclaimed`/`claimed`/`expired` (a cash note, re-checked with its Hub on every read) or `sent` (a gift: its sender can't see the claim), so a lost handoff can be resent. An unclaimed note's secret is hidden unless `--show-secrets` |
| `cashctl wallet use <name>` / `cashctl connect use <name>` | Switch your default wallet |
| `cashctl wallet balance [--breakdown\|-v] [--from <name>]` | Unified balance: every wallet's live balance + every held token's value |
| `cashctl wallet protect [id] [--token <id>]` | Re-key a still-shared cash-mode holding so the original code can no longer spend it — `receive` does this automatically; use this if that was declined or failed. With no id and more than one eligible, re-keys every one of them — unlike `redeem`, nothing here leaves your control, so an ambiguous selection has nothing to lose. A holding whose earlier re-key got no answer is settled with its Hub first and never re-keyed while unresolved: named, it is `conflict` with `retryable: false`; otherwise it is listed as `"status": "unresolved"` |
| `cashctl wallet get-info` / `budget` / `invoice <amount>` / `pay <invoice>` / `list-tx` / `sign-message <msg>` | Ordinary NIP-47 calls against the current wallet |
| `cashctl invoice <amount>` / `cashctl pay <invoice>` / `cashctl balance` | Top-level shortcuts for `wallet invoice`/`wallet pay`/`wallet balance` |
| `cashctl connect add <name> <connection>` / `list` / `rm <name>` | Register/list/remove any other NWC connection. `<connection>` may also be a circle wallet's backup file (the `{"type":"circle_wallet",…}` JSON `join --backup-file` writes; a path, or `-` for stdin): stored as kind `circle`, as its fields; `list`/`wallet show` print its name and `wallet_pubkey` only |
| `cashctl decode <string> [--check]` | Inspect any cash token, Cash Hub connection (`cashhub1...`, never its secret: keep it private, it mints bills), Circle Hub connection (`circlehub1...`, shown like a `cashhub1`), NWC URI, or npub (shown as hex) locally, no network call. A bill shows `signature_scheme` (`mint_v1`/`zap_v1`/`malformed`/`none`), `provenance` (`valid`/`invalid`/`needs_zap_event`/`none`) and `relay_hash`; only a `valid` bill is ever dialled. A `cashhub1` shows `hub_identity`, `chain` and `signature` (`verified`/`unverified`, with `signature_reason`). `--check` opts into a read-only Hub check (cash token: matching recipient; cash hub: the online check, `online_check` `pass`/`identity_mismatch`/`chain_mismatch`; circle hub: the online check and whether the circle takes joins now, `accepting`) |
| `cashctl decode <9522-json\|file.json\|-> [--target <file>] [--check]` | A Cash Zap under the client rules 1-8, signature first: `rules:[{n,ok,reason}]`, `countable` (rules 1-8 pass — rule 7 needs `--target`, the zap's target event), `minter_pubkey`, `via`/`sender_credited` for a `P`, `possibly_expired`. Never "received"; `--check` adds rule 9 read-only for your identity |
| `cashctl receive <token>` | Decode a cash token, print its details, then cross-check it against the Cash Hub before adding it to your wallet — refuses anything that doesn't check out. A cash-mode token's `cash_secret` must be embedded, `<token>#<cash_secret>` (NIP-CASH's combined cash-mode slice presentation) — pasted bare, it degrades to a read-only report instead of erroring. A saved cash-mode receipt is then offered automatic protecting: re-keyed under a fresh secret only you hold, reported under `"secured"`. A bill held already, however its string is spelled, is `conflict` before anything is dialled. Once received, the bill's Hub is asked its `network` (`get_info` on the bill's own connection): anything but `mainnet`, or no answer, prints the test-network line; `--json` carries `network` and `test_network` (the same for a Cash Zap). It never merges: merging tells the Hub the bills share an owner, so only `consolidate` (or `transfer`'s auto-combine) does it |
| `cashctl receive <9522-json\|file> \| <token> --zap-event <file\|-> \| <token> --find-zap --relay <url>` | Receive a Cash Zap (NIP-CZ): rules 1-6 and 8 before any network; a minter neither in `trusted_minters` nor listed by you needs your OK (the prompt, or `--trust-minter`; `--json` without it → `usage`, `reason: minter_untrusted`, nothing contacted); then rule 9 — the Hub's own unclaimed row for your pubkey, of exactly the amount — is the only proof it is yours. Kept with its zap event (`received: true`, `zap: true`, `rules`); claimed or shared → `invalid_input`, gone → `spent`, no answer → `network`. A zap bill alone is refused until given its event; `--find-zap` looks on `--relay` and where you read your zaps (your 10522 relays, else NIP-65 read relays), never on the bill's hints. `--relay` also reads your Cash Zap Info (10522): a minter you list there passes rule 8 without asking. A zap to a connection key needs `--as connection-key:…` (its IA is shown first): rule 9 is then a full in-place transfer to your pubkey, written ahead like any call (a lost reply → exit 8, `cash settle` finishes it) |
| `cashctl cash minters list`, `add <minter>… --chain <chain>`, `rm <minter>… --chain <chain>` | The minters you trust for Cash Zaps, one chain each (hex x-only, compressed hex or npub): a zap from one passes rule 8 without `--trust-minter`. `--json`: `{"minters":[{"minter","chain"}]}` |
| `cashctl redeem [wallet] [--token <id>...] [--all] [--amount <n>] [--invoice <bolt11>] [--as <credential>]` | Redeem held token(s) into a wallet (positional, or `--into`) or a raw invoice. `--token` is repeatable/comma-separated and `--all` takes every held token; `--amount` selects which held token(s) land exactly that much (net of any fee) without naming any — an exact single match or an exact same-Hub sum, refusing plainly if nothing adds up; each token is paid into its own invoice, so `--invoice` accepts only one, and with `--invoice` on an amount-less invoice `--amount` is required instead (a fixed-amount invoice rejects `--amount`). Every selected token is attempted: one failing never cancels or hides the others (see **Partial success**). Redeems travel over the private transport only and are batched automatically: every token against the same hub goes out in one relay event, with no flag and no fallback. A connection-key bill whose own row its Hub won't show (`cash_status` `NOT_FOUND`) is refused before anything is sent, `not_found`, `reason: status_unreadable` — transfer it first |
| `cashctl transfer [amount] [target] [--as <credential>]` | Send a held token — amount and target are positional (either order), or `--to`/`--amount`. An amount with no target defaults to a cash note (a `<token>#<secret>` string to hand anyone); neither one is a usage error. With an amount and no `--token`, cash selection picks which held token(s) reach it exactly (auto-consolidating a same-minter subset first if no single token covers it) instead of just picking one token to act on |
| `cashctl consolidate [id...] [--to <target>]` | Merge several held tokens into one — positional IDs, or `--sources`, for exact control (IDs discoverable via `wallet show --json`; plain-text `wallet show` never prints them). With neither, auto-groups held tokens by Cash Hub (only same-Hub tokens can merge) and consolidates each group with 2+ tokens — one group proceeds directly, several prompt interactively (or all process under `--json`/`--yes`). Every chosen group is attempted: one failing never cancels or hides the others (see **Partial success**). Cash notes merge too, with no identity needed: each is first moved onto a key made for that merge alone (cash_consolidate takes no cash-mode source), and your merged result is a fresh cash note. A refused merge moves them back to cash notes; one interrupted midway leaves them on that key, still yours and spendable, shown as `pending_merge` in `wallet show`/`balance` until a later `consolidate` finishes it |
| `cashctl cash settle [--wait] [--close <call>] [--as <credential>]` (also `cashctl settle`) | Look up every transfer, merge or re-key whose answer never came and finish it from the Hub's answer: applied if it happened, placed again under its derived id if the Hub never saw it (transfers and merges, at most three times), closed with its code if refused, shown with its secrets if the Hub no longer remembers it. Never resends under the same id; every money command does this for what is due first. It also turns what a merge left on a throwaway key back into cash notes, and a closed on-chain order is looked up at most hourly. also looks up `join`s whose reply was lost; `--replace <call>` re-places a mint the Hub never saw (never done on its own); `--close <call>` gives up on one call (`--yes` under `--json`; refused for a withdrawal or an answered order). `--json`: `{"calls":[{call, method, purpose, state, detail, deadline, retry_after?, token_ids, amount_millis, device, next_step, summary?, recovery?}], "joins":[{connection_pubkey, hub_pubkey, state, detail?, deadline, wallet?, summary?, hub_verified}]}`. Exit 0 when nothing placed from this wallet file stays open, else 8 |
| `cashctl cash mint <amount> [--hub <cashhub1|->] [--to <target>] [--expiry <dur>] [--allow-unverified]` | Mint a bill over a Cash Hub connection (`--hub`, `-` for stdin, or `$CASHCTL_CASH_HUB`; the URI form is refused `connection_invalid`). The Hub must answer as the string's identity and chain (else `auth`, `connection_mismatch`); an unverified string needs one confirmation (`connection_unverified` otherwise). The call is written ahead; the returned bill must carry the Hub identity's mint signature. `--to cash` (default) → `handoff` cash note; own key → held `entry`; other identity → gift. Lost reply → exit 8, `call.method: mint_cash`; never seen → `stuck`/`confirm_replace`, re-placed only by `cash settle --replace`. `--json`: `cash_token`, `handoff?`, `entry?`, `wallet_pubkey`, `amount_millis`, `expires_at`, `call`, `network`, `chain`, `test_network`, `hub_verified`, `warnings`. Rejects `-c` |
| `cashctl cash order place <cashorder1\|-> <amount> [--onchain] [--to cash\|self\|<target>] [--allow-unverified] [--wait]`, `cash order show [<order>…] [--local]`, `cash order watch <order> [--timeout]`, `cash order settle [<order>] [--replace <order> --yes]` | Buy cash over an Order connection (NIP-CO). `place`: online check (`connection_mismatch`), one confirmation for an unverified string, the terms shown (fee, `pay_millis`, window, the bill's redeem fee); a retired link → exit 7, `reason: not_selling`; out of bounds → exit 3. The order is written ahead (its cash secret or buyer key, `notify`) and placed on the private transport; the reply is `{order, state, payment, amount_millis, fee_millis, pay_millis, invoice?, address?, confirmations?, pay_by, bills, warnings, next_step, terms_changed?}` — pay exactly `pay_millis` before `pay_by`. Never printed: the secret, buyer key, `notify`. `show`/`watch` read the order and keep each bill (cash → a cash note with the order's secret; `self` → your bill; other → a gift). A lost placement → exit 8; never seen → `stuck`/`confirm_replace`, re-placed only by `settle --replace <order> --yes` (a new payment; terms shown again first; only from the wallet copy that placed it; one that fails before sending ends `not_placed`). An open order past `pay_by` with no progress ends `ended_unpaid`. Every subcommand rejects `-c` |
| `cashctl cash discover list [--type order\|circle] [--unreviewed] [--relay] [--chain] [--test-networks]`, `show <offer>`, `open <offer> <amount> [order or join flags]`, `relays list\|add\|rm`, `curators list\|add\|rm` | Offer listings (NIP-CASH §Listing Offers): read from the discovery relays (none shipped; without one → `usage`), parsed and versioned (an older version is never shown again), ranked by the local curators' recommendations; reports shown on every listing of the node. The lister's text is sanitized and never presented as terms. `show` is the tap (get_info on the listed string only): `show`/`hide_for_good`/`hide_until_newer`/`unavailable`, with the Hub's terms. `open` taps again and runs `cash order place` or `join` on the listed string (`connection_mismatch`, `not_selling`/`not_accepting` → exit 7; unavailable → exit 6). Rejects `-c` |
| `cashctl cash discover recommend <offer>… [--drop] [--text\|-]`, `recommend --withdraw <offer>`, `report <offer> --reason …`, `publish-due [--list] [--cancel <rq-…>]`, `operator add\|remove <offer>…`, `curators publish\|pull`, `list --offline` | The user's own public signals, each after a notice of what it makes public; under `--json` only with `--yes`. Without an identity: `usage`, `reason: identity_required` (no key is created). `recommend`: only an offer used a day before and not in this run (else exit 3 `recommend_not_allowed`); one per node, extending the newest. Recommendations and reports are queued unsigned with a 1–6 h delay (`{"outbox","kind","publish_after","relays","audience"}`) and published by `publish-due`, which every `cash discover` command runs first. `--withdraw`, `operator` (the user's 11191: `{"event_id","offers","added","removed","first_publish","published_to","relays_failed","not_verified"}`) and `curators publish` (names private) go out at once; no relay answering → exit 6. `list` shows an operator only when verified (`operator: {pubkey, name?, verified, followed_by}`, else null) and ranks by follows (kind 3) and curators; when several pubkeys list one offer id (`d`), it shows only the one whose signer passed at `show`, or none, and counts the rest in `contested` (each can still be `show`n by its address); an operator's offers list is also read from its NIP-65 write relays; a recommendation's tags carry the relay its listing was read from; fetched events are cached and `--offline` reads only the cache |
| `cashctl cash withdraw quote <address\|-> [--token] [--send-before]`, `send --quote <wq-…> --yes` (or interactively `send <address>` and type `yes`), `show [<w>…] [--local]`, `watch <w> [--timeout]`, `show [<w>] [--tx] [--receipt]`, `cancel <w> [--yes]`, `settle [<w>] [--replace <w> --yes]` | Pay a whole bill out on-chain (irreversible). `quote` moves nothing: `{quote, token, address, amount_millis, network_fee_millis, hub_fee_millis, fee_millis, payout_millis, sent_by, holds_until, warnings, next_step}` — show it to the user. Non-interactive `send` needs `--quote <quote> --yes` (else `usage`); `--json` never counts as consent. Local refusals: `address_invalid`, `own_order_address` (exit 3), `protect_first` (exit 2), `expiring`, `mid_merge` (exit 5: a bill still on a merge's throwaway key; `cash settle` turns it back into a cash note), `status_unreadable` (a connection-key bill whose row its Hub won't show); Hub without withdrawals → exit 1 `withdraw_not_offered`; a mismatching quote → exit 1 `quote_mismatch`. The call is written ahead (quote, address, notify, re-mint secret for a cash note); a queued payout spends the bill (`withdrawn`); a lost reply is exit 8, the bill locked, never resend. `show` follows the payout (`queued`, `late`, `broadcast`, `confirmed`, `final`, `cancelled` → the amount back as a new bill, `remint_entry`, or `remint_gone: true` with `remint_gone_reason` when the Hub reports that bill already gone); `cancel` only for a payout `broadcast` 7 days past `sent_by` (else `cancel_not_due`). `watch` follows state notices and polls until final or cancelled. `--tx` prints the transaction only when `tx_check` is `ok`. A `quote_expired` refusal is exit 7 with `detail`; any other coded refusal is looked up once after the call's deadline and ends with its own code instead of exit 8; interactively a new quote is shown for a new `yes`. A final withdrawal keeps no address, quote, receipt or secret (the address is printed once). A Hub-reported `withdraw_request_id` with no local record becomes a `recovered` withdrawal. `decode` reads quotes, receipts, payouts and addresses. Rejects `-c` |
| `cashctl cash status [--token <id>] [--all]` | Your allocation of a held token (network); `--all` also lists its co-recipients. With no id and more than one held, reports every one — read-only, so the same "nothing to lose" rule as `wallet protect` applies |
| `cashctl cash receive` / `redeem` / `transfer` / `consolidate` | Same as the top-level forms above — the canonical, fully-namespaced versions |
| `cashctl skills list` / `show <name>` / `install --dir <dir>` | The agent skills below, embedded in the binary so they match its version: list names and descriptions, print one SKILL.md to stdout, or copy each skill's files (SKILL.md and any it references) to `<dir>/<name>/`, replacing older copies — `--dir` is required, since skills go wherever your agent reads them (`usage` without it). Unknown name is `not_found` |
| `cashctl version` | Print the cashctl version |

A `--to`/positional target (`transfer`, `consolidate`) needs no prefix for
the common case — a bare 64-hex pubkey, `npub1...`, a NIP-05 identifier
(`name@domain`, resolved live via the domain's `/.well-known/nostr.json`),
or an `nconnection1...` are all sniffed by shape. `cash` is a
literal keyword. The explicit, scripted/advanced forms keep working for
what unprefixed sniffing can't cover: `pubkey:<hex>`,
`connection:<platform>:<external-id>:<ia-pubkey>`.

An `nconnection1...` never carries an Identity Authority itself (a local,
sender-side trust decision, not part of the shareable connection string)
— resolving one asks for one: `--ia
<identity>` (hex pubkey or NIP-05) supplies it non-interactively; without
it, an interactive session is prompted, and a `--json`/`--yes` call gets a
usage error naming `--ia` instead. Whenever something beyond a bare
hex/npub gets resolved (a NIP-05 lookup, or an `nconnection1...`'s IA),
the result is shown back before it's used — printed as a `resolves to:`
line in text mode, and always present as `target_resolved` in `--json`
output (empty string when nothing needed resolving).

`--as`'s credential syntax is unchanged and always needs its prefix (never
auto-detected — a bare hex string is genuinely ambiguous between a private
key and a cash secret, so guessing isn't safe here the way it is for a
public target): `pubkey:<hex-or-privkey>`, `cash:<secret>`,
`connection-key:<privkey>,<platform>,<external-id>,<attestation-file>`
(redeem/transfer only).

## Output conventions

Every command's result goes to **stdout only**, always as a single JSON
document under `--json`; progress narration and errors go to **stderr**
always, never stdout — a script parsing stdout never has to distinguish a
success shape from a failure shape on the same stream. `--json`, `-c/
--connection`, `--yes`, and `--config-dir` are global flags declared once
on the root command — except `-c/--connection`, which `receive`,
`transfer`, `consolidate`, `cash status`, `decode`, and `wallet protect`
reject outright (`usage`, exit 2): none of them ever dial a registered
wallet, so there's nothing for it to override. `--yes` (or `--json`, which
implies it) skips confirmation prompts. Every command is
JSON-only-on-request (human text by default, `--json` for the machine
shape) — there is no command that is JSON-only always.

**Failures**: exactly one top-level error report, always on stderr — a
plain `Error: ...` line by default, or `{"error", "code", "retryable",
"input"?, "nwc_code"?, "recovery"?}` with `--json`:

| `code` | exit | retryable | meaning |
|---|---|---|---|
| `usage` | 2 | no | bad/missing/conflicting flags or args, or a group command invoked without a subcommand |
| `invalid_input` | 3 | no | a supplied value failed validation/parsing (bad token, connection string, credential syntax, amount, ...) |
| `not_found` | 4 | no | the referenced thing doesn't exist (held token, registered wallet, vault entry, ...) — includes "no wallet configured yet", and a token you transferred away (no longer yours) |
| `conflict` | 5 | yes | collides with existing state (a bill already held, however its string is spelled; a wallet's rate limit) |
| `network` | 6 | yes | couldn't reach a relay/wallet, or a Hub isn't ready yet (`NOT_READY`) |
| `auth` | 7 | no | not authorized, or no longer (a wallet declined as restricted/unauthorized/expired) |
| `indeterminate` | 8 | no | a `redeem`/`transfer`/`consolidate` got no single answer (timeout, omission, incomplete reply, a reply that contradicts itself, or a result cashctl could not read), or a transfer, merge or protect was answered with a code other than `BAD_REQUEST`, so whether it was applied is unknown — never resend it blind. `transfer`, `consolidate` and `wallet protect` first wait for the call's deadline (at most 62 s; `--no-wait` skips this) and look it up once: a call the Hub applied is finished as if its reply had come, and one it refused with a code exits with that code's own class. A transfer, merge or protect stays an open call (named in the error and in `--json`'s `call`: `call`, `method`, `state`, `detail`, `deadline`, `settle_after`, `next_step`; listed in `wallet show`), its bills are not spent again until it is settled, and `cash settle` (or the next money command) asks the Hub what happened; a redeem is never resent: cashctl reads the bill's own row once, and one whose payment may still settle becomes `redeeming` (reason `redeem_in_flight`): out of every balance and every other command until `cash status` or `cash settle` sees it end |
| `exists` | 9 | no | what you're trying to create already exists (`join`: you already hold a wallet from that circle) — don't retry, use the one you have. If an earlier `join` is still open (under `pending_joins`), run `cash settle`: it looks the join up and registers the wallet it made |
| `spent` | 10 | no | the bill's Hub confirmed it is spent (a tombstone): its value has already moved. The local entry is marked spent and leaves `wallet balance`/`redeem --all`; never inferred from silence. A tombstone whose reason is `expired` or `reclaimed`, or `NOT_FOUND` for your own key or cash secret (your slice was removed), marks it `expired` or `reclaimed` instead, with that state as `reason`. Also what naming a token you redeemed or merged away yourself returns (`cash status --token`, `redeem`, `consolidate`), with no network call |
| `internal` | 1 | no | anything else — a wallet-side decline that isn't one of the above, or a cashctl-side failure |

`reason`, when present, names the cause within `code` (`connection_invalid`,
`mixed_case`, `connection_mismatch`, `connection_unverified`, `not_accepting`,
`join_unavailable`, `over_hub_limit`, `rotation_not_served`, `rotation_open`,
`above_dust`, ...).

`input`, when present, is the single specific value that caused the
failure (for a `usage` error: the unknown flag or command, or the first
extra argument) — **never** raw secret material: an `nsec1...`-shaped or bare
64-hex-char value is redacted to `""`, a `pubkey:<privkey>`/`cash:
<secret>`/`connection-key:<privkey>,...` credential string has just its
secret component blanked (`pubkey:<redacted>`, etc.), keeping the rest of
the string legible in the error, a `circlehub1...`/`cashhub1...`/
`nconnection1...` string becomes `circlehub1<redacted>` (its secret is
packed in, nothing of it is safe to show), and an NWC URI keeps all but its
`secret=` value. `retryable` lets an agent decide whether
to back off and retry (`conflict`/`network`) or fix the input and try
again (everything else) without string-matching the message. `nwc_code`,
when present, is the raw NIP-47 error code (`RESTRICTED`, `EXPIRED`,
`INSUFFICIENT_BALANCE`, ...) a wallet returned — cashctl's own code table
is deliberately coarse, so this is there for an agent that needs
finer-grained branching. `recovery`, when present, is text you must keep
verbatim: it appears only when a Hub-side mutation is confirmed to have
happened and recording it locally then failed, and it carries whatever
makes that money reachable again — for a cash-mode bill the
`<token>#<cash_secret>` handoff, for `join` the new wallet's connection
string (register it with `connect add`), neither of which anything else
will show again. It is
human-readable text that can name more than one (a `transfer` reports the
recipient's bill and your own remainder), so match the handoffs inside it
rather than expecting one bare value. It is the one field deliberately
exempt from secret redaction, and it is never truncated; in text mode it is
printed on its own unprefixed line so it can be copied as-is. `wallet history --show-secrets` is secret-bearing by design for the same
reason as `recovery`: it is the one place a handed-out but unclaimed cash
note can be read again, so treat its output like the note itself. Without
the flag, an unclaimed note's secret is replaced by `<hidden: --show-secrets>`
and `secret_hidden` is `true`. A usage
mistake prints the command's help after the error, on stderr like the error
itself; in `--json` mode that dump is skipped in favor of the structured error
alone.

**Partial success**: a command that performs several independently
committed operations — `consolidate` when it auto-groups held tokens by
Cash Hub, and `redeem` with several tokens selected — reports every
operation's outcome on stdout AND exits
nonzero if any of them failed, with the first failure's own `code` driving
the exit status. A nonzero exit from such a command therefore does not mean
nothing happened: stdout is still a complete result document and must be
read before deciding what to retry. The alternative would be worse than
untidy — aborting on the first failure leaves the already-committed
operations unreported, including any new token they produced. A
`cash status` over several bills follows the same rule: every row is
printed, and a row that is `spent` (or failed) drives the exit status.

**Ambiguous selection, with nothing named and more than one candidate**:
refuse (`usage`, exit 2) only when the operation pays value out of your
control — `redeem` with no `--token`/`--all`/`--amount` and several held
tokens. Every other case defaults to processing/showing every eligible
candidate instead: `consolidate` auto-groups and merges all of them,
`wallet protect` re-keys all of them, `cash status` reports all of them —
because in each of those the value stays fully yours (or, for `status`,
nothing moves at all), so there is nothing an ambiguous pick could lose. An
explicit `--token`/selection still narrows to one in every case.

## Before attempting a task, read the matching skill

This repo ships example-driven guidance in `skills/`, one file per area:

- Setting up an identity, managing wallets, making ordinary Lightning
  calls, or decoding any token/connection string locally (`init`,
  `wallet ...`, `connect ...`, `decode`) → `skills/cashctl-wallet/SKILL.md`
- Receiving, redeeming, transferring, or consolidating NIP-CASH tokens, or
  re-keying a still-shared cash-mode holding (`receive`, `redeem`, `transfer`,
  `consolidate`, `cash ...`, `wallet protect`) → `skills/cashctl-cash/SKILL.md`
- Joining a circle for a personal wallet (`join`, `circle join`) →
  `skills/cashctl-circle/SKILL.md`
