# cashctl

`cashctl` is a Go CLI wallet for [NIP-CASH](https://github.com/flokiorg/lokihub/blob/main/docs/nips/NIP-CASH.md)
cash tokens and [NIP-CW](https://github.com/flokiorg/lokihub/blob/main/docs/nips/NIP-CW.md)
circle wallets.
Assume the `cashctl` binary is already on `PATH`. State (identity, registered
wallets, held tokens) lives under `$XDG_CONFIG_HOME/cashctl`, overridable with
`--config-dir`. `NCLI_VAULT_PASSWORD` unlocks an ncli vault-sourced identity
non-interactively (no TTY to prompt from — needed for `init`/vault-backed
commands run unattended). `NO_COLOR` disables ANSI color on stderr.

## Commands

| Command | Purpose |
|---|---|
| `cashctl init` | Set up your identity (reusing an ncli vault entry if you have one) and optionally a default wallet |
| `cashctl join <hub-connection> <max-amount>` | Join a circle via its Circle Hub connection (`circlehub1...` or a raw NWC URI) and a requested spend cap in loki — both positional, either order, or `--hub`/`--max-amount`. A cap is required; NIP-CW has no "0 means unlimited" convention |
| `cashctl circle join <hub-connection> <max-amount>` | Same as `join` — the canonical, fully-namespaced form |
| `cashctl wallet show` | Your identity, registered wallets, and held cash tokens |
| `cashctl wallet history` | Local action log (receive/redeem/transfer/consolidate) |
| `cashctl wallet use <name>` / `cashctl connect use <name>` | Switch your default wallet |
| `cashctl wallet balance [--breakdown\|-v] [--from <name>]` | Unified balance: every wallet's live balance + every held token's value |
| `cashctl wallet protect [id] [--token <id>]` | Re-key a still-shared cash-mode holding so the original code can no longer spend it — `receive` does this automatically; use this if that was declined or failed |
| `cashctl wallet get-info` / `budget` / `invoice <amount>` / `pay <invoice>` / `list-tx` / `sign-message <msg>` | Ordinary NIP-47 calls against the current wallet |
| `cashctl invoice <amount>` / `cashctl pay <invoice>` / `cashctl balance` | Top-level shortcuts for `wallet invoice`/`wallet pay`/`wallet balance` |
| `cashctl connect add <name> <connection>` / `list` / `rm <name>` | Register/list/remove any other NWC connection |
| `cashctl decode <string> [--check]` | Inspect any cash token, Circle Hub connection (`circlehub1...`), or NWC URI locally, no network call; `--check` opts into a read-only Hub check (cash token: matching recipient; circle hub: can we join) |
| `cashctl receive <token>` | Decode a cash token, print its details, then cross-check it against the Cash Hub before adding it to your wallet — refuses anything that doesn't check out. A cash-mode token's `cash_secret` must be embedded, `<token>#<cash_secret>` (NIP-CASH's combined cash-mode slice presentation) — pasted bare, it degrades to a read-only report instead of erroring. A saved cash-mode receipt is then offered automatic protecting: re-keyed under a fresh secret (and merged with any other same-issuer holding), reported under `"secured"` |
| `cashctl redeem [wallet] [--token <id>] [--invoice <bolt11>] [--as <credential>]` | Redeem a held token into a wallet (positional, or `--into`) or a raw invoice |
| `cashctl transfer [amount] [target] [--as <credential>]` | Send a held token — amount and target are positional (either order), or `--to`/`--amount`. An amount with no target defaults to a cash note (a `<token>#<secret>` string to hand anyone); neither one is a usage error. With an amount and no `--token`, cash selection picks which held token(s) reach it exactly (auto-consolidating a same-minter subset first if no single token covers it) instead of just picking one token to act on |
| `cashctl consolidate [id...] [--to <target>]` | Merge several held tokens into one — positional IDs, or `--sources`, for exact control (IDs discoverable via `wallet show --json`; plain-text `wallet show` never prints them). With neither, auto-groups held tokens by minter (only same-minter tokens can merge) and consolidates each group with 2+ tokens — one group proceeds directly, several prompt interactively (or all process under `--json`/`--yes`) |
| `cashctl cash list-recipients [--token <id>]` | Your allocation + co-recipients of a held token (network) |
| `cashctl cash receive` / `redeem` / `transfer` / `consolidate` | Same as the top-level forms above — the canonical, fully-namespaced versions |
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
`transfer`, `consolidate`, `cash list-recipients`, and `decode` reject
outright (`usage`, exit 2): none of them ever dial a registered wallet, so
there's nothing for it to override. `--yes` (or `--json`, which implies
it) skips confirmation prompts. Every command is JSON-only-on-request
(human text by default, `--json` for the machine shape) — there is no
command that is JSON-only always.

**Failures**: exactly one top-level error report, always on stderr — a
plain `Error: ...` line by default, or `{"error", "code", "retryable",
"input"?, "nwc_code"?}` with `--json`:

| `code` | exit | retryable | meaning |
|---|---|---|---|
| `usage` | 2 | no | bad/missing/conflicting flags or args, or a group command invoked without a subcommand |
| `invalid_input` | 3 | no | a supplied value failed validation/parsing (bad token, connection string, credential syntax, amount, ...) |
| `not_found` | 4 | no | the referenced thing doesn't exist (held token, registered wallet, vault entry, ...) — includes "no wallet configured yet" |
| `conflict` | 5 | yes | collides with existing state (a token already held, a wallet's rate limit) |
| `network` | 6 | yes | couldn't reach a relay/wallet |
| `auth` | 7 | no | not authorized, or no longer (a wallet declined as restricted/unauthorized/expired) |
| `internal` | 1 | no | anything else — a wallet-side decline that isn't one of the above, or a cashctl-side failure |

`input`, when present, is the single specific value that caused the
failure — **never** raw secret material: an `nsec1...`-shaped or bare
64-hex-char value is redacted to `""`, and a `pubkey:<privkey>`/`cash:
<secret>`/`connection-key:<privkey>,...` credential string has just its
secret component blanked (`pubkey:<redacted>`, etc.), keeping the rest of
the string legible in the error. `retryable` lets an agent decide whether
to back off and retry (`conflict`/`network`) or fix the input and try
again (everything else) without string-matching the message. `nwc_code`,
when present, is the raw NIP-47 error code (`RESTRICTED`, `EXPIRED`,
`INSUFFICIENT_BALANCE`, ...) a wallet returned — cashctl's own 7-code table
is deliberately coarse, so this is there for an agent that needs
finer-grained branching. A usage mistake in `--json` mode skips the
human-readable help dump (which would otherwise land on stdout) in favor
of the structured error alone.

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
