package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// Bills are grouped by hub before being sent, because an envelope's items must all
// bind to the same hub and one cannot be split across two. That grouping stays here
// rather than in the SDK: only cashctl knows which bills it holds.
//
// Everything else about the transport is gone from this file. The SDK's Client now
// opens and reuses the session, finds and verifies the announcement, builds items and
// joins outcomes back to them — so there is no session, no fallback and no wire choice
// left to make here.

// hubGroup is the prepared bills of one hub, which can therefore share one request.
type hubGroup struct {
	// HubXOnly is the identity recovered from these bills' mint signatures — what the
	// hub's announcement is verified against. It must come from the bill rather than
	// from the announcement itself: an announcement checked against a key learned
	// from that same announcement would simply believe an attacker.
	HubXOnly string
	Plans    []redeemPlan
}

// entryHubGroup is hubGroup before the bills have been prepared.
type entryHubGroup struct {
	HubXOnly string
	Entries  []*ledger.Entry
}

// groupByHub splits items into per-hub groups, in first-appearance order, plus any
// with no usable hub identity.
//
// Generic because the split happens twice over the same rule — once over entries, once
// over prepared plans — and two copies would be two chances for those to disagree
// about which hub a bill belongs to.
func groupByHub[T any](items []T, hubOf func(T) *string) (order []string, byHub map[string][]T, ungrouped []T) {
	byHub = make(map[string][]T, len(items))
	for _, it := range items {
		hub := hubOf(it)
		if hub == nil || *hub == "" {
			ungrouped = append(ungrouped, it)
			continue
		}
		if _, seen := byHub[*hub]; !seen {
			order = append(order, *hub)
		}
		byHub[*hub] = append(byHub[*hub], it)
	}
	return order, byHub, ungrouped
}

// billHub reports which hub a bill belongs to, or nil when it has no recoverable
// minter.
//
// Every bill is mint-signed now, so nil means a bill that predates that or was issued
// by something that is not a conformant hub — either way it cannot be spent, because
// there is no hub identity to verify a transport announcement against and no other
// transport serves bill methods.
func billHub(e *ledger.Entry) *string {
	if e == nil || e.MinterPubkey == nil || *e.MinterPubkey == "" {
		return nil
	}
	return e.MinterPubkey
}

// groupEntriesByHub is groupByHub over selected bills, before preparation.
func groupEntriesByHub(entries []*ledger.Entry) ([]entryHubGroup, []*ledger.Entry) {
	order, byHub, ungrouped := groupByHub(entries, billHub)
	groups := make([]entryHubGroup, 0, len(order))
	for _, hub := range order {
		groups = append(groups, entryHubGroup{HubXOnly: hub, Entries: byHub[hub]})
	}
	return groups, ungrouped
}

// groupPlansByHub is groupByHub over prepared bills, at send time.
func groupPlansByHub(plans []redeemPlan) ([]hubGroup, []redeemPlan) {
	order, byHub, ungrouped := groupByHub(plans, func(p redeemPlan) *string { return billHub(p.Entry) })
	groups := make([]hubGroup, 0, len(order))
	for _, hub := range order {
		groups = append(groups, hubGroup{HubXOnly: hub, Plans: byHub[hub]})
	}
	return groups, ungrouped
}

// entryRelays collects the relay hints of a group's bills, in order, deduplicated.
//
// Every bill's hints are offered, not just the first's: hints were fixed when each
// bill was minted, so an older bill may name a relay the hub has since left and a
// newer one may name where it went.
func entryRelays(entries []*ledger.Entry) []string {
	var relays []string
	seen := map[string]struct{}{}
	for _, e := range entries {
		tok, err := nipcash.Decode(e.Token)
		if err != nil {
			continue
		}
		for _, r := range tok.RelayURLs {
			if r == "" {
				continue
			}
			if _, dup := seen[r]; dup {
				continue
			}
			seen[r] = struct{}{}
			relays = append(relays, r)
		}
	}
	return relays
}

// quoteHubGroup reads one hub group's fee quotes in a single request.
//
// cash_status is read-only, so unlike the spend nothing here could double-anything —
// but it is exactly as revealing per-bill on the wire, which is why it shares the
// request rather than going one at a time.
//
// Mirrors CheckClaim's semantics using the same exported MatchClaimAuto, so a quote
// means the same thing however it was fetched: the caller's own unclaimed row, matched
// by pubkey or by the bill being cash-mode, with the same ledger bookkeeping written
// from it.
func quoteHubGroup(
	ctx context.Context,
	cmd *cobra.Command,
	l *ledger.Ledger,
	g entryHubGroup,
	creds map[string]nipcash.Credential,
	jsonMode bool,
) (map[string]redeemQuote, map[string]error) {
	quotes := map[string]redeemQuote{}
	errs := map[string]error{}

	entries := make([]*ledger.Entry, 0, len(g.Entries))
	items := make([]nipcashclient.BatchStatus, 0, len(g.Entries))
	for _, e := range g.Entries {
		cred, ok := creds[e.ID]
		if !ok {
			// No credential: excluded BEFORE the request rather than sent and
			// omitted. An omission is information-free, so a bill dropped for a
			// reason already known locally would come back indistinguishable from
			// one the hub refused to serve.
			continue
		}
		tok, err := nipcash.Decode(e.Token)
		if err != nil {
			errs[e.ID] = output.RuntimeError(cmd, err)
			continue
		}
		entries = append(entries, e)
		// Both halves come from the decoded token, so BillFor is the only way to
		// supply them — the target and the secret that proves possession cannot be
		// set independently (nipcashclient.Bill).
		bill, err := nipcashclient.BillFor(tok)
		if err != nil {
			errs[e.ID] = output.RuntimeError(cmd, err)
			continue
		}
		items = append(items, nipcashclient.BatchStatus{
			ID: e.ID, Bill: bill, Credential: cred,
			// Explicit, though it is also the default: a redeem needs exactly one row
			// — the caller's own, for its fee quote — and saying so keeps the reply
			// small. A 100-recipient bill answers in ~300 bytes scoped, ~28,500 not.
			Scope: nipcash.ScopeMine,
		})
	}
	if len(items) == 0 {
		return quotes, errs
	}

	client, err := dialHubGroup(ctx, g.Entries)
	if err != nil {
		for _, it := range items {
			errs[it.ID] = output.NetworkError(cmd, err)
		}
		return quotes, errs
	}
	defer client.Close()

	var results []nipcashclient.StatusOutcome
	var sendErr error
	_ = WithSpinner(jsonMode, fmt.Sprintf("Checking %d fee quote(s) in one request...", len(items)), func() error {
		results, sendErr = client.StatusMany(ctx, items)
		return nil
	})
	if len(results) == 0 {
		if sendErr == nil {
			sendErr = errors.New("hub returned no answer")
		}
		for _, it := range items {
			errs[it.ID] = output.NetworkError(cmd, sendErr)
		}
		return quotes, errs
	}

	myPubHex, _ := localPubKeyHex(cmd)
	byID := map[string]*ledger.Entry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	for _, r := range results {
		entry := byID[r.ID]
		if entry == nil {
			continue
		}
		switch {
		case r.Succeeded() && r.Result != nil:
			recipient, ok := nipcash.MatchClaimAuto(r.Result.Recipients, myPubHex)
			if !ok {
				errs[r.ID] = output.NotFoundError(cmd, r.ID,
					fmt.Errorf("couldn't determine this token's amount — check `cashctl wallet show`, or that it's still valid"))
				continue
			}
			q, qErr := recordQuote(cmd, l, entry, recipient)
			if qErr != nil {
				errs[r.ID] = qErr
				continue
			}
			quotes[r.ID] = q
		case r.State == nipcashclient.OutcomeError && r.Error != nil:
			errs[r.ID] = output.NWCErrorForCashToken(cmd, &relayclient.WalletError{
				Method: "cash_status", Code: r.Error.Code, Message: r.Error.Message,
			})
		default:
			// Omitted. Harmless here in a way it is NOT for a spend: nothing was
			// executed, so this is simply "no quote".
			errs[r.ID] = output.NotFoundError(cmd, r.ID,
				fmt.Errorf("the hub returned no answer for this token — check `cashctl wallet show`, or that it's still valid"))
		}
	}
	return quotes, errs
}

// redeemHubGroup spends a whole hub group in one request.
func redeemHubGroup(
	ctx context.Context,
	cmd *cobra.Command,
	l *ledger.Ledger,
	g hubGroup,
	outcomes []redeemOutcome,
	destName string,
	jsonMode bool,
) {
	entries := make([]*ledger.Entry, 0, len(g.Plans))
	items := make([]nipcashclient.BatchRedeem, 0, len(g.Plans))
	for _, p := range g.Plans {
		tok, err := nipcash.Decode(p.Entry.Token)
		if err != nil {
			outcomes[p.Index].Err = output.RuntimeError(cmd, err)
			continue
		}
		entries = append(entries, p.Entry)
		bill, err := nipcashclient.BillFor(tok)
		if err != nil {
			outcomes[p.Index].Err = output.RuntimeError(cmd, err)
			continue
		}
		items = append(items, nipcashclient.BatchRedeem{
			// The ledger id, so every outcome joins straight back to its entry
			// without relying on ordering.
			ID:   p.Entry.ID,
			Bill: bill,
			// Amount nil for everything except an explicit, amountless
			// --invoice paired with --amount (invoiceAmountOverride) — the
			// one case where the invoice itself has nothing to encode and
			// the override is the only source of truth for how much to pay
			// out of this bill.
			Params: nipcash.CashRedeemParams{Invoice: p.Invoice, Credential: p.Cred, Amount: p.InvoiceAmount},
		})
	}
	if len(items) == 0 {
		return
	}

	client, err := dialHubGroup(ctx, entries)
	if err != nil {
		for _, it := range items {
			idx := outcomeIndex(outcomes, it.ID)
			if idx >= 0 {
				outcomes[idx].Err = output.NetworkError(cmd, err)
			}
		}
		return
	}
	defer client.Close()

	var results []nipcashclient.RedeemOutcome
	var sendErr error
	_ = WithSpinner(jsonMode, fmt.Sprintf("Redeeming %d token(s) in one request...", len(items)), func() error {
		results, sendErr = client.RedeemMany(ctx, items)
		return nil
	})
	if len(results) == 0 {
		if sendErr == nil {
			sendErr = errors.New("hub returned no outcomes")
		}
		for _, it := range items {
			idx := outcomeIndex(outcomes, it.ID)
			if idx >= 0 {
				outcomes[idx].Err = output.NetworkError(cmd, sendErr)
			}
		}
		return
	}
	results = retryWithPendingSecrets(g.Plans, items, results, func(retries []nipcashclient.BatchRedeem) []nipcashclient.RedeemOutcome {
		var out []nipcashclient.RedeemOutcome
		_ = WithSpinner(jsonMode, fmt.Sprintf("Retrying %d token(s) with a pending secret...", len(retries)), func() error {
			// A failed retry is not reported: the original decline is the one worth
			// showing, so the error is deliberately dropped here.
			out, _ = client.RedeemMany(ctx, retries)
			return nil
		})
		return out
	})
	applyRedeemResults(cmd, l, g.Plans, results, outcomes, destName)
}

// dialHubGroup opens one connection for a whole hub group.
//
// Any of the group's bills will do: they share a hub, and the SDK's Client recovers
// that hub's identity from whichever bill it was dialled with. The rest of the group's
// relay hints are still worth having, so the first bill that dials successfully wins.
func dialHubGroup(ctx context.Context, entries []*ledger.Entry) (*nipcashclient.Client, error) {
	var lastErr error
	for _, e := range entries {
		client, err := nipcashclient.Connect(ctx, e.Token)
		if err != nil {
			lastErr = err
			continue
		}
		return client, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no bills to dial")
	}
	return nil, lastErr
}

// outcomeIndex finds a bill's outcome slot by ledger id.
func outcomeIndex(outcomes []redeemOutcome, id string) int {
	for i := range outcomes {
		if outcomes[i].EntryID == id {
			return i
		}
	}
	return -1
}

// credentialForToken derives the credential a bill's own string implies.
//
// Needed because bill methods now authorize per ITEM: the connection no longer
// authorizes anything, so even a read like cash_status has to say who is asking. At
// receive time there is no ledger entry yet, so resolveCredential cannot help — the
// token string is all there is.
//
// An embedded secret means cash-mode and IS the authorization (NIP-CASH §Bearer
// Items). Otherwise the bill is identity-bound and the local identity is what proves
// control of it.
func credentialForToken(cmd *cobra.Command, embeddedSecret string) (nipcash.Credential, error) {
	if embeddedSecret != "" {
		return nipcash.BySecret(embeddedSecret), nil
	}
	return localCashCredential(cmd)
}

// recordQuote turns a matched roster row into a quote and writes the ledger
// bookkeeping resolveRedeemQuote does, so the batched and single-bill paths cannot
// drift in what a quote records.
//
// Returns an error rather than a bare quote because the Hub's figures are validated
// here (validateQuotedAmounts) and a refusal has to reach the caller: this used to
// persist whatever arrived and set Verified = true, so a hostile Hub — reachable via
// nothing more than a token's own RelayURLs — could write an arbitrary "verified"
// balance into the ledger. The attested amount comes from the TOKEN rather than from
// entry.AmountMillis, deliberately: the line below overwrites the entry's amount with
// the Hub's, so on a second attempt the entry no longer holds an independent figure to
// check against, while the token's is signed and immutable.
func recordQuote(cmd *cobra.Command, l *ledger.Ledger, entry *ledger.Entry, recipient nipcash.RecipientStatus) (redeemQuote, error) {
	q := redeemQuote{
		AmountMillis:        recipient.AmountMillis,
		RedeemFeeMillis:     recipient.RedeemFeeMillis,
		NetRedeemableMillis: recipient.NetRedeemableMillis,
		ExpiresAt:           recipient.ExpiresAt,
	}
	var tok nipcash.Token
	if decoded, err := nipcash.Decode(entry.Token); err == nil {
		tok = decoded
		if entry.MinterPubkey != nil {
			q.AttestedAmountMillis = decoded.AttestedAmountMillis
		}
	}
	// Before the ledger sees any of it, so a refusal leaves nothing behind.
	if err := validateQuotedAmounts(cmd, tok, entry.MinterPubkey, q); err != nil {
		return redeemQuote{}, err
	}
	entry.AmountMillis = &recipient.AmountMillis
	entry.ExpiresAt = recipient.ExpiresAt
	_ = l.SetVerified(entry.ID, true)
	return q, nil
}

// recordRedeemed is the local bookkeeping for one bill that really paid out.
func recordRedeemed(l *ledger.Ledger, entry *ledger.Entry, destName string) {
	if entry == nil {
		return
	}
	_ = l.SetStatus(entry.ID, ledger.StatusRedeemed)
	if entry.AmountMillis != nil {
		l.AppendHistory("redeem", fmt.Sprintf("redeemed %s into %s", output.FormatAmount(int64(*entry.AmountMillis)), destName))
	} else {
		l.AppendHistory("redeem", fmt.Sprintf("redeemed into %s", destName))
	}
}

// plansEntry finds a group's plan by ledger id.
// retryWithPendingSecrets is the pending-secret fallback, in the shape batching
// needs: per item, and as one extra request rather than one per bill.
//
// An interrupted protect step (cmd/receive_secure.go) can leave an entry holding
// TWO candidate cash secrets — the old one and the rekey's replacement — with no
// way to know which the hub accepted, because NIP-CASH has no read-only way to
// ask (nipcashclient.CheckClaim only proves *some* recipient exists, never
// *which* secret). The only place cashctl can learn the answer is a real spend,
// which is why this lives on the spend path rather than in a reconcile step
// nothing calls.
//
// spendCashEntry does exactly this for the single-call methods (transfer). Redeem
// stopped being a single call when it became a batch, and the fallback was simply
// left behind — a silent regression: a recovery that used to work returned
// NOT_FOUND instead, telling someone a bill they own cannot be spent.
//
// Only genuinely wrong-secret declines are retried. An omission is NOT retried
// and must not be: it is indistinguishable from a redemption whose reply was
// lost, so resending could double-spend.
// send is a parameter rather than a client so the decision logic — which items
// qualify, and what a failed retry must NOT do — is testable without a relay.
// Same shape as consolidateItemsFn; no package-level seam.
func retryWithPendingSecrets(
	plans []redeemPlan,
	items []nipcashclient.BatchRedeem,
	results []nipcashclient.RedeemOutcome,
	send func([]nipcashclient.BatchRedeem) []nipcashclient.RedeemOutcome,
) []nipcashclient.RedeemOutcome {
	itemByID := make(map[string]nipcashclient.BatchRedeem, len(items))
	for _, it := range items {
		itemByID[it.ID] = it
	}

	retries := make([]nipcashclient.BatchRedeem, 0, 1)
	pendingByID := make(map[string]string, 1)
	for _, r := range results {
		if r.State != nipcashclient.OutcomeError || r.Error == nil || r.Error.Code != "NOT_FOUND" {
			continue
		}
		entry := plansEntry(plans, r.ID)
		if entry == nil || entry.PendingCashSecret == "" || entry.PendingCashSecret == entry.CashSecret {
			continue
		}
		it, ok := itemByID[r.ID]
		if !ok {
			continue
		}
		it.Params.Credential = nipcash.BySecret(entry.PendingCashSecret)
		retries = append(retries, it)
		pendingByID[r.ID] = entry.PendingCashSecret
	}
	if len(retries) == 0 {
		return results
	}

	for _, rr := range send(retries) {
		if !rr.Succeeded() {
			// The FIRST decline is the one worth reporting — a second guess's own
			// failure adds nothing, and could itself just be the same wrong-secret
			// signal for a genuinely-wrong pending value.
			continue
		}
		entry := plansEntry(plans, rr.ID)
		if entry != nil {
			// Promote, so the next spend starts from the secret that actually works.
			// Not Saved here: the caller's own recordRedeemed does that, and picks
			// these fields up because Entry is a pointer into the ledger's slice —
			// the same contract spendCashEntry relies on.
			entry.CashSecret = pendingByID[rr.ID]
			entry.PendingCashSecret = ""
		}
		for i := range results {
			if results[i].ID == rr.ID {
				results[i] = rr
				break
			}
		}
	}
	return results
}

func plansEntry(plans []redeemPlan, id string) *ledger.Entry {
	for _, p := range plans {
		if p.Entry.ID == id {
			return p.Entry
		}
	}
	return nil
}

// applyRedeemResults maps a batch's per-item outcomes onto this run's outcome slots.
//
// Separated from the send so the mapping is testable without a relay: it is where the
// three states are decided, and two of them are easy to get subtly wrong in a way no
// compiler catches.
func applyRedeemResults(
	cmd *cobra.Command,
	l *ledger.Ledger,
	plans []redeemPlan,
	results []nipcashclient.RedeemOutcome,
	outcomes []redeemOutcome,
	destName string,
) {
	byID := make(map[string]int, len(outcomes))
	for i, o := range outcomes {
		byID[o.EntryID] = i
	}
	for _, r := range results {
		idx, ok := byID[r.ID]
		if !ok {
			continue
		}
		switch {
		case r.Succeeded() && r.Result != nil:
			outcomes[idx].Result = r.Result
			recordRedeemed(l, plansEntry(plans, r.ID), destName)
		case r.State == nipcashclient.OutcomeError && r.Error != nil:
			// A per-item error mirrors NIP-47's own error shape deliberately, so it
			// goes through the same classifier a single-bill decline does. The exit
			// code, retryable flag, cash-token wording and raw nwc_code are then
			// identical however many bills shared the request — a caller must not
			// have to know the batch size to read an error.
			outcomes[idx].Err = output.NWCErrorForCashToken(cmd, &relayclient.WalletError{
				Method:  "cash_redeem",
				Code:    r.Error.Code,
				Message: r.Error.Message,
			})
		default:
			// Not served: the hub said nothing. Deliberately information-free — it is
			// the same answer for a bill the hub does not hold, a proof that did not
			// verify, and a method it will not serve, because telling them apart
			// would make batching an oracle for which bills a hub holds. So it is
			// reported as something to RESOLVE BY ASKING, never as a silent success
			// and never as safe to resend: an omission is indistinguishable from a
			// redemption whose reply was lost.
			outcomes[idx].Err = output.ConflictError(cmd, r.ID, fmt.Errorf(
				"the hub returned no answer for this token — it may or may not have been redeemed; check with `cashctl cash list-recipients --token %s` before retrying", r.ID))
		}
	}
}
