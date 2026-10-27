---
name: cashctl-circle
description: Join a circle to get a personal Lightning wallet (`cashctl join`, aliasing `cashctl circle join`), the self-service call into a Circle Hub's create_circle_wallet. Use when handed a circlehub1... connection string and asked to join/onboard/get a wallet from it. For local-only inspection of a circlehub1... string itself, see `cashctl decode` in `skills/cashctl-wallet/SKILL.md`.
license: Unlicense
---

<!-- Mirrors ohstr/cashctl's cmd/circle.go and cmd/shortcuts.go as of
writing. Self-contained by design — update by hand if flags/schemas
change. -->

# cashctl join / cashctl circle join

```sh
cashctl join circlehub1... 100 --json
cashctl join - 100 --json < hub.txt    # "-" reads the connection from stdin, out of ps and shell history
```

`join` is a top-level shortcut for `cashctl circle join` — identical flags
and behavior; the fully-namespaced form exists for scripted/agentic use.
Both forms work identically; prefer `join`.

| Flag | Meaning |
|---|---|
| *(positional)*, or `--hub` (required) | the Circle Hub's `circlehub1...` string (a NWC URI is refused) |
| *(positional)*, or `--max-amount` (required) | requested spend cap, in loki — either positional order works (`join <hub> <amount>` or `join <amount> <hub>`), sniffed by shape |
| `--expiry` | requested expiry duration (`0` = the Hub's own default) |
| `--budget-renewal` | `daily`\|`weekly`\|`monthly`\|`yearly`\|`never` (default: the Hub's own) |
| `--as` | override credential — `pubkey:<privkey>` (the only NIP-CW credential mode); defaults to your local identity |
| `--max-receive` | requested receive cap, in loki — required unless the Hub reports a default (`per_wallet_max_receive_millis`) |
| `--allow-unverified` | use a string whose signature doesn't verify; ask the user first, never pass it on your own |
| `--backup-file` | when the Hub can't rotate a lost connection: write the new wallet's fields there (0600) |

`--hub`/`--max-amount` are kept working exactly as before for
scripted/agentic use — the positional form above is just the shorter path
for everything else. A cap is required either way: NIP-CW has no "0 means
unlimited" wire convention, so omitting it is rejected locally before
ever dialing the Hub (the Hub would reject it too, just after a
round-trip).

```sh
# {"wallet": "circle:<label-or-n>", "default": true, "response": {"wallet_pubkey", "relays", "connection_pubkey", "expires_at", "max_amount_millis", "max_amount", "max_receive_millis", "fees_ppm", "budget_renewal", "dust_threshold_millis", "rotation_delay_secs"}, "terms", "warnings", "network", "chain", "test_network", "hub_verified"}
cashctl join circlehub1... 100 --json
```

**Every join is private.** The request goes over NIP-CASH's private
transport, so the circle's other members don't see your pubkey. When the
Hub's signed announcement can't be found, nothing is sent: `network`,
`reason: join_unavailable` — retry later. A circle not taking members now is
`auth`, `reason: not_accepting`.

**The string is checked.** Before anything is sent, the Hub must answer as
the identity and chain the `circlehub1` names; otherwise `auth`,
`reason: connection_mismatch` — never retry with a string from the same
place. A string whose signature doesn't verify (`hub_verified: false`) is
used only after the user confirms it once (the prompt, or
`--allow-unverified` once they agreed); under `--json` without the flag it
is `usage`, `reason: connection_unverified`, nothing sent. `network` other
than `mainnet` is a test network (`test_network: true`): not real money.

`max_amount_millis` (`max_amount`: the same in loki) is the cap the Hub actually granted, which can be lower
than the one you asked for (text mode says so). `fees_ppm` is the circle's
in-circle fee, fixed at join.

The wallet's connection key is made on your device: `join` generates it and
writes the join down (secret, deadline) before asking; the Hub sees only
`connection_pubkey`, and the wallet is stored as its fields, never a URI.
If the reply is lost, `join` looks the join up at once; if it is still
unknown, `join` exits `indeterminate` (8) with `call.method:
create_circle_wallet` and `settle_after`, and `cashctl cash settle` finishes
it later (the wallet the Hub made is registered then). An `unknown` lookup
means no wallet was made: join again (with a new key, never automatically).
An expired one stays listed under `pending_joins` in `wallet show`.

When the Hub can't rotate a lost connection (`rotation_delay_secs` absent),
`join` warns first (`warnings`): losing the wallet's key loses its balance.
Pass `--backup-file <path>` to keep a copy.

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
| Hub still starting up | `NOT_READY` | `network` | yes, after a short wait (see below) |
| asking too often | `RATE_LIMITED` | `conflict` | yes, after a wait |
| not a member of this circle | `RESTRICTED` | `auth` | no |
| you already hold a wallet here | `WALLET_EXISTS` | `exists` | no — use the wallet you have |

`NOT_READY` means the Hub cannot check circle membership yet (typically its
relays are not reachable). `decode --check` does not detect this. Retry with
a growing delay — about 5s, 15s, then 60s — and if it persists past a few
minutes, it is for the circle's host to fix, not something to keep retrying.

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
