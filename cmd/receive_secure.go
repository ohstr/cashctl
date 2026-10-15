package cmd

import (
	"context"
	"errors"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
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
	return out
}

// isWrongSecretDecline reports whether err looks like a cash_secret
// mismatch (NWC code NOT_FOUND) rather than some other failure — NIP-CASH
// §Redemption Metadata guarantees a wrong secret always declines this way.
func isWrongSecretDecline(err error) bool {
	var walletErr *relayclient.WalletError
	return errors.As(err, &walletErr) && walletErr.Code == "NOT_FOUND"
}

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

// protectRekeyOnly is protectCashReceipt's no-merge path: re-key entry's
// cash secret in place, without going through nipcashclient.RekeyCashSlice.
//
// A cash-mode target's secret is generated purely locally — NIP-CASH §Cash-Mode
// Slices: the caller supplies only a one-way commitment over the wire, the
// secret itself never crosses it — so there is no reason to let placing the
// call be the difference between "the new secret exists on disk" and "the
// new secret exists only in this process's own memory, gone the instant it
// dies." nipcash.NewCashTarget() is generated here, in cashctl's own
// code, and entry.PendingCashSecret is saved BEFORE the call — the same
// write-ahead discipline a database uses for its own log.
//
// Once persisted, a kill or lost response can leave the outcome genuinely
// ambiguous: NIP-CASH has no read-only way to ask the Hub which of two
// candidate secrets it accepted (nipcashclient.CheckClaim's own doc
// comment — a cash-mode match only proves *some* recipient exists, never
// *which* secret). resolveCredential's own fallback resolves that
// ambiguity the only way the protocol allows: by trying the pending secret
// on the entry's next real spend attempt if the usual one is declined.
func protectRekeyOnly(cmd *cobra.Command, l *ledger.Ledger, entry *ledger.Entry, jsonMode bool) (protectedStatus, *ledger.Entry) {
	bt := nipcash.NewCashTarget()
	entry.PendingCashSecret = bt.Secret()
	if err := l.Save(); err != nil {
		// Nothing has been sent to the Hub yet — entry.CashSecret is
		// still the only real secret, so this is an ordinary failure.
		entry.PendingCashSecret = ""
		printProtectFailure(jsonMode, err)
		return protectedStatus{Status: "failed", Error: err.Error()}, entry
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var dialErr bool
	var result *nipcash.CashTransferResult
	err := WithSpinner(jsonMode, "Protecting...", func() error {
		client, cErr := nipcashclient.Connect(ctx, entry.Token)
		if cErr != nil {
			dialErr = true
			return cErr
		}
		defer client.Close()
		r, cErr := client.CashTransfer(ctx, nipcash.CashTransferParams{
			Credential:    nipcash.BySecret(entry.CashSecret),
			To:            bt,
			CurrentAmount: *entry.AmountMillis,
		})
		if cErr != nil {
			return cErr
		}
		result = r
		return nil
	})
	if err != nil {
		var walletErr *relayclient.WalletError
		// A dial failure never reached the Hub at all; a *WalletError is
		// the Hub actually answering with a real decline. Either way, the
		// call is DEFINITELY known not to have taken effect — safe to
		// retract the pending secret and keep reporting the old one.
		if dialErr || errors.As(err, &walletErr) {
			entry.PendingCashSecret = ""
			_ = l.Save() // best-effort tidy-up; entry.CashSecret itself is unchanged either way
			wrongSecret := isWrongSecretDecline(err)
			if wrongSecret {
				printProtectFailureWrongSecret(jsonMode, err)
			} else {
				printProtectFailure(jsonMode, err)
			}
			return protectedStatus{Status: "failed", Error: err.Error(), LikelyWrongSecret: wrongSecret}, entry
		}
		// Ambiguous: a transport-level failure (timeout, dropped
		// connection) with no definitive answer from the Hub. Leave
		// PendingCashSecret exactly as already saved above — don't
		// touch it either way — so resolveCredential can try it on the
		// next real spend of this entry.
		printProtectAmbiguousFailure(jsonMode, err)
		return protectedStatus{Status: "failed", Error: err.Error(), PendingSecretUnresolved: true}, entry
	}
	// The Hub never returns the secret itself for this call — see
	// CashTransferResult's own doc comment; bt.Secret() IS the new secret.
	//
	// But it does not always re-key IN PLACE. When this bill has changed hands
	// before, the Hub carves the slice into a brand-new wallet instead, because
	// re-keying in place would leave the new secret on a connection a previous
	// holder still reaches — that connection's secret is derived from the app ID
	// and cannot be rotated, so giving a bill away never gives up access to it.
	// Carving is what makes this protection real.
	//
	// So the bill may have MOVED, and following it is not optional: the carve
	// drains and deletes the old wallet, so keeping the old token here leaves
	// every later command addressing a bill that no longer exists — the Hub
	// answers "no bill with this target wallet_pubkey" and the funds look gone.
	// The new token arrives inside the private transport's own sealed reply, so
	// only this caller can read it.
	moved := result != nil && result.NewWalletToken != ""
	if moved {
		entry.Token = result.NewWalletToken
		if result.NewWalletPubkey != "" {
			entry.WalletPubkey = result.NewWalletPubkey
		}
	}

	entry.CashSecret = bt.Secret()
	entry.PendingCashSecret = ""
	entry.CashProtection = ledger.CashProtected
	if moved {
		l.AppendHistory("secure", "re-keyed this cash into a new bill so the shared secret can no longer spend it, and the previous holder's connection no longer reaches it")
	} else {
		l.AppendHistory("secure", "re-keyed this cash so the shared secret can no longer spend it")
	}
	if err := l.Save(); err != nil {
		// The rekey is CONFIRMED (the call above returned no error) — but
		// this save's own failure is harmless data-safety-wise: SQLite
		// commits l.save()'s whole batch atomically (internal/ledger.Save's
		// own doc comment), so a failed commit here leaves the row exactly
		// as the write-ahead save above left it — CashSecret still the
		// OLD value, PendingCashSecret still bt.Secret() — which is
		// exactly the state resolveCredential's fallback already knows how
		// to recover from. Report it the same way as the ambiguous case
		// above rather than inventing a third message for what is, from
		// disk's point of view, the identical situation.
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
	output.Printf("Received, but couldn't confirm protecting worked (%v).\nMay have worked anyway — no action needed, your next spend tries both.\n", err)
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
