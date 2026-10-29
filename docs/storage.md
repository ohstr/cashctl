# cashctl.db

cashctl keeps all its state in one SQLite file, `cashctl.db`, under the config directory (`--config-dir`,
else `$XDG_CONFIG_HOME/cashctl`), mode 0600. It holds the identity key and cash secrets in the clear: back it up
like a wallet, and keep it until every open call and withdrawal is finished.

The file records its schema in `meta.schema_version`. This cashctl writes version **4**.

| version | written by | read by this cashctl |
|---|---|---|
| 4 | 0.7.0-rc.2 and later | as is |
| 3 | 0.7.0-rc.1 | migrated to 4 in place the first time it is opened (below) |
| 2 or older, or no `meta` | pre-release builds | refused, never read: move the file aside |

## Migration from version 3

One transaction, under the write lock, the first time any command opens the file:

- **entries**: every token is rewritten in its one spelling (lowercase, the known TLV entries in ascending type
  order, unknown types dropped; see `docs/client-rules.md` A6), with the wallet pubkey it names. Version 3 keyed
  bills by their string, so one bill received under two spellings could be held twice. Where one wallet pubkey
  has several rows that are `held`, `redeeming` or `zap_claiming`, one stays live: the one an open call spends,
  then the one re-keyed furthest, then the one received last. Each other becomes `duplicate`: counted nowhere,
  offered to nothing, every secret kept, with a `migrate` line in `wallet history`. The token's `UNIQUE` is
  replaced by `idx_entries_live_wallet`, one live row per wallet pubkey.
- **discover_events** gains `relay`, the relay that first served the event (a recommendation's hint).
- **withdrawals** gains `receipt_invalid_seen`, the answers in a row whose receipt did not verify.

No row is deleted.

## Holding statuses

`entries.status` is one of `held`, `redeeming`, `zap_claiming` (live: at most one row per wallet pubkey),
`redeemed`, `transferred`, `consolidated`, `withdrawn`, `spent`, `expired`, `reclaimed`, `not_received` and
`duplicate`. `entries.created_by_call` names the call of this device that made the bill (a mint, a split's
remainder, a merge, an order, a cancelled payout's re-mint; `local` for one with no call row); it is empty for a
bill received from someone else, whose amount others may know.

## Rotation rows

A member's own rotate request (`circle request rotate`, NIP-CW §Recovering a Lost Connection) is a row of the
`joins` table, as a join is: written ahead with the new connection key and secret before the signed request is
printed, looked up with the join lookup naming that key, and moved through the same states (`written`,
`pending`, `done`, `not_applied`, `shown`). What makes it a rotation is its `terms` column, which holds this JSON
object, and nothing else:

```json
{
  "purpose": "rotation",
  "wallet_pubkey": "<64 hex: the wallet the request moves to the row's connection_pubkey>",
  "event": { "kind": 23199, "...": "the member request exactly as signed" }
}
```

| key | value |
|---|---|
| `purpose` | always `"rotation"`; a row is a rotation if and only if its `terms` parse as JSON with this value |
| `wallet_pubkey` | the circle wallet being rotated |
| `event` | the signed member request, kept byte for byte so it can be shown again |

For a rotation row, `wallet_name` is that wallet's name here and `deadline` is the request's `created_at` plus 24
hours, the last moment a Hub accepts it. A join's `terms` is the join request cashctl sent, which never has a
`purpose` key. The format needs no column of its own, so a rotation row reads the same before and after the
migration from version 3.
