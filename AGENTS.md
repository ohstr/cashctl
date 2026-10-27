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
connection reads that value from the first line of stdin. `NO_COLOR` disables ANSI color on
stderr.

## Commands

| Command | Purpose |
|---|---|
| `cashctl init` | Set up your identity (reusing an ncli vault entry if you have one) and optionally a default wallet |
| `cashctl join <hub-connection> <max-amount>` | Join a circle via its Circle Hub connection (`circlehub1...` or a raw NWC URI) and a requested spend cap in loki — both positional, either order, or `--hub`/`--max-amount`. A cap is required; NIP-CW has no "0 means unlimited" convention. The wallet's connection key is made on your device and its secret saved before the request; only `connection_pubkey` is sent. When the Hub offers private join (`get_info` names a node `identity`), the request goes over NIP-CASH's private transport; a fallback to the shared connection is never silent (stderr line in text mode, `fallback_reason` in `--json`, alongside `private`), and `--private=require` refuses instead, sending nothing. `--json`'s `response` carries `wallet_pubkey`, `relays`, `connection_pubkey`, `expires_at`, `max_amount_millis` (the cap granted, maybe lower than asked; `max_amount` is the same in loki), `fees_ppm`, `budget_renewal` — never the connection string |
| `cashctl circle join <hub-connection> <max-amount>` | Same as `join` — the canonical, fully-namespaced form |
| `cashctl wallet show` | Your identity, registered wallets, and held cash tokens; also each `join` the Hub never answered (`pending_joins` in `--json`: `connection_pubkey`, `hub_pubkey`, `at` — never the secret), and each transfer, merge or re-key that has no answer yet (`open_calls`: `call`, `method`, `purpose`, `state`, `deadline`, `token_ids`, `amount_millis`, `device`, and each new code it generated under `secrets` — one may be the only code that spends a bill, keep them) |
| `cashctl wallet history [--show-secrets]` | Local action log (receive/redeem/transfer/consolidate), then every cash note and gift you handed out — kept permanently, each `unclaimed`/`claimed`/`expired` (a cash note, re-checked with its Hub on every read) or `sent` (a gift: its sender can't see the claim), so a lost handoff can be resent. An unclaimed note's secret is hidden unless `--show-secrets` |
| `cashctl wallet use <name>` / `cashctl connect use <name>` | Switch your default wallet |
| `cashctl wallet balance [--breakdown\|-v] [--from <name>]` | Unified balance: every wallet's live balance + every held token's value |
| `cashctl wallet protect [id] [--token <id>]` | Re-key a still-shared cash-mode holding so the original code can no longer spend it — `receive` does this automatically; use this if that was declined or failed. With no id and more than one eligible, re-keys every one of them — unlike `redeem`, nothing here leaves your control, so an ambiguous selection has nothing to lose. A holding whose earlier re-key got no answer is settled with its Hub first and never re-keyed while unresolved: named, it is `conflict` with `retryable: false`; otherwise it is listed as `"status": "unresolved"` |
| `cashctl wallet get-info` / `budget` / `invoice <amount>` / `pay <invoice>` / `list-tx` / `sign-message <msg>` | Ordinary NIP-47 calls against the current wallet |
| `cashctl invoice <amount>` / `cashctl pay <invoice>` / `cashctl balance` | Top-level shortcuts for `wallet invoice`/`wallet pay`/`wallet balance` |
| `cashctl connect add <name> <connection>` / `list` / `rm <name>` | Register/list/remove any other NWC connection |
| `cashctl decode <string> [--check]` | Inspect any cash token, Cash Hub connection (`cashhub1...`, never its secret: keep it private, it mints bills), Circle Hub connection (`circlehub1...`), NWC URI, or npub (shown as hex) locally, no network call. A bill shows `signature_scheme` (`mint_v1`/`zap_v1`/`malformed`/`none`), `provenance` (`valid`/`invalid`/`needs_zap_event`/`none`) and `relay_hash`; only a `valid` bill is ever dialled. A `cashhub1` shows `hub_identity`, `chain` and `signature` (`verified`/`unverified`, with `signature_reason`). `--check` opts into a read-only Hub check (cash token: matching recipient; cash hub: the online check, `online_check` `pass`/`identity_mismatch`/`chain_mismatch`; circle hub: can we join) |
| `cashctl receive <token>` | Decode a cash token, print its details, then cross-check it against the Cash Hub before adding it to your wallet — refuses anything that doesn't check out. A cash-mode token's `cash_secret` must be embedded, `<token>#<cash_secret>` (NIP-CASH's combined cash-mode slice presentation) — pasted bare, it degrades to a read-only report instead of erroring. A saved cash-mode receipt is then offered automatic protecting: re-keyed under a fresh secret only you hold, reported under `"secured"`. It never merges: merging tells the Hub the bills share an owner, so only `consolidate` (or `transfer`'s auto-combine) does it |
| `cashctl redeem [wallet] [--token <id>...] [--all] [--amount <n>] [--invoice <bolt11>] [--as <credential>]` | Redeem held token(s) into a wallet (positional, or `--into`) or a raw invoice. `--token` is repeatable/comma-separated and `--all` takes every held token; `--amount` selects which held token(s) land exactly that much (net of any fee) without naming any — an exact single match or an exact same-Hub sum, refusing plainly if nothing adds up; each token is paid into its own invoice, so `--invoice` accepts only one, and with `--invoice` on an amount-less invoice `--amount` is required instead (a fixed-amount invoice rejects `--amount`). Every selected token is attempted: one failing never cancels or hides the others (see **Partial success**). Redeems travel over the private transport only and are batched automatically: every token against the same hub goes out in one relay event, with no flag and no fallback |
| `cashctl transfer [amount] [target] [--as <credential>]` | Send a held token — amount and target are positional (either order), or `--to`/`--amount`. An amount with no target defaults to a cash note (a `<token>#<secret>` string to hand anyone); neither one is a usage error. With an amount and no `--token`, cash selection picks which held token(s) reach it exactly (auto-consolidating a same-minter subset first if no single token covers it) instead of just picking one token to act on |
| `cashctl consolidate [id...] [--to <target>]` | Merge several held tokens into one — positional IDs, or `--sources`, for exact control (IDs discoverable via `wallet show --json`; plain-text `wallet show` never prints them). With neither, auto-groups held tokens by Cash Hub (only same-Hub tokens can merge) and consolidates each group with 2+ tokens — one group proceeds directly, several prompt interactively (or all process under `--json`/`--yes`). Every chosen group is attempted: one failing never cancels or hides the others (see **Partial success**). Cash notes merge too, with no identity needed: each is first moved onto a key made for that merge alone (cash_consolidate takes no cash-mode source), and your merged result is a fresh cash note. A refused merge moves them back to cash notes; one interrupted midway leaves them on that key, still yours and spendable, shown as `pending_merge` in `wallet show`/`balance` until a later `consolidate` finishes it |
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
| `conflict` | 5 | yes | collides with existing state (a token already held, a wallet's rate limit) |
| `network` | 6 | yes | couldn't reach a relay/wallet, or a Hub isn't ready yet (`NOT_READY`) |
| `auth` | 7 | no | not authorized, or no longer (a wallet declined as restricted/unauthorized/expired) |
| `indeterminate` | 8 | no | a `redeem`/`transfer`/`consolidate` got no single answer (timeout, omission, incomplete reply, a reply that contradicts itself, or a result cashctl could not read), or a transfer, merge or protect was answered with a code other than `BAD_REQUEST`, so whether it was applied is unknown — never resend it blind. A transfer, merge or protect stays an open call (named in the error, listed in `wallet show`) and its bills are not spent again until the Hub says what happened; for a redeem, check with `cash status --token <id>` first |
| `exists` | 9 | no | what you're trying to create already exists (`join`: you already hold a wallet from that circle) — don't retry, use the one you have. If an earlier `join` went unanswered (its key is under `pending_joins`), that wallet was made but its details never arrived and can't be fetched again: it is unusable — ask the circle's host to delete the wallet with that `connection_pubkey`, then join again |
| `spent` | 10 | no | the bill's Hub confirmed it is spent (a tombstone): its value has already moved. The local entry is marked spent and leaves `wallet balance`/`redeem --all`; never inferred from silence. Also what naming a token you redeemed or merged away yourself returns (`cash status --token`, `redeem`, `consolidate`), with no network call |
| `internal` | 1 | no | anything else — a wallet-side decline that isn't one of the above, or a cashctl-side failure |

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
