package cmd

import (
	"github.com/ohstr/nmilat/nipcash"

	"github.com/ohstr/cashctl/internal/ledger"
)

// spendCashEntry places a single wire call that spends entry — with
// cred, resolveCredential's own choice — and, if entry carries a
// not-yet-reconciled PendingCashSecret (see cmd/receive_secure.go's own
// doc comment on how one gets there: a protect step that couldn't confirm
// whether its rekey landed before failing) and the call declines
// specifically as a wrong-secret NOT_FOUND, retries once with the pending
// secret instead.
//
// This is the only place cashctl can ever actually learn which of two
// candidate cash secrets an interrupted rekey left live — NIP-CASH has
// no read-only way to ask (nipcashclient.CheckClaim's own doc comment: a
// cash-mode match only proves *some* recipient exists, never *which* secret)
// — so it happens here, at the point an entry is genuinely about to be
// spent, rather than as a separate "reconcile" step nothing else in
// cashctl calls unprompted.
//
// Deliberately doesn't Save() the promotion itself: every caller of this
// (redeem, transfer) already does its own SetStatus+Save immediately after
// a successful spend, which picks up entry's mutated fields for free —
// see internal/ledger.Entry being a pointer into the ledger's own slice.
//
// place is called with the credential to use; T is whatever result type
// the specific wire method returns (CashRedeemResult, CashTransferResult,
// ...). Never second-guesses an explicit --as override or a identity-bound
// entry — resolveCredential already priced those in before cred ever
// reached here, and PendingCashSecret is only ever set on a genuinely
// cash-mode entry to begin with.
func spendCashEntry[T any](entry *ledger.Entry, cred nipcash.Credential, place func(nipcash.Credential) (T, error)) (T, error) {
	result, err := place(cred)
	if err == nil || entry.PendingCashSecret == "" || entry.PendingCashSecret == entry.CashSecret || !isWrongSecretDecline(err) {
		return result, err
	}
	pending := entry.PendingCashSecret
	retryResult, retryErr := place(nipcash.BySecret(pending))
	if retryErr != nil {
		// The FIRST decline is the one worth reporting — a second guess's
		// own failure adds nothing (and could itself just be the same
		// wrong-secret signal, now for a genuinely-wrong pending value).
		return result, err
	}
	entry.CashSecret = pending
	entry.PendingCashSecret = ""
	return retryResult, nil
}
