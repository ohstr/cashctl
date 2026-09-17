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
# {"wallet": "circle:<label-or-n>", "default": true, "response": {...}}
cashctl join circlehub1... 100 --json
```

A `--hub` value that's actually a **Cash Hub** connection (`cashhub1...`)
gets a specific corrective error (`code: "invalid_input"`) — cashctl can't
mint, and joining isn't the right verb for a Cash Hub anyway. The
resulting personal wallet is saved to your wallet inventory under a
generated name and set as your default if it's your first-ever wallet
(`--json` always accepts the default-wallet prompt; text mode asks unless
`--yes`).

There is no `circle leave`/`circle list` — a circle membership is just an
ordinary registered wallet from this point on (`cashctl wallet show`,
`cashctl connect rm <name>` to drop it).

To inspect a `circlehub1...` connection locally without joining it, use
the general-purpose `cashctl decode circlehub1...` (see
`skills/cashctl-wallet/SKILL.md`) — the same TLV payload `join --hub`
decodes internally before dialing, just exposed directly.
