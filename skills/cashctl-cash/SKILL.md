---
name: cashctl-cash
description: Receive a NIP-CASH token into your local wallet (`cashctl receive`), redeem a held token into a Lightning wallet or raw invoice (`cashctl redeem`), send a held token to someone else (`cashctl transfer`), merge several held tokens into one (`cashctl consolidate`), and check its Hub-side recipients (`cashctl cash status`). For local-only inspection of a token string itself (no network call, including mint-signature verification), see `cashctl decode` in `skills/cashctl-wallet/SKILL.md`. Use whenever an agent is handed a lokicash1... (or other cash-token-family) string, needs to cash it out to Lightning, needs to forward it to another identity, or needs to combine multiple small tokens.
license: Unlicense
---

<!-- Mirrors ohstr/cashctl's cmd/cash_receive.go, cmd/cash_redeem.go,
cmd/cash_transfer.go, cmd/cash_consolidate.go, and cmd/cash_inspect.go
(cash status) as of writing. See skills/cashctl-wallet/SKILL.md for
cmd/decode.go. Self-contained by design — update by hand if flags/schemas
change. -->

# cashctl receive / redeem / transfer / consolidate / cash ...

## `cashctl receive <token>` — cash it in

```sh
cashctl receive lokicash1... --json                            # prints the token's details, then verifies against the Cash Hub
cashctl receive lokicash1...#deadbeef --json                   # cash-mode: the combined "<token>#<cash_secret>" presentation
```

Always a network call: receive prints the token's details, then
cross-checks it against the Cash Hub (the same `list_recipients` call
`cashctl cash status` makes) before saving anything. Anything
that doesn't check out — no matching recipient on the Cash Hub, or the
Cash Hub can't be reached at all — is refused outright; nothing gets
added to your wallet. Pasting a Circle Hub (`circlehub1...`) or Cash Hub
(`cashhub1...`) connection here instead of a token gets a specific
corrective error (`code: "invalid_input"`) naming the right command
(`cashctl join ...`, or "cashctl can't mint").

**A cash-mode token (`identity_required: false`) is two values, not
one.** The token string only lets you dial the wallet — it is *never* a
valid spending credential by itself, no matter how it looks. The real
credential, `cash_secret`, is a separate value the Hub operator hands
out once, alongside the token, at mint time, and it MUST arrive embedded
in the token itself: `<token>#<cash_secret>` (NIP-CASH's combined
cash-mode slice presentation — `#` never appears in a token's own bech32
charset, so splitting it back apart is always unambiguous). There is no
`--secret` flag: a cash-mode token pasted without the embedded secret has
nothing `receive` can act on, so it degrades to a read-only report
instead of erroring — `{"received": false, "reason":
"no_embedded_secret"}`, exit 0, nothing saved. `cashctl decode` on the
combined presentation reports `embedded_cash_secret_present: true`
(never the secret's value) if you want to check which form you have
before receiving.

**A saved cash-mode receipt is then offered automatic protecting.**
Anyone who saw the same cash secret before it reached you — the
sender, or anyone the sender showed it to — could still spend it too,
for as long as it stays shared. Once `receive` saves a cash-mode
entry, it asks to protect it immediately (defaults to yes; always
proceeds non-interactively under `--yes`/`--json` — there's no separate
flag to opt out). Protecting re-keys the slice under a fresh secret only
your wallet knows, and — if you already hold other cash mint-signed by
the same issuer — merges it into that holding in the same step. A
same-Hub holding that's already expired is left out of that merge
instead of included: NIP-CASH's merge rule inherits the earliest expiry
across every source, so folding a dead one in would kill the fresh
receipt's own good deadline too. The fresh receipt still gets protected
either way — just re-keyed alone (`"rekeyed"`) instead of merged
(`"consolidated"`) whenever the only other same-Hub holding was the one
excluded. There's no `--json` field naming what got left out (the
in-process note is text-mode only); `secured.status` not matching what
you expected given the holdings on record is the signal. Reported
under `"secured"` in `--json` output: `{"status": "rekeyed"}` (re-keyed
in place), `{"status": "consolidated", "consolidated_with": [...],
"final_entry_id": "..."}` (merged into a new entry), `{"status":
"declined"}`, or `{"status": "not_applicable"}` (not a cash-mode
receive). A wire failure here reports `{"status": "failed", "error":
"..."}` but never fails `receive` itself — the cash is already genuinely
yours; retry protecting later with `cashctl wallet protect [id]` (or
`--token <id>`, same effect — re-keys a single still-shared holding in
place; `consolidate --to cash` needs 2+ sources). With no id and more than
one still-shared holding, re-keys **every** one of them instead of asking
which — unlike `redeem`, nothing here leaves your control, so an
ambiguous selection has nothing to lose, and `--json`/`--yes` with several
eligible returns an array (`{"protected": [...]}`, each row carrying
`id`) rather than refusing as ambiguous. Because the secret is always captured — and kept current
— up front, `redeem`/`transfer` never *need* a `--as cash:<secret>`
override for a held token's own call — the one real use is overriding
the entry's on-disk secret with a fresher one you learned out-of-band
(e.g. a `recovery` handoff for this exact entry, below) without editing
the ledger first: `cashctl redeem --token tok-a1b2 --as cash:<secret> --json`.

**`retryable`/`recovery` in practice.** Every error from `redeem`,
`transfer`, and `consolidate` carries `retryable` (branch on it instead of
the message: `true` means back off and retry, `false` means fix the input
first) — this is the same top-level `{"error", "code", "retryable", ...}`
shape AGENTS.md's error table describes for every command, not something
special to this skill's own three. `recovery` is the one field exempt from
secret redaction and never truncated, and it appears only when a Hub-side
mutation is *confirmed* to have happened but the local save that was
supposed to record it then failed — the money moved, your wallet just
doesn't know it yet. For `consolidate`, that's the merged or
sent-to-cash result's own `<token>#<cash_secret>` — the only copy that
will ever be shown:

```json
{
  "error": "Consolidate succeeded on the Hub, but saving that locally failed (disk full) — your wallet's local record does not match reality.",
  "code": "internal",
  "retryable": false,
  "recovery": "The resulting token — save this now, it will not be shown again: lokicash1...#a1b2c3...(64 hex)"
}
```

`recovery` is free text, not a fixed shape — it can name more than one
handoff, so match what's actually inside it rather than expecting one
bare value. `transfer`'s own version is the clearest case: a split send
can fail to save with BOTH the recipient's bill and your own remainder
still unrecorded, and both get named in one string ("Save these: the
recipient's own — save this now, it will not be shown again: ...; your
own remainder: ..."). `redeem`'s differs in shape entirely, since a
payout has no secret to hand back: a `token=preimage` list per entry that
paid, naming exactly which already-held entries will now incorrectly
keep showing as held until reconciled. Either way: treat a nonzero exit
carrying `recovery` as "money moved, ledger didn't" — never as "nothing
happened, retry freely".

## `cashctl redeem` — cash it out

```sh
cashctl redeem --json                              # auto-picks your one held token + default wallet
cashctl redeem work --token tok-a1b2 --json         # positional wallet name — or --into work
cashctl redeem --token tok-a1b2,tok-c3d4 --json     # several at once — repeatable, or comma-separated
cashctl redeem --all --json                        # every held token
cashctl redeem --amount 300 --json                 # selects which held token(s) land exactly 300, net of any fee
cashctl redeem --invoice lnbc1... --json           # bypasses both — any invoice, no cashctl wallet needed
```

**Several tokens in one call.** `--token` is repeatable (and accepts a
comma-separated list); `--all` selects every held token. `--token` and
`--all` together is a usage error, and naming the same token twice is too
(the second attempt could only fail, and would fail as a misleading
network timeout, because a Hub deletes a spent slice and then stays silent
about it).

**`--amount`** selects which held token(s) to use, so there's no `--token`
id to look up first: `redeem --amount 300` lands exactly 300 at the
destination, after whatever fee the Hub quotes — an exact single-token net
match, or an exact same-Hub net sum if no single token covers it alone
(same reasoning as `transfer --amount`'s own coin-selection, but matched
against each candidate's live net-redeemable quote, not its cached face
value — a nonzero fee makes those different numbers). No combination found
is a plain refusal naming what's held and what was asked for, not a guess:
redeeming part of a token that doesn't already exist to hit an odd amount
is **not attempted** — it would need a second live measurement to do
exactly (the fee rate isn't exposed on the wire, only its effect on one
already-existing amount), and that's a deliberately separate piece of work,
not folded in here. `--amount` and `--token`/`--all` are mutually
exclusive — pick one.

With `--invoice`, `--amount` means something different: the invoice
usually fixes its own amount, so pairing one that does with `--amount` is
a usage error (same as pasting a fixed-amount invoice into any Lightning
wallet — the amount field is locked). An amount-less invoice has no figure
of its own, so there `--amount` is required, not just accepted, and is
checked against the bill's own live quote before anything is sent.

Each token is paid out into **its own invoice**: a slice pays out exactly
once and an invoice is payable once, so N tokens need N invoices, all
drawn from the one destination wallet. `--invoice` therefore accepts only
one token — combining it with several is a usage error rather than a
partial redemption.

Every selected token is attempted, and one failing never stops the others:
each payout is separate and irreversible, so aborting part-way through
would leave the caller unable to tell which tokens already paid. Under
`--json` a multi-token run returns `{"redeemed": [...], "to_wallet"}` (or
`to_invoice`), one entry per token with `token`, `amount_millis`, and a
`status` of:

| `status` | meaning |
|---|---|
| `ok` | paid out; entry also carries `fee_mloki` and `preimage` |
| `failed` | entry also carries `error`, `code`, and `nwc_code` when the wallet returned one |
| `not_attempted` | the run was declined before this token was reached — no wire call was made, so there is nothing to retry or reconcile |

A single-token run keeps the original top-level
`{"redeemed_token", "fee_mloki", "preimage", "to_wallet"|"to_invoice"}`
shape, and a single token that *failed* still prints nothing on stdout —
its error on stderr says everything. As with `consolidate`, **a partially
successful run prints its full result on stdout AND exits nonzero**, so a
nonzero exit does not mean nothing was paid: read stdout before retrying
anything. A `preimage` is the only proof a given payout happened, so if
recording the run locally fails afterwards, every preimage is named in the
error message for reconciliation.

**Wire path.** There is nothing to choose. `redeem`, `transfer`,
`consolidate` and `cash_status` are served over the private transport only,
and cashctl batches automatically: every token redeemed against the same hub
in one run travels in one relay event, whatever the count. No flag, no
fallback, no per-token path to opt into.

Batching is why the transport exists, and it is a privacy property rather
than a round-trip saving. A per-token request is tagged with that token's own
wallet pubkey, so redeeming forty tokens that way publishes forty events
seconds apart and ties them together for anyone watching the relay. Both
halves of a redeem share one session per hub — the fee quote (`cash_status`)
and the spend (`cash_redeem`) — so neither republishes the set the other is
hiding.

This also means an unsigned bill cannot be spent at all: the mint signature
is the only thing a token carries that identifies its minting hub, and so the
only thing a hub's announcement can be verified against. Hubs now sign every
mint with their Lightning identity, and a mint that cannot be signed fails
outright rather than producing a bill nobody can redeem.

A batched token whose hub returns **no answer** is reported as `failed`
with code `conflict`, and that case needs care: an omission is deliberately
information-free (it is the same answer for a token the hub does not hold,
a proof that did not verify, and a method it will not serve — telling them
apart would make batching an oracle for which tokens a hub holds), so it is
indistinguishable from a redemption whose reply was lost. Never retry it
blind; check with `cashctl cash status --token <id>` first.

If you hold more than one token and none of
`--token`/`--all`/`--json`/`--yes` is given, `redeem` (and
`transfer`/`consolidate` below) prompts interactively with a numbered list
instead of failing outright — accepting a comma-separated selection or
`all`, though a bare Enter is **not** "all" here, since redeeming pays out
irreversibly and the cheap default must be the one that spends nothing.
Under `--json`/`--yes`/a non-interactive script, `--token <id>`, `--all`
or `--amount` is still required (`cashctl wallet show` lists held-token
IDs), since there's no terminal to prompt from — note this is deliberately stricter
than `consolidate`, which does process every group non-interactively,
because consolidating keeps the value yours and redeeming does not. A
**connection-key-bound** token needs an explicit `--as
connection-key:<privkey>,<platform>,<external-id>,<attestation-file>` —
cashctl does not auto-refresh a connection-key credential from a relay
(a deliberate scope limit: re-deriving a fresh live attestation isn't
automatic), so the error names exactly what to pass.

Pass that credential through `CASHCTL_AS` rather than `--as` in anything
scripted. `--as` is always secret-bearing, and a value on the command line
is visible to `ps` for as long as the process runs and lands in the shell's
history file; the env var keeps it out of both. `--as` still wins when both
are set, so a one-off override works as before. Not airtight — an
environment variable is inherited by child processes and readable from
`/proc/<pid>/environ` by the same user — but strictly better than argv, and
argv is the one exposure a process cannot do anything about from the inside.

In an interactive (non-`--json`/`--yes`) session, the confirmation prompt
shows the expected fee (only if non-zero) and warns if the token's own
redemption deadline is close or already passed, and — like `transfer`/
`consolidate` — defaults to **no** on a bare Enter. Those two previews are
shown for a single-token redeem only: with several tokens, one fee line
per token would bury the total the prompt exists to have you check, so the
prompt names the token count and the total instead and the per-token fee
lands in the result. None of this affects
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
cashctl transfer cash --json
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

If that auto-consolidate subset would mix an already-expired source with
a healthy one, the call refuses outright instead of merging them first:
`code: "invalid_input"`, naming the expired source's own ID, nothing
attempted — same reasoning as `consolidate`'s own refusal below (NIP-CASH's
merge rule inherits the earliest expiry across every source, so merging a
dead one in kills the healthy source's deadline too). An all-expired
subset is unaffected: that still reaches the Hub and comes back
`code: "auth"`/`nwc_code: "EXPIRED"` as always, since there's no healthy
deadline left to protect.

## `cashctl consolidate` — merge several into one

```sh
cashctl consolidate --json                                    # no sources given: auto-groups held tokens by Cash Hub, merges each group
cashctl consolidate tok-a1b2 tok-c3d4 --json                   # positional IDs — or --sources tok-a1b2,tok-c3d4
cashctl consolidate --sources tok-a1b2,lokicash1...:5:pubkey:<privkey> --to pubkey:<hex> --json
cashctl consolidate --sources tok-a1b2,tok-c3d4 --to nconnection1... --ia ia@example.com --json
```

Positional IDs, or `--sources` comma-separated: a bare ID already in your
ledger (amount/credential resolved automatically), or the verbose
`<token>:<amount-loki>:<credential>` form (`--sources` only) for a source that
isn't held locally — only `pubkey:`/`cash:` credentials work in that
verbose form (a `connection-key:` value has its own embedded commas,
ambiguous in this shorthand — receive it into your ledger first instead).
IDs are discoverable via `cashctl wallet show --json`'s `held_tokens`
array — plain-text `wallet show` deliberately never prints them.
Needs at least 2 sources per call. Like `transfer`, an `nconnection1...`
`--to` target needs `--ia <identity>` (hex pubkey or NIP-05) to resolve
its Identity Authority — an interactive session prompts for it if
missing, a `--json`/`--yes` call gets `code: "usage"` naming `--ia`.

With neither positional IDs nor `--sources` given: only same-Hub
tokens can actually be merged, so cashctl groups held tokens by Cash Hub
and consolidates each group with 2+ tokens (a lone token from a Hub
needs no merge, and is skipped) — under `--json`, every qualifying group
is processed with no prompt.

**A selection — explicit or auto-grouped — that mixes an already-expired
source with a healthy one is refused, not merged.** NIP-CASH's merge rule
inherits the earliest expiry across every source, so merging a dead one
in would kill the healthy source's own good deadline too the instant the
merge lands — cashctl checks every source's expiry live before attempting
anything and refuses outright on a mix: `code: "invalid_input"`, naming
the expired source(s), nothing attempted, even under `--json`/`--yes`.
For an explicit `--sources`/positional call this is the whole command's
error. For the no-args auto-grouped path, it's that one Hub group's own
`"failed"` entry in `{"consolidated": [...]}` (same shape as any other
group failure) — sibling groups are unaffected — unless it's the only
group, in which case it's the top-level error the same way. An
*all*-expired selection is unaffected either way: that still reaches the
Hub and comes back `code: "auth"`/`nwc_code: "EXPIRED"`, since there's no
healthy deadline left to protect.

Every chosen group is attempted, and one group failing never stops the
others: each group is its own separately committed `cash_consolidate`
call, so aborting on the first failure would leave earlier groups already
merged on the Hub and never report the tokens they produced.

Response shape depends on how many groups were processed and whether all
of them succeeded. Exactly one, succeeded (the common case) returns the
same `{"new_entry", "expires_at", "target_resolved"}` object as the
explicit-sources form always has. Otherwise you get
`{"consolidated": [...]}`, one entry per group, each with `hub`,
`sources` (the ledger IDs it tried), and a `status` of:

| `status` | meaning |
|---|---|
| `ok` | merged; entry also carries `new_entry`, `expires_at`, `target_resolved` |
| `declined` | a person said no at the prompt — a choice, not a fault, and never an exit-code failure |
| `failed` | entry also carries `error`, `code`, and `nwc_code` when the wallet returned one — same vocabulary as a top-level error |

Check which key is present rather than assuming one shape. **A partially
successful run prints its full result on stdout AND exits nonzero**, with
the first failing group's own `code` driving the exit status — so a
nonzero exit here does not mean nothing happened, and stdout must be read
before deciding what to retry. `declined` groups alone never make the
exit nonzero.
`--to` defaults to your own identity; `--to cash` merges into a
fresh, anonymous cash note instead (same keyword `transfer` uses) —
requires a Hub that accepts a cash-mode `cash_consolidate` target.

## `cashctl cash status`

Renamed from `cash list-recipients`, which still works as an alias — the command
calls `cash_status` and nothing else, so the old name described a method that no
longer exists.

```sh
cashctl cash status --token tok-a1b2 --json  # network call — your allocation + co-recipients
cashctl cash status --json                   # with no id and several held, reports every one
```

With no id and more than one held token, reports every one of them in a
`{"statuses": [...]}` array instead of refusing — this is pure read
access, so it's even less contentious than `wallet protect`'s own version
of the same rule above: nothing moves at all, let alone leaves your
control.

For mint-signature verification or a plain field dump of a token
(`wallet_pubkey`, `relays`, `mint_signature_valid`/`minter_pubkey` if
present, ...), use the general-purpose `cashctl decode lokicash1...`
instead (see `skills/cashctl-wallet/SKILL.md`) — it never touches the
network, works on any token string held or not, and is the cheapest way to
inspect one before deciding whether to `receive` it at all.

## End-to-end scenarios

Each command above in isolation; here's how they chain in practice.

**Receive a gift, it auto-merges, redeem the result directly** — no need
to look anything up between the two calls, `receive`'s own response names
the entry to act on next:

```sh
cashctl receive lokicash1...#deadbeef --json
# {"entry": {"id": "tok-new456", ...}, "secured": {"status": "consolidated", "final_entry_id": "tok-new456", "consolidated_with": ["tok-existing999"]}}
cashctl redeem --token tok-new456 --json
```

**A transfer amount no single token covers, auto-consolidate fires, check
what's left** — one command does both chained calls; the aftermath shows
up in the next `wallet show`:

```sh
cashctl wallet balance --json                              # see what's held before
cashctl transfer --to npub1w0lxfr9... --amount 12 --json    # consolidates a covering subset first, then sends
cashctl wallet show --json                                  # held_tokens' new entry, if the send wasn't exact
```

**One of several named sources turns out to be expired — refused, retry
without it:**

```sh
cashctl consolidate tok-a1b2 tok-c3d4 tok-e5f6 --json
# {"error": "tok-a1b2 already expired — merging with a healthy source would make the whole result unusable too. Nothing was changed. Drop it and retry with just the healthy source(s)", "code": "invalid_input", "retryable": false, "input": "tok-a1b2"}
cashctl consolidate tok-c3d4 tok-e5f6 --json                # drop the named source, retry with the healthy two
```
