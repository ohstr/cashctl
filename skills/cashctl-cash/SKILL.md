---
name: cashctl-cash
description: Receive a NIP-CASH token into your local wallet (`cashctl receive`), redeem a held token into a Lightning wallet or raw invoice (`cashctl redeem`), send a held token to someone else (`cashctl transfer`), merge several held tokens into one (`cashctl consolidate`), and check its Hub-side recipients (`cashctl cash list-recipients`). For local-only inspection of a token string itself (no network call, including mint-signature verification), see `cashctl decode` in `skills/cashctl-wallet/SKILL.md`. Use whenever an agent is handed a lokicash1... (or other cash-token-family) string, needs to cash it out to Lightning, needs to forward it to another identity, or needs to combine multiple small tokens.
license: Unlicense
---

<!-- Mirrors ohstr/cashctl's cmd/cash_receive.go, cmd/cash_redeem.go,
cmd/cash_transfer.go, cmd/cash_consolidate.go, and cmd/cash_inspect.go
(list-recipients) as of writing. See skills/cashctl-wallet/SKILL.md for
cmd/decode.go. Self-contained by design — update by hand if flags/schemas
change. -->

# cashctl receive / redeem / transfer / consolidate / cash ...

## `cashctl receive <token>` — cash it in

```sh
cashctl receive lokicash1... --json                            # prints the token's details, then verifies against the Cash Hub
cashctl receive lokicash1...#deadbeef --json                   # bearer-mode: the combined "<token>#<bearer_secret>" presentation
```

Always a network call: receive prints the token's details, then
cross-checks it against the Cash Hub (the same `list_recipients` call
`cashctl cash list-recipients` makes) before saving anything. Anything
that doesn't check out — no matching recipient on the Cash Hub, or the
Cash Hub can't be reached at all — is refused outright; nothing gets
added to your wallet. Pasting a Circle Hub (`circlehub1...`) or Cash Hub
(`cashhub1...`) connection here instead of a token gets a specific
corrective error (`code: "invalid_input"`) naming the right command
(`cashctl join ...`, or "cashctl can't mint").

**A bearer-mode token (`identity_required: false`) is two values, not
one.** The token string only lets you dial the wallet — it is *never* a
valid spending credential by itself, no matter how it looks. The real
credential, `bearer_secret`, is a separate value the Hub operator hands
out once, alongside the token, at mint time, and it MUST arrive embedded
in the token itself: `<token>#<bearer_secret>` (NIP-CASH's combined
bearer-slice presentation — `#` never appears in a token's own bech32
charset, so splitting it back apart is always unambiguous). There is no
`--secret` flag: a bearer token pasted without the embedded secret has
nothing `receive` can act on, so it degrades to a read-only report
instead of erroring — `{"received": false, "reason":
"no_embedded_secret"}`, exit 0, nothing saved. `cashctl decode` on the
combined presentation reports `embedded_bearer_secret_present: true`
(never the secret's value) if you want to check which form you have
before receiving.

**A saved bearer-mode receipt is then offered automatic protecting.**
Anyone who saw the same bearer secret before it reached you — the
sender, or anyone the sender showed it to — could still spend it too,
for as long as it stays shared. Once `receive` saves a bearer-mode
entry, it asks to protect it immediately (defaults to yes; always
proceeds non-interactively under `--yes`/`--json` — there's no separate
flag to opt out). Protecting re-keys the slice under a fresh secret only
your wallet knows, and — if you already hold other cash mint-signed by
the same issuer — merges it into that holding in the same step. Reported
under `"secured"` in `--json` output: `{"status": "rekeyed"}` (re-keyed
in place), `{"status": "consolidated", "consolidated_with": [...],
"final_entry_id": "..."}` (merged into a new entry), `{"status":
"declined"}`, or `{"status": "not_applicable"}` (not a bearer-mode
receive). A wire failure here reports `{"status": "failed", "error":
"..."}` but never fails `receive` itself — the cash is already genuinely
yours; retry protecting later with `cashctl wallet protect [id]`
(re-keys a single still-shared holding in place; `consolidate --to
bearer-target` needs 2+ sources). Because the secret is always captured — and kept current
— up front, `redeem`/`transfer` never need a `--as bearer:<secret>`
override for a held token.

## `cashctl redeem` — cash it out

```sh
cashctl redeem --json                              # auto-picks your one held token + default wallet
cashctl redeem work --token tok-a1b2 --json         # positional wallet name — or --into work
cashctl redeem --invoice lnbc1... --json           # bypasses both — any invoice, no cashctl wallet needed
```

If you hold more than one token and `--token`/`--json`/`--yes` isn't given,
`redeem` (and `transfer`/`consolidate` below) prompts interactively with a
numbered list instead of failing outright — under `--json`/`--yes`/a
non-interactive script, `--token <id>` is still required (`cashctl wallet
show` lists held-token IDs), since there's no terminal to prompt from. A
**connection-key-bound** token needs an explicit `--as
connection-key:<privkey>,<platform>,<external-id>,<attestation-file>` —
cashctl does not auto-refresh a connection-key credential from a relay
(a deliberate scope limit: re-deriving a fresh live attestation isn't
automatic), so the error names exactly what to pass.

In an interactive (non-`--json`/`--yes`) session, the confirmation prompt
shows the expected fee (only if non-zero) and warns if the token's own
redemption deadline is close or already passed, and — like `transfer`/
`consolidate` — defaults to **no** on a bare Enter. None of this affects
`--json`/`--yes` calls: the prompt (and the live lookup behind the fee/
expiry preview) is skipped entirely in that mode, same as always.

## `cashctl transfer` — send it to someone else

The destination needs no prefix for the common case — cashctl sniffs a
bare hex pubkey, `npub1...`, a NIP-05 identifier (`name@domain`, resolved
live against the domain's own `/.well-known/nostr.json`), or an
`nconnection1...` by shape:

```sh
cashctl transfer npub1w0lxfr9... --json                     # send it all
cashctl transfer 3 alice@example.com --json                  # split off 3, keep the rest as a new held token
cashctl transfer bearer-target --json
cashctl transfer connection:<platform>:<external-id>:<ia-pubkey> --json
cashctl transfer nconnection1... --ia ia@example.com --json  # or hex — see below
```

Equivalent explicit-flag form for scripted/agentic use — same
auto-detection applies to the `--to` value either way:

```sh
cashctl transfer --to npub1w0lxfr9... --amount 3 --json
```

An `nconnection1...` never carries an Identity Authority itself (a local,
sender-side trust decision, not part of the shareable connection string).
Resolving one asks for one: `--ia <identity>` (hex pubkey or NIP-05)
supplies it non-interactively; without it, an interactive session prompts
for it, and a `--json`/`--yes` call gets `code: "usage"` naming `--ia`
instead.

Whenever something beyond a bare hex/npub gets resolved (a NIP-05 lookup,
or an `nconnection1...`'s IA), the result is shown back before it's used —
a `resolves to:` line in text mode, and always present as
`target_resolved` in `--json` output (empty string when nothing needed
resolving) — never trust an auto-detected identity silently.

A successful split transfer's remainder is saved back into your own
ledger automatically — check the response's `remainder_entry` field
(`--json`) or the new `tok-...` ID printed in text mode.

**Cash selection**: giving an amount without `--token` doesn't just pick
*a* held token — cashctl picks which one(s) reach that amount exactly:
one token matching exactly (full transfer), the smallest single token
that covers it (split, best-fit), or — if no single token covers it but
several from the same minter, summed, do — consolidating that subset
first, then transferring from the result (two chained calls under one
confirmation; under `--json`/`--yes` this executes without re-confirming
the consolidate step separately). If nothing covers it at all, the call
fails with `code: "usage"` naming exactly how much is held and that funds
are fragmented across separate minters/Hubs, rather than silently
splitting the send across several transfers.

## `cashctl consolidate` — merge several into one

```sh
cashctl consolidate --json                                    # no sources given: auto-groups held tokens by minter, merges each group
cashctl consolidate tok-a1b2 tok-c3d4 --json                   # positional IDs — or --sources tok-a1b2,tok-c3d4
cashctl consolidate --sources tok-a1b2,lokicash1...:5:pubkey:<privkey> --to pubkey:<hex> --json
```

Positional IDs, or `--sources` comma-separated: a bare ID already in your
ledger (amount/credential resolved automatically), or the verbose
`<token>:<amount-loki>:<credential>` form (`--sources` only) for a source that
isn't held locally — only `pubkey:`/`bearer:` credentials work in that
verbose form (a `connection-key:` value has its own embedded commas,
ambiguous in this shorthand — receive it into your ledger first instead).
IDs are discoverable via `cashctl wallet show --json`'s `held_tokens`
array — plain-text `wallet show` deliberately never prints them.
Needs at least 2 sources per call.

With neither positional IDs nor `--sources` given: only same-minter
tokens can actually be merged, so cashctl groups held tokens by minter
and consolidates each group with 2+ tokens (a lone token from a minter
needs no merge, and is skipped) — under `--json`, every qualifying group
is processed with no prompt. Response shape depends on how many groups
were actually processed: exactly one (the common case) returns the same
`{"new_entry", "expires_at", "target_resolved"}` object as the
explicit-sources form always has; more than one returns
`{"consolidated": [{"new_entry", "expires_at", "target_resolved"}, ...]}`
instead — check which key is present rather than assuming one shape.
`--to` defaults to your own identity; `--to bearer-target` merges into a
fresh, anonymous bearer note instead (same keyword `transfer` uses) —
requires a Hub that accepts a bearer `cash_consolidate` target.

## `cashctl cash list-recipients`

```sh
cashctl cash list-recipients --token tok-a1b2 --json  # network call — your allocation + co-recipients
```

For mint-signature verification or a plain field dump of a token
(`wallet_pubkey`, `relays`, `mint_signature_valid`/`minter_pubkey` if
present, ...), use the general-purpose `cashctl decode lokicash1...`
instead (see `skills/cashctl-wallet/SKILL.md`) — it never touches the
network, works on any token string held or not, and is the cheapest way to
inspect one before deciding whether to `receive` it at all.
