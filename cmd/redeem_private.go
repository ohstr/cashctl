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
	// transportAuto uses the private transport for every hub that announces
	// one and falls back per bill for the rest. The default: a hub offering
	// the transport is the only evidence that it can serve it, and asking is
	// one cheap replaceable-event read.
	transportAuto transportMode = "auto"
	// transportPrivate refuses to fall back, so a test or an operator can be
	// certain which path was exercised. Without this, a silent fallback would
	// make a broken transport look like a working one.
	transportPrivate transportMode = "private"
	// transportStandard skips the private transport entirely — the escape
	// hatch for a money path, so a new wire format can always be taken out of
	// the loop without downgrading the SDK.
	transportStandard transportMode = "standard"
)

func parseTransportMode(cmd *cobra.Command) (transportMode, error) {
	raw, _ := cmd.Flags().GetString("transport")
	switch transportMode(raw) {
	case "", transportAuto:
		return transportAuto, nil
	case transportPrivate:
		return transportPrivate, nil
	case transportStandard:
		return transportStandard, nil
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
	order := make([]string, 0, len(plans))
	byHub := make(map[string][]redeemPlan, len(plans))
	var ungrouped []redeemPlan

	for _, p := range plans {
		if p.Entry.MinterPubkey == nil || *p.Entry.MinterPubkey == "" {
			ungrouped = append(ungrouped, p)
			continue
		}
		hub := *p.Entry.MinterPubkey
		if _, seen := byHub[hub]; !seen {
			order = append(order, hub)
		}
		byHub[hub] = append(byHub[hub], p)
	}

	groups := make([]hubGroup, 0, len(order))
	for _, hub := range order {
		groups = append(groups, hubGroup{HubXOnly: hub, Plans: byHub[hub]})
	}
	return groups, ungrouped
}

// planRelays collects the relay hints of a group's bills, in order and without
// duplicates.
//
// Every bill's hints are offered, not just the first's: the hints were fixed
// when each bill was minted, so an older bill may name a relay the hub has since
// left and a newer one may name where it went. The announcement's own relay list
// takes over once it is found (BatchSession.Refresh).
func planRelays(plans []redeemPlan) []string {
	var relays []string
	seen := map[string]struct{}{}
	for _, p := range plans {
		tok, err := nipcash.Decode(p.Entry.Token)
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

// redeemGroupPrivately spends a whole hub group in one relay event.
//
// Returns false when the group could not be served this way at all and the
// caller should fall back — which is safe only because every such refusal
// happens BEFORE anything is published: a missing announcement fails at session
// setup, and an unbatchable credential or an oversized item fails in packing,
// which runs to completion before the first envelope goes out. Once a request is
// on the wire this returns true and reports outcomes, never a fallback, since
// re-sending could double-spend.
func redeemGroupPrivately(
	ctx context.Context,
	cmd *cobra.Command,
	l *ledger.Ledger,
	g hubGroup,
	outcomes []redeemOutcome,
	destName string,
	jsonMode bool,
) (served bool, setupErr error) {
	relays := planRelays(g.Plans)
	if len(relays) == 0 {
		return false, fmt.Errorf("no relay hints on this hub's bills")
	}

	// The client is needed only to construct the session; the session does its
	// own relay I/O from the announcement afterwards, so this connection is not
	// held open for the batch.
	client, err := nipcashclient.Connect(ctx, g.Plans[0].Entry.Token)
	if err != nil {
		return false, err
	}
	defer client.Close()

	var session *nipcashclient.BatchSession
	err = WithSpinner(jsonMode, "Looking up the hub's batch inbox...", func() error {
		s, sErr := client.NewBatchSession(ctx, g.HubXOnly, relays)
		if sErr != nil {
			return sErr
		}
		session = s
		return nil
	})
	if err != nil {
		// ErrNoAnnouncement is the expected answer for a hub that simply does
		// not offer the transport — it is OPTIONAL in both directions — so it
		// is a fallback, not a fault.
		return false, err
	}

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
			recordRedeemed(l, plansEntry(g.Plans, r.ID), destName)
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
