package cmd

import (
	"fmt"

	"github.com/ohstr/nmilat/nipcash"

	"github.com/ohstr/cashctl/internal/ledger"
)

// parkedCashSecret is a destination cash secret written to disk ahead of the
// wire call that commits it, plus enough to find its row again. The zero value
// means "nothing was parked" — the honest state for a non-cash target, where
// the wire reply carries everything needed and there is no local-only secret
// to lose — and every method on it is a no-op in that state, so a caller never
// has to branch on the target kind twice.
//
// Holds the row's ID and the ledger, NOT a *ledger.Entry. A pointer from
// l.Find is into l.Entries' own backing array, and every one of these callers
// does an l.Add between parking and releasing — the merged/remainder entry —
// which appends and so can reallocate that array, leaving the pointer aimed at
// the old copy. The release then silently cleared nothing and the park
// survived a successful send, which is how this was found: the regression test
// below asserts the release, not just the write. Same hazard resolveLiveEntry
// documents for SelectionPlan.Entry, reached a different way.
type parkedCashSecret struct {
	ledger  *ledger.Ledger
	entryID string
	secret  string
}

// parkDestinationCashSecret persists target's secret against the first of
// sourceIDs that is actually in l, and Saves, BEFORE the caller places the
// wire call that creates the destination bill.
//
// Why this exists at all: a cash-mode target's secret is generated entirely
// client-side and only a one-way commitment of it ever crosses the wire
// (NIP-CASH §Cash-Mode Slices — the Hub never learns or returns the
// preimage). So from the moment credential.ParseTarget("cash") mints it to
// the moment ledger.Save() lands, it exists in exactly one place: this
// process's heap. A kill in that window after the Hub has committed leaves a
// bill that is real, funded and permanently unspendable, with no ledger row
// for anything to reconcile against and no protocol-level way to recover the
// preimage. cashctl registers no SIGINT handler, so Ctrl-C during the
// "Transferring…"/"Consolidating…" spinner is exactly that kill — which makes
// this an ordinary user action rather than a rare crash. Audit finding
// D-CLI-1.
//
// This is the same write-ahead discipline protectCashReceipt and
// protectRekeyOnly already use, and the same argument their own doc comments
// make: "there is no reason to let placing the call be the difference between
// 'the new secret exists on disk' and 'the new secret exists only in this
// process's own memory, gone the instant it dies'."
//
// A non-cash target returns the zero parkedCashSecret and writes nothing:
// for a pubkey/connection-key destination the Hub's reply is sufficient to
// reach the funds, so there is no local-only value at risk.
//
// A cash target with no resolvable source row is an ERROR, not a silent
// proceed. It is not reachable through today's commands — every caller
// resolves its sources from the ledger before getting here — but proceeding
// would place the exact call whose interruption destroys money, with the
// safety net absent, which is the whole defect being fixed. Refusing costs a
// retry; proceeding can cost the funds. Guarded the same defensive way
// doCashConsolidate's own empty-dial-candidates case is.
func parkDestinationCashSecret(l *ledger.Ledger, sourceIDs []string, target nipcash.Target) (parkedCashSecret, error) {
	bt, ok := target.(*nipcash.CashTarget)
	if !ok {
		return parkedCashSecret{}, nil
	}
	secret := bt.Secret()
	if secret == "" {
		// A CashTarget with no secret cannot be spent by anyone whatever
		// happens next, so there is nothing to protect and nothing that
		// writing an empty string would protect it from. Reported rather
		// than parked: it means the target was built by something other
		// than nipcash.NewCashTarget, which is a programming error.
		return parkedCashSecret{}, fmt.Errorf("internal: cash-mode target carries no secret, so its destination bill would be unspendable regardless")
	}
	for _, id := range sourceIDs {
		e, found := l.Find(id)
		if !found {
			continue
		}
		e.PendingDestinationCashSecret = secret
		if err := l.Save(); err != nil {
			// Nothing has reached the Hub yet, so this is an ordinary
			// pre-flight failure: roll the field back and let the caller
			// refuse. The alternative — placing the call anyway — is
			// precisely the unprotected window.
			e.PendingDestinationCashSecret = ""
			return parkedCashSecret{}, fmt.Errorf("could not record the destination cash secret before placing the call, so the call was not placed (your funds have not moved): %w", err)
		}
		return parkedCashSecret{ledger: l, entryID: id, secret: secret}, nil
	}
	return parkedCashSecret{}, fmt.Errorf("internal: no ledger row among %v to record the destination cash secret against, so the call was not placed (your funds have not moved)", sourceIDs)
}

// release clears the park, for the caller to persist with the Save it already
// makes on its own success path.
//
// Deliberately does NOT Save: the clear must land in the SAME write that
// records the successful outcome, so that a failure of that write leaves the
// parked secret on disk. That is the one case where it is still the only copy
// — the Hub committed, the reply arrived, and the local record of it did not
// land — which is exactly when it must survive. Callers that reach their own
// reportUnsavedResult path therefore correctly leave the park in place.
func (p parkedCashSecret) release() {
	if p.ledger == nil {
		return
	}
	// Re-found rather than remembered — see the type's own doc comment on why
	// a pointer held across the caller's l.Add goes stale.
	e, found := p.ledger.Find(p.entryID)
	if !found {
		return
	}
	// Compared before clearing so a concurrent `cashctl receive`/`protect`
	// that re-parked this row for its own, different call is never silently
	// discarded by this one's success.
	if e.PendingDestinationCashSecret == p.secret {
		e.PendingDestinationCashSecret = ""
	}
}

// unreconciledDestinationParks returns one line per ledger row still carrying
// a destination cash secret that was never reconciled, newest row last.
//
// This is the read side of the write-ahead log, and the reason the column is
// worth more than a passing test: a secret persisted where no human can ever
// see it protects nothing. A non-empty PendingDestinationCashSecret means some
// earlier `transfer --to cash`/`consolidate --to cash` placed its call and
// never recorded an outcome — the process was killed, or its final Save
// failed. The bill may or may not exist on the Hub; what IS certain is that if
// it does, this is the only copy of the secret that can spend it, and it is
// necessary (though on its own not sufficient — the bill's token comes back
// only in the reply) to recover the funds.
//
// Scans every entry rather than l.Held(): a park survives on a row that an
// interrupted consolidate had already marked consolidated, which Held()
// excludes by definition.
//
// The secret is printed in full, deliberately. cashctl already prints
// `<token>#<secret>` on the ordinary success path with the same "save this now"
// framing — so this is not a new class of exposure, and the alternative is a
// value no user can act on.
//
// This comment used to cite reportUnsavedResult's recovery path as a second
// precedent for printing it. That was false: until D-CLI-6 that path had its
// secret removed by wrapCLIError's catch-all RedactSecretInput, whose
// giftSecretPattern matches `#<64 hex>`, so it printed `#<redacted>` and no
// secret at all. It genuinely does print one now, through CLIError.Recovery.
// Worth keeping as a note: the reasoning here was sound but rested on a
// neighbouring behaviour nobody had tested.
func unreconciledDestinationParks(l *ledger.Ledger) []string {
	var lines []string
	for _, e := range l.Entries {
		if e.PendingDestinationCashSecret == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf(
			"  %s: an interrupted send from this token may have created a cash bill. "+
				"Its secret — the only copy, save it now: %s\n"+
				"     You also need that bill's token to spend it, which only the Hub can still tell you; "+
				"check `cashctl wallet history` and the Hub's own records before assuming the send failed.",
			e.ID, e.PendingDestinationCashSecret))
	}
	return lines
}
