---
name: cashctl-wallet
description: Set up a cashctl identity and default wallet (`cashctl init`), manage registered NWC connections (`cashctl connect add/list/use/rm`, `cashctl wallet use`), inspect identity/wallets/history (`cashctl wallet show/history`), check a unified balance across every wallet and held cash token (`cashctl wallet balance`), make ordinary NIP-47 calls against the current wallet (`cashctl wallet get-info/budget/invoice/pay/list-tx/sign-message`), and locally decode any cash token, Circle Hub connection, or NWC URI (`cashctl decode`). Use when setting up cashctl for the first time, registering a plain Lightning wallet connection, switching the default wallet, checking balance/budget, paying/creating a Lightning invoice, or inspecting a pasted token/connection string before acting on it.
license: Unlicense
---

<!-- Mirrors ohstr/cashctl's cmd/wallet_init.go, cmd/connect.go, cmd/wallet.go,
cmd/wallet_balance.go, cmd/wallet_ops.go, and cmd/decode.go as of writing.
Self-contained by design — update by hand if flags/schemas change. -->

# cashctl init / wallet / connect / decode

## `cashctl init` — first-time setup

```sh
cashctl init            # interactive: offers an existing ncli vault entry, or generates one
cashctl init --json      # scripted: always generates fresh, skips the wallet-connection offer
```

Idempotent: re-running it just reports `{"already_configured": true}`
(`--json`) or a one-line message (text mode) rather than erasing/
regenerating anything. There is no `--force`/reset flag — remove
`cashctl.db` under `cashctl`'s config dir (see `cashctl wallet show` for
where that is, or pass `--config-dir` to point cashctl at a fresh one) if
you genuinely want to start over. Note this wipes *all* local state
(identity, registered wallets, held tokens, history), not identity alone —
they all now live in that one database.

Under `--json`, the wallet-connection offer is always skipped (there's no
way to paste a connection string non-interactively in that one prompt) —
register one afterward with `cashctl connect add` instead.

## `cashctl connect` / `cashctl wallet use` — managing wallet connections

```sh
cashctl connect add work nostr+walletconnect://...
cashctl connect list --json
cashctl wallet use work    # same as `cashctl connect use work`
cashctl connect rm work
```

`connect add`'s first-ever wallet is auto-set as default under `--json`
(and interactively offered under text mode); every subsequent one is only
offered, never auto-set. A wallet is looked up by name everywhere a
connection value is expected (`--to`, `-c/--connection`, `wallet use`) — if
the name isn't found, most of these accept the raw value directly instead
(lets a script pass an inline URI without a prior `connect add`).

## `cashctl wallet show` / `history`

```sh
cashctl wallet show --json     # {"npub", "identity_source", "wallets", "default_wallet", "held_tokens"}
cashctl wallet history --json  # {"history": [{"at","action","detail"}, ...]}
```

## `cashctl wallet balance` — the unified figure

```sh
cashctl wallet balance --json                # {"total_mloki", "stranded_mloki", "breakdown"}
cashctl wallet balance --breakdown --json    # same, always includes the itemized "breakdown" array
cashctl wallet balance --from work --json    # one wallet/token only: {"name","amount_mloki",...}
```

Sums every registered wallet's live `get_balance` result plus every
unredeemed held cash token's cached amount — the way a real wallet app
shows "your balance," not a protocol inventory. An unreachable wallet is
silently omitted from the sum (not fatal to the whole command) **except**
one declining with the NIP-47 `EXPIRED` code specifically, which falls
back to the last-known cached figure, marked `"stranded": true` — so an
expired wallet's balance is never just invisible, but is clearly flagged
as no longer money-moving.

## `cashctl wallet get-info` / `budget` / `invoice` / `pay` / `list-tx` / `sign-message`

Ordinary NIP-47 calls against whichever wallet is current (override with
`-c/--connection <name-or-raw-value>` for one call, without switching the
default):

```sh
cashctl wallet get-info --json
cashctl wallet budget --json
cashctl wallet invoice 5 --desc "test" --json      # amount is loki; prints {"invoice", "payment_hash", ...}
cashctl wallet pay lnbc1... --json
cashctl wallet list-tx --json
cashctl wallet sign-message "hello" --json
```

`cashctl invoice <amount>` / `cashctl pay <invoice>` are top-level shortcuts
for `wallet invoice`/`wallet pay` — identical flags and output, just
without the `wallet` prefix.

Any wallet decline surfaces as `code: "auth"` (restricted/unauthorized/
expired), `code: "conflict"` (rate-limited, retry), or `code: "internal"`
(insufficient balance, payment failed, or anything else) — check the
`nwc_code` field in `--json` output for the exact NIP-47 reason.

## `cashctl decode` — inspect anything, locally

```sh
cashctl decode lokicash1... --json      # {"type":"cash_token","hrp","wallet_pubkey","relays","identity_required",...}
cashctl decode lokicash1...#deadbeef --json  # bearer combined presentation — adds "embedded_bearer_secret_present":true
cashctl decode circlehub1... --json     # {"type":"circlehub","hrp","wallet_pubkey","relays","label"}
cashctl decode nostr+walletconnect://... --json  # {"type":"nwc_uri","wallet_pubkey","relays"}
cashctl decode lokicash1... --check --json    # adds "check":{"ok","amount_millis"?,"error"?} — a network call
cashctl decode circlehub1... --check --json   # adds "check":{"ok","error"?} — is joining even possible here
```

The one general-purpose local decoder: paste in whichever of the three
connection/token shapes cashctl accepts elsewhere and it figures out which
one it is (same sniffing `receive`/`join --hub`/`connect add` already use)
and dumps its fields. No network call, nothing dialed or held by default —
safe to run on anything before deciding what to do with it. A cash token's
optional mint-provenance pair only appears (`mint_signature`,
`attested_amount_millis`) when the token actually carries one. A **Cash
Hub** connection (`cashhub1...`) gets a specific corrective error instead
of a decode — cashctl has no mint capability and there's no local decoder
for that format. **The pairing secret is never included in the output,
for any of the three shapes** — this is inspection only, never a way to
extract a working spending/dialing credential. A bearer-mode cash token
MAY arrive as `<token>#<bearer_secret>` (NIP-CASH's combined
bearer-slice presentation — `cashctl receive` accepts it directly; there
is no `--secret` flag at all, so this embedded form is the only way a
bearer token's secret ever reaches `receive`) — decode splits it
automatically and reports only `embedded_bearer_secret_present: true`,
never the secret's value, same rule as the token's own pairing secret.

**`--check` opts into one read-only network round trip** — the only
exception to "no network call," and only for the two shapes where a
Hub-side answer actually exists: a **cash token** gets the exact check
`cashctl receive` makes before saving it (matching recipient on the Hub,
same `code`-free `{"ok","amount_millis"}`/`{"ok":false,"error"}` shape),
just without ever saving anything; a **circlehub1...** connection gets
"is joining even possible here" — reachable, and the Hub actually
advertises `create_circle_wallet` — which is *not* the same as "this
identity is allowlisted to join" (only `cashctl join` itself, by actually
attempting it, can tell you that). `--check` is a no-op for an NWC URI —
`cashctl connect add` already answers "is it reachable" by dialing for
real.
