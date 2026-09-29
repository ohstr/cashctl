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

// This file is redeem's private-transport path: several bills spent in ONE relay
// event instead of one event per bill.
//
// What it buys is not only fewer round trips. On the standard transport every
// request is p-tagged with a bill's own wallet pubkey, so a holder's bills are a
// public, linkable set — and redeeming forty of them publishes forty events
// within seconds of each other, tying them together for anyone watching the
// relay, without decrypting anything. Batched, that count and that timing
// correlation are gone.
//
// Scope, stated plainly: only the cash_redeem calls travel this way so far. The
// fee quote that precedes them is still one standard-transport cash_status per
// bill, each p-tagged with its own bill, so the linkability win is real but not
// yet complete — a watcher can still see the roster reads. Batching those is the
// obvious follow-up and is deliberately not bundled here, because it would mean
// rebuilding the prepare path that the single-bill money flow depends on.

// transportMode is which wire path a redeem should take.
type transportMode string

const (
	// transportAuto uses the private transport for every hub that announces one
	// and falls back per bill for the rest.
	//
	// NOT the default, deliberately. A bill derived by cashctl itself — a
	// consolidate's merged output, a split's remainder — INHERITS its sources'
	// minter (see ledger.Entry.MinterPubkey) rather than carrying a mint
	// signature of its own. Such a bill therefore looks batchable, and against a
	// live hub its items come back omitted: the envelope is unwrapped and the
	// item is then not served, for a reason not yet identified.
	//
	// Omission is information-free by design, so the caller cannot tell that from
	// a hub declining, and a redeem that reports "it may or may not have been
	// redeemed" is a bad outcome to hand someone by default. Until that case is
	// understood, routing real money over this path is opt-in.
	transportAuto transportMode = "auto"
	// transportPrivate refuses to fall back, so a test or an operator can be
	// certain which path was exercised. Without this, a silent fallback would
	// make a broken transport look like a working one.
	transportPrivate transportMode = "private"
	// transportStandard skips the private transport entirely: one request event
	// per bill, exactly as before this work. The DEFAULT, so nothing about an
	// existing redeem changes until a caller asks for the new path.
	transportStandard transportMode = "standard"
)

func parseTransportMode(cmd *cobra.Command) (transportMode, error) {
	raw, _ := cmd.Flags().GetString("transport")
	switch transportMode(raw) {
	case "", transportStandard:
		return transportStandard, nil
	case transportAuto:
		return transportAuto, nil
	case transportPrivate:
		return transportPrivate, nil
	default:
		return "", output.InvalidInputError(cmd, raw, fmt.Errorf("--transport must be auto, private or standard"))
	}
}

// hubGroup is the prepared bills of one hub, which can therefore share one
// request envelope.
type hubGroup struct {
	// HubXOnly is the identity recovered from these bills' mint signatures. It
	// is what the hub's announcement is verified against, which is the whole
	// reason it must come from the bill rather than from the announcement
	// itself: an announcement checked against a key learned from that same
	// announcement would just believe an attacker, and envelopes would be
	// encrypted to them.
	HubXOnly string
	Plans    []redeemPlan
}

// entryHubGroup is hubGroup before the bills have been prepared — the same
// split, made early enough to decide the wire path before any quoting happens.
type entryHubGroup struct {
	HubXOnly string
	Entries  []*ledger.Entry
}

// groupByHub splits items into per-hub groups, in first-appearance order, plus
// the ones with no usable hub identity.
//
// Generic because the split has to happen twice over the same rule: once over
// entries, to choose a wire path before quoting, and once over prepared plans,
// to send. Two copies of this rule would be two chances for the quote and the
// spend to disagree about which hub a bill belongs to.
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

// groupEntriesByHub is groupByHub over selected bills, before preparation.
func groupEntriesByHub(entries []*ledger.Entry) ([]entryHubGroup, []*ledger.Entry) {
	order, byHub, ungrouped := groupByHub(entries, func(e *ledger.Entry) *string { return e.MinterPubkey })
	groups := make([]entryHubGroup, 0, len(order))
	for _, hub := range order {
		groups = append(groups, entryHubGroup{HubXOnly: hub, Entries: byHub[hub]})
	}
	return groups, ungrouped
}

// groupPlansByHub splits prepared bills into per-hub groups plus the ones that
// cannot be batched at all.
//
// A bill is groupable only if its mint signature recovered a minter. That is not
// a trust decision — VerifyProvenance recovers WHICH key signed, never that the
// key is one to trust (see ledger.Entry.MinterPubkey) — but it is exactly the
// right value here, because addressing is all it is used for: only the real hub
// can sign the announcement this key is checked against, so a bill minted by hub
// H can only ever resolve to H's own inbox. A bill with no recoverable minter
// has no hub identity to check an announcement against and so cannot use the
// transport at all.
func groupPlansByHub(plans []redeemPlan) ([]hubGroup, []redeemPlan) {
	order, byHub, ungrouped := groupByHub(plans, func(p redeemPlan) *string { return p.Entry.MinterPubkey })
	groups := make([]hubGroup, 0, len(order))
	for _, hub := range order {
		groups = append(groups, hubGroup{HubXOnly: hub, Plans: byHub[hub]})
	}
	return groups, ungrouped
}

// entryRelays collects the relay hints of a group's bills, in order and without
// duplicates.
//
// Every bill's hints are offered, not just the first's: the hints were fixed
// when each bill was minted, so an older bill may name a relay the hub has since
// left and a newer one may name where it went. The announcement's own relay list
// takes over once it is found (BatchSession.Refresh).
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

// redeemSessions is one opened batch session per hub identity.
//
// Opened once per run and reused for BOTH the fee quote and the spend. That
// reuse is the point rather than an optimisation: a session opened only for the
// spend would leave the quotes on the standard transport, where each is tagged
// with its own bill's wallet pubkey — which republishes exactly the linkable set
// that batching the spend was meant to hide.
type redeemSessions map[string]*nipcashclient.BatchSession

// openRedeemSessions asks each hub whether it offers a batch inbox.
//
// Returns the sessions it could open plus, per hub, why it could not. A hub that
// publishes no announcement is the expected answer for one that simply does not
// offer the transport — it is OPTIONAL in both directions — so that is a reason
// to fall back, not a fault.
func openRedeemSessions(
	ctx context.Context,
	entries []*ledger.Entry,
	mode transportMode,
	jsonMode bool,
) (redeemSessions, map[string]error) {
	sessions := redeemSessions{}
	failures := map[string]error{}
	if mode == transportStandard {
		return sessions, failures
	}

	groups, _ := groupEntriesByHub(entries)
	for _, g := range groups {
		relays := entryRelays(g.Entries)
		if len(relays) == 0 {
			failures[g.HubXOnly] = errors.New("no relay hints on this hub's bills")
			continue
		}
		var session *nipcashclient.BatchSession
		err := WithSpinner(jsonMode, "Looking up the hub's batch inbox...", func() error {
			// The client is needed only to construct the session; the session
			// does its own relay I/O from the announcement afterwards, so this
			// connection is not held open for the batch.
			client, cErr := nipcashclient.Connect(ctx, g.Entries[0].Token)
			if cErr != nil {
				return cErr
			}
			defer client.Close()
			s, sErr := client.NewBatchSession(ctx, g.HubXOnly, relays)
			if sErr != nil {
				return sErr
			}
			session = s
			return nil
		})
		if err != nil {
			failures[g.HubXOnly] = err
			continue
		}
		sessions[g.HubXOnly] = session
	}
	return sessions, failures
}

// quoteGroupPrivately reads a whole hub group's fee quotes in ONE relay event.
//
// cash_status is read-only, so unlike the spend there is nothing here that could
// double-anything — but it is exactly as revealing on the standard transport,
// which is why it belongs on the same envelope as the spend it precedes.
//
// Mirrors CheckClaim's own semantics deliberately, using the same exported
// MatchClaimAuto, so a quote means the same thing whichever transport fetched
// it: the caller's own unclaimed row, matched by pubkey or by the bill being
// cash-mode, and the same ledger bookkeeping written from it.
func quoteGroupPrivately(
	ctx context.Context,
	cmd *cobra.Command,
	l *ledger.Ledger,
	session *nipcashclient.BatchSession,
	entries []*ledger.Entry,
	creds map[string]nipcash.Credential,
	jsonMode bool,
) (map[string]redeemQuote, map[string]error) {
	quotes := map[string]redeemQuote{}
	errs := map[string]error{}

	items := make([]nipcashclient.BatchStatus, 0, len(entries))
	for _, e := range entries {
		tok, err := nipcash.Decode(e.Token)
		if err != nil {
			errs[e.ID] = output.RuntimeError(cmd, err)
			continue
		}
		items = append(items, nipcashclient.BatchStatus{
			ID: e.ID, Target: tok.WalletPubkey, Credential: creds[e.ID],
			// Explicit, though it is also the private transport's default. A
			// redeem needs exactly one row — the caller's own, for its fee quote
			// — and saying so keeps the reply ~300 bytes instead of ~28,500 for a
			// 100-recipient bill. Batched, that difference is per bill, and an
			// unscoped batch is the common way to make a reply outgrow its
			// envelope and need chunking.
			Scope: nipcash.ScopeMine,
		})
	}
	if len(items) == 0 {
		return quotes, errs
	}

	var results []nipcashclient.StatusOutcome
	var sendErr error
	_ = WithSpinner(jsonMode, fmt.Sprintf("Checking %d fee quotes in one request...", len(items)), func() error {
		results, sendErr = session.StatusMany(ctx, items)
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
				// Same answer, and the same wording, CheckClaim's own
				// ErrClaimNotFound produces on the standard transport.
				errs[r.ID] = output.NotFoundError(cmd, r.ID,
					fmt.Errorf("couldn't determine this token's amount — check `cashctl wallet show`, or that it's still valid"))
				continue
			}
			quotes[r.ID] = recordQuote(l, entry, recipient)
		case r.State == nipcashclient.OutcomeError && r.Error != nil:
			errs[r.ID] = output.NWCErrorForCashToken(cmd, &relayclient.WalletError{
				Method: "cash_status", Code: r.Error.Code, Message: r.Error.Message,
			})
		default:
			// Omitted. Harmless here in a way it is NOT for a spend: nothing was
			// executed, so this is simply "no quote", and the bill is reported
			// as unquotable rather than redeemed into an unknown state.
			errs[r.ID] = output.NotFoundError(cmd, r.ID,
				fmt.Errorf("the hub returned no answer for this token — check `cashctl wallet show`, or that it's still valid"))
		}
	}
	return quotes, errs
}

// recordQuote turns a matched roster row into a quote and writes the same ledger
// bookkeeping resolveRedeemQuote does, so the two transports cannot drift.
func recordQuote(l *ledger.Ledger, entry *ledger.Entry, recipient nipcash.RecipientStatus) redeemQuote {
	entry.AmountMillis = &recipient.AmountMillis
	entry.ExpiresAt = recipient.ExpiresAt
	_ = l.SetVerified(entry.ID, true)
	return redeemQuote{
		AmountMillis:        recipient.AmountMillis,
		RedeemFeeMillis:     recipient.RedeemFeeMillis,
		NetRedeemableMillis: recipient.NetRedeemableMillis,
		ExpiresAt:           recipient.ExpiresAt,
	}
}

// redeemGroupPrivately spends a whole hub group in one relay event.
//
// Returns false when the group could not be served this way at all and the
// caller should fall back — which is safe only because every such refusal
// happens BEFORE anything is published: an unbatchable credential or an
// oversized item fails in packing, which runs to completion before the first
// envelope goes out. Once a request is on the wire this returns true and reports
// outcomes, never a fallback, since re-sending could double-spend.
func redeemGroupPrivately(
	ctx context.Context,
	cmd *cobra.Command,
	l *ledger.Ledger,
	session *nipcashclient.BatchSession,
	g hubGroup,
	outcomes []redeemOutcome,
	destName string,
	jsonMode bool,
) (served bool, setupErr error) {
	items := make([]nipcashclient.BatchRedeem, 0, len(g.Plans))
	for _, p := range g.Plans {
		tok, dErr := nipcash.Decode(p.Entry.Token)
		if dErr != nil {
			return false, dErr
		}
		items = append(items, nipcashclient.BatchRedeem{
			// The ledger id, so every outcome joins straight back to the entry
			// it belongs to without relying on ordering.
			ID:     p.Entry.ID,
			Target: tok.WalletPubkey,
			Params: nipcash.CashRedeemParams{Invoice: p.Invoice, Credential: p.Cred},
		})
	}

	var results []nipcashclient.RedeemOutcome
	var sendErr error
	_ = WithSpinner(jsonMode, fmt.Sprintf("Redeeming %d tokens in one request...", len(items)), func() error {
		results, sendErr = session.RedeemMany(ctx, items)
		return nil
	})

	// Nothing was published, so the caller may still try the standard path.
	// Distinguished from a partial reply by there being no outcomes at all:
	// RedeemMany returns outcomes AND an error when a reply arrived in fewer
	// chunks than the hub sent, and those outcomes are real answers.
	if len(results) == 0 {
		if sendErr == nil {
			sendErr = errors.New("hub returned no outcomes")
		}
		return false, sendErr
	}

	applyRedeemResults(cmd, l, g.Plans, results, outcomes, destName)
	return true, nil
}

// plansEntry finds a group's plan by ledger id.
func plansEntry(plans []redeemPlan, id string) *ledger.Entry {
	for _, p := range plans {
		if p.Entry.ID == id {
			return p.Entry
		}
	}
	return nil
}

// recordRedeemed is the local bookkeeping for one bill that really paid out,
// shared by both transports so they cannot drift.
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

// applyRedeemResults maps a batch's per-item outcomes onto this run's own outcome
// slots.
//
// Extracted from the send so the mapping is testable without a relay, because it
// is where the three states are decided and two of them are easy to get subtly
// wrong in a way no compiler catches.
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
			// A per-item error deliberately mirrors NIP-47's own error shape
			// ("so per-item failures read the same as a single-request failure
			// does today" — transport.ResultError), so it goes through the same
			// classifier a one-bill redeem's decline does. That keeps the exit
			// code, the retryable flag, the cash-token-specific wording and the
			// raw nwc_code identical whichever transport carried the refusal —
			// a caller must not have to branch on the wire path to read an
			// error.
			outcomes[idx].Err = output.NWCErrorForCashToken(cmd, &relayclient.WalletError{
				Method:  "cash_redeem",
				Code:    r.Error.Code,
				Message: r.Error.Message,
			})
		default:
			// Not served: the hub said nothing about this bill. Deliberately
			// information-free — it is the same answer for a bill the hub does
			// not hold, a proof that did not verify, and a method it will not
			// serve, because distinguishing them would turn batching into an
			// oracle for which bills a hub holds. So this is reported as a
			// failure the caller must resolve by ASKING, never as a silent
			// success and never as something safe to resend: an omission is
			// indistinguishable from a redemption whose reply was lost.
			outcomes[idx].Err = output.ConflictError(cmd, r.ID, fmt.Errorf(
				"the hub returned no answer for this token — it may or may not have been redeemed; check with `cashctl cash list-recipients --token %s` before retrying", r.ID))
		}
	}
}
