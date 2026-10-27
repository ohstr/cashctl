package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ohstr/nmilat/nipcash"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// protectedStatus is receive's own abstracted report of what the automatic
// protect step did — never named "transfer"/"consolidate"/"cash_secret"
// to the user, only the outcome: rekeyed | consolidated | not_applicable |
// declined | failed. Surfaced under --json alongside "entry".
type protectedStatus struct {
	Status           string
	ConsolidatedWith []string
	FinalEntryID     string
	Error            string
	// LikelyWrongSecret is set when Status is "failed" and the decline
	// looks like the embedded cash_secret itself was wrong (NWC code
	// NOT_FOUND), not a transient network/server issue — unlike an
	// ordinary failed-protect case, this likely means the entry was
	// never really spendable at all.
	LikelyWrongSecret bool
	// PendingSecretUnresolved is set when Status is "failed" and the
	// failure was a transport-level one (timeout, dropped connection) with
	// no definitive answer from the Hub — the rekey may have landed
	// anyway. The candidate secret is safely on disk regardless (see
	// protectRekeyOnly's own doc comment) and resolveCredential tries it
	// automatically on the next spend of this entry — this is never a
	// dead end the way LikelyWrongSecret's case is.
	PendingSecretUnresolved bool
	// Unresolved is set when Status is "failed" because an earlier re-key or
	// move of this bill was never confirmed and the Hub could not settle it:
	// nothing was written and no call was placed, since a new secret written
	// over the pending one could destroy the only secret that spends it.
	Unresolved bool
	// Recovery is the <token>#<secret> handoff for a confirmed re-key whose
	// local save failed after the Hub moved the bill to a new token.
	Recovery string
}

func (s protectedStatus) json() map[string]any {
	out := map[string]any{"status": s.Status}
	if len(s.ConsolidatedWith) > 0 {
		out["consolidated_with"] = s.ConsolidatedWith
	}
	if s.FinalEntryID != "" {
		out["final_entry_id"] = s.FinalEntryID
	}
	if s.Error != "" {
		out["error"] = s.Error
	}
	if s.LikelyWrongSecret {
		out["likely_wrong_secret"] = true
	}
	if s.PendingSecretUnresolved {
		out["pending_secret_unresolved"] = true
	}
	if s.Unresolved {
		out["unresolved"] = true
	}
	if s.Recovery != "" {
		out["recovery"] = s.Recovery
	}
	return out
}

// isWrongSecretDecline reports whether err is a cash_secret mismatch rather
// than some other failure.
func isWrongSecretDecline(err error) bool {
	var walletErr *relayclient.WalletError
	return errors.As(err, &walletErr) && isWrongSecretCode(walletErr.Code)
}

// isWrongSecretCode is the code a Hub answers a cash_secret that does not
// verify with: UNAUTHORIZED, never NOT_FOUND, which means a verified holder
// with no slice (NIP-CASH §Errors).
func isWrongSecretCode(code string) bool { return code == "UNAUTHORIZED" }

// protectCashReceipt is receive's automatic follow-up for a freshly
// received cash-mode entry: confirm with the user, then re-key it
// (nipcashclient.RekeyCashSlice) — optionally consolidating with other
// held same-minter tokens — so the original, possibly-shared secret can
// no longer spend it. Best-effort: a failure here never fails receive
// itself, since the bill is already genuinely theirs by the time this
// runs (checkClaimWithCashHub already passed). Returns the report to
// surface and the entry that should now be treated as "the" result of
// this receive (the same entry for rekeyed/declined/failed/not_applicable,
// a new one for consolidated).
func protectCashReceipt(cmd *cobra.Command, l *ledger.Ledger, entry *ledger.Entry, isCash bool) (protectedStatus, *ledger.Entry) {
	jsonMode, _ := cmd.Flags().GetBool("json")
	if !isCash {
		return protectedStatus{Status: "not_applicable"}, entry
	}
	// defaultYes=true is intentional here, unlike a money-moving prompt:
	// declining leaves the holding MORE exposed (still shared), not less —
	// an unattended --yes/--json/EOF receive protecting by default is the
	// safe direction, same reasoning as auto-securing at all. What the
	// audit that found this actually flagged: the prompt itself never
	// explained the mechanism, so even the interactive path left a human
	// agreeing to something opaque — fixed by naming it plainly, not by
	// flipping the default.
	if !Confirm(cmd, true, "This cash is still shared — anyone holding it can spend it.\n"+
		"Protect it now? (re-keys it under a new secret only you hold)") {
		return protectedStatus{Status: "declined"}, entry
	}

	// Re-key only, never merge: merging tells the Hub the bills share an owner,
	// which the holder decides explicitly with `consolidate` (NIP-CASH §Privacy).
	return protectRekeyOnly(cmd, l, entry, jsonMode)
}

// protectRekeyOnly is protectCashReceipt's no-merge path: re-key entry's cash
// secret onto a fresh one only this wallet holds.
//
// The new secret is generated here and only its commitment crosses the wire
// (NIP-CASH §Cash-Mode Slices). It is written ahead with the call (its
// call_secrets row) before anything is sent, so a kill or a lost answer never
// costs it: the call stays open, the bill is not used again until the Hub says
// what happened, and `wallet show` lists the new code meanwhile.
func protectRekeyOnly(cmd *cobra.Command, l *ledger.Ledger, entry *ledger.Entry, jsonMode bool) (protectedStatus, *ledger.Entry) {
	if _, open := l.OpenCallFor(entry.ID); open {
		printProtectUnresolved(jsonMode, entry.ID)
		return protectedStatus{Status: "failed", Error: errUnresolvedRekey(entry.ID).Error(), Unresolved: true}, entry
	}
	bt, err := nipcash.NewCashTarget()
	if err != nil {
		printProtectFailure(jsonMode, err)
		return protectedStatus{Status: "failed", Error: err.Error()}, entry
	}
	id := entry.ID
	var result *nipcash.CashTransferResult
	var call *ledger.Call
	err = WithSpinner(jsonMode, "Protecting...", func() error {
		var cErr error
		result, call, cErr = moveCash(cmd, l, entry, nipcash.BySecret(entry.CashSecret), bt, purposeProtect)
		return cErr
	})
	entry, _ = l.Find(id)
	if err != nil {
		if output.AsCLIError(err).Code == output.CodeIndeterminate {
			// No decided answer: the call stays open with the new secret, and is
			// looked up before this bill is used again.
			printProtectAmbiguousFailure(jsonMode, err)
			return protectedStatus{Status: "failed", Error: err.Error(), PendingSecretUnresolved: true}, entry
		}
		printProtectFailure(jsonMode, err)
		return protectedStatus{Status: "failed", Error: err.Error()}, entry
	}
	// The Hub does not always re-key IN PLACE. When this bill has changed hands
	// before, it carves the slice into a brand-new wallet, because the old
	// connection's secret cannot be rotated and a previous holder still reaches
	// it. So the bill may have MOVED, and following it is not optional: the
	// carve drains and deletes the old wallet.
	moved := result != nil && result.NewWalletToken != ""
	if moved {
		entry.Token = strings.ToLower(result.NewWalletToken)
		if result.NewWalletPubkey != "" {
			entry.WalletPubkey = result.NewWalletPubkey
		}
		entry.Verified, entry.MinterPubkey = returnedBillCheck(entry.Token, entry.MinterPubkey)
		if !entry.Verified {
			output.Notef(jsonMode, unverifiedReturnNote)
		}
	}
	entry.CashSecret = bt.Secret()
	entry.CashProtection = ledger.CashProtected
	closeCall(l, call, ledger.CallDone, "")
	if moved {
		l.AppendHistory("secure", "re-keyed this cash into a new bill so the shared secret can no longer spend it, and the previous holder's connection no longer reaches it")
	} else {
		l.AppendHistory("secure", "re-keyed this cash so the shared secret can no longer spend it")
	}
	if err := l.Save(); err != nil {
		// The re-key is CONFIRMED, but disk still holds the call open with the
		// new secret. In place, that is recoverable: the call is looked up. After
		// a carve the new token exists only in this process, so hand it over.
		if moved {
			recovery := entryRecoveryString(entry)
			printProtectUnsaved(jsonMode, err, recovery)
			return protectedStatus{Status: "failed", Error: err.Error(), Recovery: recovery}, entry
		}
		printProtectAmbiguousFailure(jsonMode, err)
		return protectedStatus{Status: "failed", Error: err.Error(), PendingSecretUnresolved: true}, entry
	}
	if !jsonMode {
		output.Println("Protected.")
	}
	return protectedStatus{Status: "rekeyed"}, entry
}

func printProtectFailure(jsonMode bool, err error) {
	if jsonMode {
		return
	}
	output.Printf("Received, but protecting failed (%v) — still shared. Retry: `cashctl wallet protect`.\n", err)
}

// printProtectAmbiguousFailure is printProtectFailure's counterpart for a
// transport-level failure with no definitive answer from the Hub — unlike
// the ordinary case, this is never a dead end: the candidate secret this
// attempt generated is already safely on disk, and the next real spend of
// this entry (redeem/transfer/consolidate) tries it automatically if the
// usual secret is declined (see resolveCredential's own doc comment).
func printProtectAmbiguousFailure(jsonMode bool, err error) {
	if jsonMode {
		return
	}
	output.Printf("Couldn't confirm protecting worked (%v).\n%s\n", err, protectAmbiguousAdvice)
}

// protectAmbiguousAdvice is what an unconfirmed re-key tells the holder. It
// never says "no action needed": after a carve the bill may have moved to a
// token this wallet never learned.
const protectAmbiguousAdvice = "It may have worked. cashctl settles it before your next spend; don't hand out the old code again."

// errUnresolvedRekey is the refusal for a bill with a call still open: placing
// another could act on a bill whose state only the Hub knows.
func errUnresolvedRekey(id string) error {
	return fmt.Errorf("an earlier call on %s has no answer yet, so nothing was changed. "+
		"`cashctl wallet show` lists it with any new code it carries, which may be the only one that spends the bill now; "+
		"don't hand out the old code again", id)
}

func printProtectUnresolved(jsonMode bool, id string) {
	if jsonMode {
		return
	}
	output.Printf("Not protected: %v.\n", errUnresolvedRekey(id))
}

// printProtectUnsaved is the text-mode line for a confirmed re-key whose save
// failed after a carve; the recovery itself also travels in the error.
func printProtectUnsaved(jsonMode bool, err error, recovery string) {
	if jsonMode {
		return
	}
	output.Printf("Moved on the Hub, but saving that locally failed (%v). Save this now, it will not be shown again:\n%s\n", err, recovery)
}

// printProtectFailureWrongSecret is printProtectFailure's counterpart for
// isWrongSecretDecline — printed instead of, never alongside, the
// ordinary message, whose "still shared" framing is false reassurance
// when the real problem is that this cash was never genuinely redeemable
// with the secret presented at all.
func printProtectFailureWrongSecret(jsonMode bool, err error) {
	if jsonMode {
		return
	}
	output.Printf("Received, but the secret doesn't match (%v) — likely wrong/truncated. Ask for the full \"token#secret\" string again.\n", err)
}

func printProtectPartialFailure(jsonMode bool, err error) {
	if jsonMode {
		return
	}
	output.Printf("Received — old secret is dead and this is yours alone now; only the merge failed (%v). Nothing at risk; merge later with `cashctl consolidate`.\n", err)
}
