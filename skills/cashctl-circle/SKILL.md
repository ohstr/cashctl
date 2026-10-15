---
name: cashctl-circle
description: Join a circle to get a personal Lightning wallet (`cashctl join`, aliasing `cashctl circle join`), the self-service call into a Circle Hub's create_circle_wallet. Use when handed a circlehub1... connection string (or a raw NWC URI for a Hub that hasn't adopted the bech32 form) and asked to join/onboard/get a wallet from it. For local-only inspection of a circlehub1... string itself, see `cashctl decode` in `skills/cashctl-wallet/SKILL.md`.
license: Unlicense
---

<!-- Mirrors ohstr/cashctl's cmd/circle.go and cmd/shortcuts.go as of
writing. Self-contained by design — update by hand if flags/schemas
change. -->

# cashctl join / cashctl circle join

```sh
cashctl join circlehub1... 100 --json
```

`join` is a top-level shortcut for `cashctl circle join` — identical flags
and behavior; the fully-namespaced form exists for scripted/agentic use.
Both forms work identically; prefer `join`.

| Flag | Meaning |
|---|---|
| *(positional)*, or `--hub` (required) | the Circle Hub connection: `circlehub1...` (recommended) or a raw NWC URI |
| *(positional)*, or `--max-amount` (required) | requested spend cap, in loki — either positional order works (`join <hub> <amount>` or `join <amount> <hub>`), sniffed by shape |
| `--expiry` | requested expiry duration (`0` = the Hub's own default) |
| `--budget-renewal` | `daily`\|`weekly`\|`monthly`\|`yearly`\|`never` (default: the Hub's own) |
| `--as` | override credential — `pubkey:<privkey>` (the only NIP-CW credential mode); defaults to your local identity |

`--hub`/`--max-amount` are kept working exactly as before for
scripted/agentic use — the positional form above is just the shorter path
for everything else. A cap is required either way: NIP-CW has no "0 means
unlimited" wire convention, so omitting it is rejected locally before
ever dialing the Hub (the Hub would reject it too, just after a
round-trip).

```sh
# {"wallet": "circle:<label-or-n>", "default": true, "response": {"wallet_pubkey", "relays", "connection_pubkey", "expires_at", "max_amount_millis", "fees_ppm", "budget_renewal"}}
cashctl join circlehub1... 100 --json
```

`max_amount_millis` is the cap the Hub actually granted, which can be lower
than the one you asked for (text mode says so); it is `null` if the Hub did
not say. `fees_ppm` is the circle's in-circle fee, fixed at join.

The wallet's connection key is made on your device: `join` generates it,
saves its secret before asking, and sends the Hub only `connection_pubkey`.
The Hub never sees the secret and cannot send a connection string back, so
there is no "show it again": the wallet saved under `wallet` is the only
copy. If the Hub never answers, whether the wallet was created is unknown;
the key stays listed under `pending_joins` in `wallet show`. If a later
`join` then says `exists`, that wallet is the one with that
`connection_pubkey`: ask the circle's host to delete it, then join again.
A Hub from before member-generated keys answers with no relays; `join`
then fails as `internal` (not retryable: the wallet was likely made), and
the key is kept the same way.

A `--hub` value that's actually a **Cash Hub** connection (`cashhub1...`)
gets a specific corrective error (`code: "invalid_input"`) — cashctl can't
mint, and joining isn't the right verb for a Cash Hub anyway. The
resulting personal wallet is saved to your wallet inventory under a
generated name and set as your default if it's your first-ever wallet
(`--json` always accepts the default-wallet prompt; text mode asks unless
`--yes`).

A Hub's refusal tells you whether to retry — branch on `code`, with
`nwc_code` for the detail:

| refusal | `nwc_code` | `code` | retry? |
|---|---|---|---|
| Hub still starting up | `NOT_READY` | `network` | yes, after a short wait |
| asking too often | `RATE_LIMITED` | `conflict` | yes, after a wait |
| not a member of this circle | `RESTRICTED` | `auth` | no |
| you already hold a wallet here | `WALLET_EXISTS` | `exists` | no — use the wallet you have |

Two joins from one identity within the same second look like a replayed
proof to the Hub; `join` then waits a second and asks again once, so the
answer you get is the real one (often `exists`, if the first join landed).

A Hub that predates `WALLET_EXISTS` refuses with `RESTRICTED` and an
"already has an active circle wallet" message; `join` reports that as
`exists`/`WALLET_EXISTS` too.

There is no `circle leave`/`circle list` — a circle membership is just an
ordinary registered wallet from this point on (`cashctl wallet show`,
`cashctl connect rm <name>` to drop it).

To inspect a `circlehub1...` connection locally without joining it, use
the general-purpose `cashctl decode circlehub1...` (see
`skills/cashctl-wallet/SKILL.md`) — the same TLV payload `join --hub`
decodes internally before dialing, just exposed directly.

## End-to-end: join, then use it like any other wallet

"a circle membership is just an ordinary registered wallet from this
point on" (above) means every `skills/cashctl-wallet/SKILL.md` NIP-47
example applies immediately after joining, with no extra step:

```sh
cashctl join circlehub1... 100 --json         # {"wallet": "circle:1", "default": true, ...}
cashctl wallet budget --json                  # confirm the spend cap the Hub actually granted
cashctl wallet invoice 5 --desc "test" --json  # already the default wallet — no --connection needed
```

Use `-c circle:1` (the name `join` reported) on any `wallet ...` call to
target this membership without it being the default, e.g. right after
joining a second circle.
