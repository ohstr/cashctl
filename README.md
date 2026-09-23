# >_ cashctl

[![Release](https://img.shields.io/github/v/release/ohstr/cashctl)](https://github.com/ohstr/cashctl/releases/latest)
[![CI](https://github.com/ohstr/cashctl/actions/workflows/ci.yml/badge.svg)](https://github.com/ohstr/cashctl/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/ohstr/cashctl.svg)](https://pkg.go.dev/github.com/ohstr/cashctl)
[![License: Unlicense](https://img.shields.io/badge/license-Unlicense-blue.svg)](LICENSE)

**A wallet CLI for [Cash](https://github.com/flokiorg/lokihub/blob/main/docs/nips/NIP-CASH.md)
and [Circle wallets](https://github.com/flokiorg/lokihub/blob/main/docs/nips/NIP-CW.md).**

## Features

- [`cashctl transfer`](#cashctl-transfer) — Send a held cash token
- [`cashctl decode`](#cashctl-decode) — Inspect any cash token, Circle Hub connection, or NWC URI locally, no network call
- [`cashctl receive`](#cashctl-receive) — "Cash-in" a token: verify and add it to your wallet
- [`cashctl redeem`](#cashctl-redeem) — Redeem a held cash token into a Lightning wallet
- [`cashctl consolidate`](#cashctl-consolidate) — Merge several held cash tokens into one
- [`cashctl cash list-recipients`](#cashctl-cash-list-recipients) — Check your allocation and co-recipients of a held token (network)
- [`cashctl join`](#cashctl-join) — Join a circle to get a personal Lightning wallet
- [`cashctl init`](#cashctl-init) — Set up your identity and, optionally, a wallet
- [`cashctl wallet show/history/use`](#cashctl-wallet-show) — Show your identity, registered wallets, and local action history
- [`cashctl wallet balance`](#cashctl-wallet-balance) — Sums every wallet's balance plus unredeemed held tokens into one figure
- [`cashctl wallet protect`](#cashctl-wallet-protect) — Re-key a still-shared cash-mode holding so the original secret can no longer spend it
- [`cashctl wallet <op>`](#cashctl-wallet-op) — Run ordinary NWC operations against whichever wallet is current
- [`cashctl connect add/list/use/rm`](#cashctl-connect-addlistuserm) — Register an NWC connection: a plain Lightning wallet you already have
- [`cashctl version`](#cashctl-version) — Print the cashctl version

`cashctl` mints nothing itself — minting is the Cash Hub operator's own tooling.

## Installation

**AI agents:**

```
Fetch https://ohstr.github.io/cashctl/PROMPT.md
```

See [Agent skills](#agent-skills) below for what that does.

**macOS / Linux:**

```sh
curl -fsSL https://ohstr.github.io/cashctl/install.sh | sh
```

Detects your OS and CPU (amd64/arm64) and installs to `/usr/local/bin`.
Falls back to `~/.local/bin` if that's not writable.

**Homebrew** (macOS/Linux):

```sh
brew install ohstr/tap/cashctl
```

**Windows (PowerShell):**

```powershell
irm https://ohstr.github.io/cashctl/install.ps1 | iex
```

**go install**:

```sh
go install github.com/ohstr/cashctl@latest
```

**Docker** — no toolchain required:

```sh
# :latest tracks the newest release, :edge tracks main — see Docker section below.
docker run --rm ghcr.io/ohstr/cashctl:latest --help
```

**From source** — see [Development](#development).

## `cashctl transfer`

Send a held cash token. The destination needs no prefix for
the common case — a hex pubkey, `npub1...`, a NIP-05 identifier
(`name@domain`, resolved live), or an `nconnection1...` are all recognized
by shape:

```sh
cashctl transfer 5                                 # no recipient — get a cash string to hand anyone
cashctl transfer npub1w0lxfr9...                    # transfer it all
cashctl transfer 3 alice@example.com                # split off 3, keep the rest as a new token
cashctl transfer cash
cashctl transfer connection:<platform>:<external-id>:<ia-pubkey>
cashctl transfer nconnection1... --ia ia@example.com
```

Equivalent, explicit-flag form for scripted/agentic use:
`cashctl transfer --to npub1w0lxfr9... --amount 3`.

An `nconnection1...` never carries an Identity Authority itself — resolving
one asks for one, via `--ia <identity>` (hex or NIP-05) or an interactive
prompt. Whenever a target gets resolved to something not obvious from what
you typed (a NIP-05 lookup, or an `nconnection1...`'s IA), cashctl shows it
back before using it.

**Cash selection**: when an amount is given and `--token` isn't, cashctl
picks which held token(s) reach it exactly, rather than just resolving
which one token to act on:

- one held token's amount matches exactly → a full transfer of it.
- one held token covers it → split-transfers the smallest one that does
  (best-fit, not the largest available).
- no single token covers it, but several from the same minter, summed,
  do → consolidates that subset into one token first, then transfers from
  the result — two chained calls shown as one confirmation.
- nothing covers it → refuses, naming exactly how much you hold and why
  it can't be reached (funds fragmented across separate Hubs), rather
  than silently sending as several transfers to different minters.

## `cashctl decode`

Inspect any cash token, Circle Hub connection (`circlehub1...`), or NWC
URI locally — no network call, and no wallet needed.

```sh
cashctl decode lokicash1...       # local-only, includes mint-signature verification if present
cashctl decode lokicash1... --check  # also cross-checks against the Hub
```

Without `--check`, an interactive session asks whether to run it (default
**no** — no network call at all unless you opt in); `--json` never prompts
and just skips it.

## `cashctl receive`

"Cash-in" a token.

```sh
cashctl receive lokicash1...
cashctl receive lokicash1...#deadbeef   # cash-mode: the combined "<token>#<cash_secret>" presentation
```

Decodes the token, prints its details, then cross-checks it against the
Cash Hub before saving — a token with no matching recipient, or one the
Hub can't be reached to confirm at all, is refused outright.

If you paste a Circle Hub or Cash Hub connection here instead of a token,
`cashctl` gives you a specific error pointing you to the right command.

**A cash-mode token is two values, not one.** `lokicash1...` alone only
decodes it — redeeming or transferring it needs the secret embedded,
`<token>#<cash_secret>`. There's no `--secret` flag: a cash-mode token
pasted without it just gets inspected and checked, never saved.

**A saved cash-mode receipt gets protected automatically** — re-keyed
under a fresh secret (defaults to yes; always proceeds under
`--yes`/`--json`) and merged with any other cash you hold from the same
issuer. A failure here doesn't fail the receive — retry later with
`cashctl consolidate --to cash`.

## `cashctl redeem`

Redeem a held cash token into a Lightning wallet.

```sh
cashctl redeem                          # auto-picks your one held token and default wallet
cashctl redeem work                     # into wallet "work" — or: --token tok-a1b2 --into work
cashctl redeem --invoice lnbc1...       # bypass both — redeem into any invoice, no cashctl wallet needed
```

If you hold more than one token and don't pass `--token`, `redeem`
(and `transfer`/`consolidate` too) shows a numbered list and asks which
one — `--token`/`--yes`/`--json` skip straight past it for scripted use.

Before confirming, `redeem` shows the expected fee (only when it's
actually non-zero — a same-node redeem is routinely free) and warns if
the token's redemption deadline is close or already passed. Every
money-moving confirmation (`redeem`/`transfer`/`consolidate`) defaults to
**no** on a bare Enter.

| Flag | Meaning |
|---|---|
| `--token` | which held token (auto-picked if you only hold one) |
| *(positional)*, or `--into` | destination wallet (default: your default wallet) |
| `--invoice` | redeem straight into this external invoice |
| `--as` | override credential — required for a connection-key-bound token |

## `cashctl consolidate`

Merge several held cash tokens into one.

```sh
cashctl consolidate                             # auto-detects which held tokens share a minter and merges each group
cashctl consolidate tok-a1b2 tok-c3d4            # just these two
cashctl consolidate --sources tok-a1b2,lokicash1...:5:pubkey:<privkey> --to pubkey:<hex>
```

With no IDs/`--sources` given, cashctl can't just merge *everything* —
only tokens sharing a minter can actually be combined — so it groups your
held tokens by minter and consolidates each group that has 2+ tokens
(a lone token from a minter needs nothing merged, and is left alone).
One group: it just proceeds. More than one: an interactive session asks
which group(s) to process (Enter for all); `--json`/`--yes` processes
every qualifying group, since there's no terminal to ask from.

Positional IDs (or `--sources`, comma-separated) skip all of that for
exact control — amount and credential are already known for each held
entry. Use the verbose `<token>:<amount-loki>:<credential>` form
(`--sources` only) for a source that isn't in your local ledger; the IDs
themselves are visible via `cashctl wallet show --json`; plain-text
`wallet show` never prints them (see `cashctl consolidate --help`).
`--to` defaults to your own identity; `--to cash` merges into a
fresh, anonymous cash note instead (needs Hub support). An
`nconnection1...` `--to` target needs `--ia <identity>` (hex or NIP-05) to
resolve its Identity Authority, same as `transfer`.

## `cashctl cash list-recipients`

Check your allocation and co-recipients of a held token — the same call
`cashctl receive` makes internally to check a token before saving it.

```sh
cashctl cash list-recipients               # your allocation + co-recipients of a held token
```

## `cashctl join`

Join a circle to get a personal Lightning wallet.

```sh
cashctl join <circlehub1... or NWC URI> 100
```

The self-service entry point into a circle. Give it a Circle Hub's
connection and the spend cap you want — both positional, in either order,
or via `--hub`/`--max-amount`. A cap is required — there's no "unlimited"
option.

`join` calls `create_circle_wallet` on your behalf and saves the resulting
wallet. If it's your first wallet, it also becomes your default.

```sh
cashctl join circlehub1... --max-amount 100 --budget-renewal monthly
```

| Flag | Meaning |
|---|---|
| *(positional)*, or `--hub` | the Circle Hub connection (required) |
| *(positional)*, or `--max-amount` | requested spend cap, in loki (required) |
| `--expiry` | requested expiry duration (default: the Hub's own) |
| `--budget-renewal` | `daily`\|`weekly`\|`monthly`\|`yearly`\|`never` (default: the Hub's own) |
| `--as` | override credential (defaults to your local identity) |

`join` is a top-level shortcut for `cashctl circle join`.

## `cashctl init`

Set up your identity and, optionally, a wallet.

```sh
cashctl init
```

Reuses your [ncli](https://github.com/ohstr/ncli) vault identity if you have
one. Otherwise it generates a new identity just for `cashctl`.

If you already have a Lightning wallet connection (NWC), `init` offers to
register it as your default. Run `init` again any time — it's idempotent,
and just reports where things stand.

**Note:** under `--json`, `init` generates a fresh local identity and
skips the wallet offer — there's no way to paste a connection string in
that mode.

```sh
cashctl init --json
# {
#   "npub": "npub1...",
#   "identity_source": "cashctl-local",
#   "default_wallet": ""
# }
```

## `cashctl wallet show`

Your identity, wallets, and history.

```sh
cashctl wallet show      # identity, registered wallets, held tokens
cashctl wallet history   # local action log (receive/redeem/transfer/...)
cashctl wallet use <name>  # switch your default wallet (also: cashctl connect use)
```

## `cashctl wallet balance`

Your unified balance.

```sh
cashctl wallet balance             # one number: every wallet + every held token, summed
cashctl wallet balance --breakdown # itemized, per-wallet/per-token (short: -v)
cashctl wallet balance --from work # just one wallet or held token
```

An expired wallet can't be queried live. `balance` falls back to the
last-known figure from your most recent successful check, marked
`stranded` (`[expired]` in text mode).

`cashctl balance` is also available as a top-level shortcut for `wallet
balance`.

## `cashctl wallet protect`

Re-key a cash-mode holding that's still shared, so the original secret can no
longer spend it. `receive` does this automatically; use this if that was
declined or failed.

```sh
cashctl wallet protect         # picks the holding for you
cashctl wallet protect <id>    # a specific held token (see `wallet show --json`)
cashctl wallet protect --token <id>  # same, by flag
```

## `cashctl wallet <op>`

Ordinary NWC wallet operations. Plain [NIP-47](https://github.com/nostr-protocol/nips/blob/master/47.md)
calls against whichever wallet is current (`-c/--connection` overrides it
for one call):

```sh
cashctl wallet get-info
cashctl wallet budget
cashctl wallet invoice 5000 --desc "coffee"
cashctl wallet pay lnbc1...
cashctl wallet list-tx
cashctl wallet sign-message "hello"
```

`invoice` and `pay` are also available as top-level shortcuts: `cashctl
invoice 5000` / `cashctl pay lnbc1...`.

## `cashctl connect add/list/use/rm`

Register a Lightning wallet you already have, over NWC — your own, or
one handed to you from another device:

```sh
cashctl connect add work nostr+walletconnect://...
cashctl connect list
cashctl connect use work
cashctl connect rm work
```

## `cashctl version`

Print the cashctl version.

```sh
cashctl version
```

## Agent skills

For coding agents: [AGENTS.md](AGENTS.md) points to the matching skill
under [`skills/`](skills/) — one per command group. Each skill works
standalone with just the `cashctl` binary on `PATH`.

```
npx skills add ohstr/cashctl --all -y
```

`cashctl --help` prints the complete command tree.

## Configuration

State lives under `$XDG_CONFIG_HOME/cashctl` (or `~/.config/cashctl` on
Linux/macOS) in a single SQLite database, `cashctl.db` (0600 — it can hold
a plaintext identity key and cash-mode spending secrets). Override the
location with `--config-dir`. Set `NO_COLOR` to disable ANSI color on
stderr.

**Breaking, if you used a pre-release build**: `cashctl.db` replaces the
three flat JSON files (`identity.json`, `connections.json`, `ledger.json`)
earlier builds used — no migration path, no dual-read. If you have
existing state in those files, back them up before upgrading; cashctl
won't see them anymore.

If you point `init` at an [ncli](https://github.com/ohstr/ncli) vault,
`cashctl` only reads it. Your vault stays at its own usual path, unaffected.
Set `NCLI_VAULT_PASSWORD` to unlock it non-interactively — needed for
`init` and other vault-backed commands run without a TTY (scripted/agentic
use).

## Docker

```sh
docker run --rm -v ~/.config/cashctl:/root/.config/cashctl ghcr.io/ohstr/cashctl:latest wallet show
```

The `:edge` tag tracks `main`; a versioned tag tracks that release.

## Development

Build from source with [`just`](https://github.com/casey/just):

```sh
just build   # go build -o cashctl .
```

## License

[Unlicense](LICENSE) — public domain.
