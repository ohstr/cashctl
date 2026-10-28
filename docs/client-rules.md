# Shared client rules

cashctl and cashkit follow the same rule wherever the NIPs leave a choice to the client. Where the NIPs
decide, the NIP wins. Each rule below says what both clients do, which section it rests on, and how cashctl
does it. cashkit documents the same rules in its `docs/design.md`.

| rule | both clients | spec |
|---|---|---|
| A1 Withdraw from a merge key | refused (`mid_merge`) | NIP-CASH §Redeeming On-chain; NIP-CP §Writing (Withdrawals) |
| A2 Re-mint already gone | closed, nothing added, the user told (`remint_gone`) | NIP-CASH §Redeeming On-chain |
| A3 Presumed-paid redeem | re-opened by a later read that shows the row unclaimed | NIP-CASH §Answer or Omit; NIP-CP §Balance |
| A4 Lost withdrawal | re-placed automatically on the owning device while its quote counts as valid | NIP-CP §Writing (the lookup rules); NIP-CASH §Lost Replies |
| A5 NIP-42 AUTH | answered only on the user's own pocket relays, as the user | NIP-CP §Wrapping |
| SG-1 Connection-key status | `NOT_FOUND` is never spent; a redeem or withdrawal is refused first (`status_unreadable`) | NIP-CASH §Scoping the Roster |

## A1. A bill on a merge key is never withdrawn

A merge moves cash notes onto a key made for that merge alone (a throwaway key). Both clients refuse to quote
or withdraw such a bill, with reason `mid_merge`. The bill goes back to a cash note first.

- Why: a withdrawal's lookups and its cancel request are authorized by the claim-proof signer until the payout
  is final or cancelled, and a cancelled payout's re-mint is delivered to that signer (NIP-CASH §Redeeming
  On-chain). A pocket entry has no slot for a merge key (NIP-CP §Writing), so another device could not follow
  the withdrawal. The NIPs do not require a client to offer every bill for withdrawal.
- cashctl: `cash withdraw quote` and `send` refuse it (`conflict`, `reason: mid_merge`). Every money command,
  `cash withdraw quote` and now `cash settle` first turn what a merge left on a throwaway key back into cash
  notes, so only a bill whose move is still open is refused.

## A2. A cancelled payout whose re-mint is already gone

A payout the Hub cancels re-mints the amount as a new bill. When the Hub's answer carries that bill as a
tombstone, both clients close the withdrawal, keep the source spent, add nothing, and tell the user once. It is
a notice, never a question.

- cashctl: a notice when it happens, a history line, and in `cash withdraw show` (`--json`) `remint_gone:
  true` with `remint_gone_reason`, the tombstone's reason.

## A3. A presumed-paid redeem is re-opened

A redeem whose row is still claimed 24 hours after its invoice expired is presumed paid (`redeemed`). Both
clients read it again on every later status refresh. A read that shows the row unclaimed makes the bill held
again; a tombstone makes the spend final.

- Why: the bill is still live while its row is unclaimed. A final state comes only from a definitive result,
  never from a timeout (NIP-CP §Balance).
- cashctl: `cash status` and `cash settle` re-read every bill with `RedeemPresumedAt` set.

## A4. A lost withdrawal is re-placed automatically

When a `cash_withdraw` got no answer and its lookup answers `unknown`, the device that placed it places it
again, under the derived id, with the same quote, `notify` and re-mint secret, at most 3 times. Neither client
has its own view of the chain, so a quote counts as valid until a call of this withdrawal was refused
`quote_expired` or `quote_invalid`.

| case | what both clients do |
|---|---|
| lost reply, omission, crash before publish, then `unknown` | re-placed automatically |
| a call of this withdrawal refused `quote_expired`/`quote_invalid` | never re-sent; a new quote needs the user |
| any other coded refusal, then `unknown` | closed not applied, with its code |
| `REQUEST_REUSED`, then `unknown` | re-placed only after the user confirms (default no) |
| a withdrawal recovered from `withdraw_request_id` | never re-placed |
| another device's call | shown, never re-placed |

- Why: the lookup rules of NIP-CP §Writing let a wallet place a `cash_withdraw` again automatically only when it
  got no answer and the lookup answered `unknown`, while its quote is valid; a wallet without its own view of
  the chain treats the quote as valid until it is refused `quote_expired`. A mint or an order is still
  re-placed only after the user confirms.
- cashctl: `cash settle` re-places it; a refused quote leaves it `stuck`/`quote_expired`; `REQUEST_REUSED`
  leaves it `stuck`/`confirm_replace` for `cash withdraw settle --replace <w> --yes`. With nmilat's
  `ReplaceOptions.NoChainView` (after the move off nmilat `spec-sync/parity-1`), cashctl passes `NoChainView:
  true` for this check.

## A5. NIP-42 AUTH

A client answers a relay's NIP-42 AUTH only on the user's own pocket relays (their kind-10050 relays, else their
NIP-65 write relays), only as the user, and only after a request was refused `auth-required:`. That
authenticated connection carries pocket traffic only. A Hub, circle, order, listing or discovery relay is never
answered.

- Why: those relays SHOULD serve a gift wrap only to its `p` (NIP-CP §Wrapping). Authenticating anywhere else would
  link the user's identity to their Hub traffic.
- cashctl: it keeps no Cash Pocket, so it never answers AUTH. A relay that requires AUTH to read is treated as one
  that does not answer.

## SG-1. A connection-key holder's own row

A Hub answers a connection-key holder's `cash_status` with `NOT_FOUND` (NIP-CASH §Scoping the Roster). Both
clients read that as "status not readable": never spent, never "not yours".

- A redeem or a withdrawal is decided by reading that row, so both clients refuse one first, with reason
  `status_unreadable`. Nothing is sent, and the bill can still be transferred.
- cashctl: the auto-invoice and `--invoice` redeems and the withdraw quote all read the row first. A bill
  already `redeeming` whose row can't be read says why in `cash settle` (`status_unreadable`, or the credential
  it needs, given through `CASHCTL_AS`).
