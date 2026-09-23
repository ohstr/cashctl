# Integration tests

A black-box suite that drives the *compiled* `cashctl` binary as a real user
would, against a real, already-running [lokihub](https://github.com/ohstr/lokihub)
instance — as opposed to the unit tests under `internal/`, which never touch
a network.

Excluded from normal `go test`/CI runs by the `integration` build tag:

```sh
go test -tags integration ./integration/...
```

## Setup

1. Copy `config.local.yaml.example` to `config.local.yaml` (gitignored —
   never committed).
2. Point `admin_api.base_url` at a running lokihub instance's admin HTTP API
   (the same one its frontend calls).
3. Mint a bearer token: `POST {base_url}/api/unlock` with the instance's
   unlock password, `"permission": "full"`, and an explicit
   `token_expiry_days`. Paste the resulting token into `admin_api.token`.

That's the *only* fixture this suite needs pre-provisioned. Every
cash_hub/circle_hub each test exercises is minted on demand through that
admin API and torn down again in its own `t.Cleanup` (see `admin_client.go`)
— there's no long-lived hub to hand-set-up first.

If `config.local.yaml` is missing, or `admin_api` is left blank, every test
skips cleanly (not a failure) — this suite is opt-in.

The token expires (30 days by default). An expired token makes tests fail
loudly, not skip — that's a config problem for the operator to refresh, not
a capability gap in cashctl.

## What it proves

Each test drives the real compiled binary as a subprocess (`binary.go`
builds it once, cached for the whole run) with its own fully isolated
`--config-dir` and `XDG_CONFIG_HOME`, so runs never interact with each other
or with a real user's own cashctl/ncli state:

- `TestCashLifecycle_MintReceiveRedeem` — mints a real cash token to a
  freshly generated local identity via a live cash_hub, `cashctl receive`s
  it (which always cross-checks against the Hub now), and `cashctl
  redeem`s it into a real invoice from the same hub. The full mint →
  receive → redeem round trip, proven live.
- `TestCashInspect_DecodeAndListRecipients` — mints a cash-mode token, then
  exercises `cashctl decode` (local-only) and `cashctl cash
  list-recipients` (network) against it.
- `TestCircleJoin_CreateWalletAndGetInfo` — provisions an allowlist-policy
  circle_hub, authorizes cashctl's local identity under it, joins via `cashctl
  join --hub <circlehub1...>` (the real bech32 Circle Hub connection format,
  end to end), and confirms the resulting personal wallet is live via
  `cashctl wallet get-info`.
- `TestReceive_PersistsMinterPubkeyForSignedToken` — mints with a mint
  signature requested and confirms `receive` persists the recovered minter
  pubkey onto the held entry (skips cleanly if the Hub doesn't attach one —
  minting a signature is best-effort server-side, not something a client
  can force).
- `TestRedeem_IntoRegisteredWallet` — exercises redeem's `--into` flag
  (renamed from `--to`) against a real registered wallet, end to end.
- `TestCashTransfer_TargetAutoDetection` — a bare hex pubkey and an
  `npub1...` target, both unprefixed, against a real transfer call.
- `TestCashTransfer_CashSelection_*` (`ExactMatch`/`BestFitSplit`/
  `Fragmented`/`AutoConsolidate`) — the four cash-selection cases
  (`internal/ledger.SelectForAmount`) against real held tokens: an exact
  match transfers whole, several covering tokens pick the smallest
  (best-fit), a shortfall refuses outright, and — the one needing a mint
  signature, same best-effort skip as above — two same-minter tokens that
  individually don't cover the amount get auto-consolidated first, verified
  both via the response and independently via `list_recipients` on the
  original (now-spent) connections. `AutoConsolidate` caught a real bug in
  `nmilat/nipcash/client.TransferFromSources` during development: its
  second wire call reused the client dialed against the *original*
  wallet, but `cash_consolidate` always spins off a genuinely new wallet
  pubkey, and `cash_transfer`'s proof-building binds to the dialed
  client's own wallet pubkey specifically — every auto-consolidate
  transfer failed with NWC `NOT_FOUND`, right after the (irreversible)
  consolidate step had already landed. Fixed by reconnecting to the newly
  consolidated wallet before the final transfer call.
- `TestWalletShow_NeverLeaksSecrets` / `TestDecode_NeverLeaksTokenSecret` —
  a held entry's real spending/dialing secrets (the token's own NWC
  pairing secret, and a cash-mode entry's `cash_secret`) must never
  appear in `wallet show`/`decode` output, checked against real, known
  values rather than assumed safe. Caught a real leak during development:
  `ledger.Entry` embeds directly into several commands' `--json` output
  (`wallet show`'s `held_tokens`, `receive`'s `entry`, `transfer`'s
  `remainder_entry`, `consolidate`'s `new_entry`), and its `Secret`/
  `CashSecret` fields had ordinary JSON tags — every one of those
  responses was echoing the real credential back in plaintext. Fixed by
  tagging both `json:"-"` on the struct itself, so no future command that
  happens to return a `ledger.Entry` can reintroduce the same leak.
- `TestCashTransfer_OverdraftAttempt` — asking to split off more than a
  held token's own amount: cashctl has no client-side check of this
  (`SplitAmount` is sent as given), so this proves the Hub's own
  rejection surfaces as a clean, classified error and the token is left
  completely untouched locally, not silently partially applied.
- `TestCashRedeem_AlreadyRedeemedTokenRejectedOnRetry` /
  `TestCashConsolidate_ReusingAlreadyConsolidatedSourceRejected` — drain a
  token for real, then try to reuse the same already-claimed local entry
  a second time (by explicit `--token`/`--sources` ID, bypassing
  `Held()`'s own exclusion of it). Proves the actual protection against
  reusing a spent slice lives server-side — cashctl's local `status`
  field is bookkeeping, not a security boundary — and that a rejected
  retry never corrupts the ledger (no duplicate history entry, no
  resurrected held token).
- `TestCashTransfer_ToCashTarget_SecretMustBeRecoverable` — transfers to
  `cash` and confirms the freshly-generated cash secret is
  actually recoverable from the response, by extracting it and redeeming
  with it for real. Caught a second real bug during development: the wire
  request only ever carries a one-way commitment of that secret
  (NIP-CASH §Cash-Mode Slices), and `ParseTarget` generated the real secret
  and then simply discarded it once the function returned — every
  `transfer cash` (and `consolidate --to cash`, sharing
  the same code path) was moving funds into a cash note nobody, not
  even the sender, could ever recover. Fixed by surfacing it through the
  same `Resolved`/`target_resolved` mechanism every other resolved
  identity already uses.
- `TestCashReceive_CashWithoutEmbeddedSecretDegradesToReadOnly` — a
  cash-mode token pasted with no embedded secret (there is no `--secret`
  flag) is never a usage error: it degrades to a read-only report and
  saves nothing, the same contract `decode --check` has.
- `TestCashReceive_AutoSecuresCashReceipt_NoOtherHoldings` /
  `_MergesWithExistingHolding` — receive's own automatic post-receive
  securing step (re-key a freshly-received cash-mode slice so the secret you
  were handed can no longer spend it; merge it with an existing
  same-minter holding if there is one), verified end to end against a
  real Hub: the original secret is confirmed dead by attempting to spend
  it directly afterward, and the merged case confirms both original
  sources show fully claimed via `list_recipients`, independent of
  cashctl's own local bookkeeping. The merge case currently fails against
  this project's own lab Hub (`BAD_REQUEST: new_identity.identity_type
  must be pubkey for cash_consolidate`) — that Hub's `cash_consolidate`
  doesn't yet accept a cash-mode target, a server-side deployment gap, not a
  cashctl/nmilat bug; left failing on purpose rather than skipped, since
  it's a real, currently-unmet prerequisite for that one path.
- `TestCashReceive_RapidSequentialReceivesPreserveOrder` — several real
  receives back to back (likely landing in the same wall-clock second)
  still list in `wallet show` in the order they were actually received —
  the live-Hub-round-trip counterpart to internal/ledger's own
  `TestSaveAndLoad_PreservesInsertionOrderForSameTimestamp`.

Everything else — the CLI's error contract, confirmation flows, flag
parsing, and cross-command Sniff-based routing — is covered by `internal/`'s
unit tests and the `agent-eval/` suite instead; this suite exists
specifically to prove the network-touching command bodies work against a
real server, not to re-litigate logic already covered elsewhere.
