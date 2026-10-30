package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ohstr/nmilat/nipcash"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// errBillSpent is what every path says once a bill's Hub has answered with a
// tombstone.
var errBillSpent = errors.New("the Hub says this bill is spent: its value has already moved and the bill is gone")

var (
	errBillExpired   = errors.New("the Hub says this bill expired: its value went back to the Hub and the bill is gone")
	errBillReclaimed = errors.New("the Hub says your slice of this bill was reclaimed: it was removed and its value swept back to the Hub, so it is not yours any more")
)

// The proof an answer about a held bill was given for (NIP-CP §Balance).
const (
	proofIdentityKey   = "identity_key"
	proofCashSecret    = "cash_secret"
	proofConnectionKey = "connection_key"
	proofOtherKey      = "other_key"
)

// billAnswer is one answer about a held bill, from cash_status, cash_redeem,
// cash_transfer, cash_consolidate or a lookup done.
type billAnswer struct {
	OwnSpend  bool
	Tombstone *nipcash.Tombstone
	NotFound  bool
	Proof     string // for NotFound: one of the proof* kinds
}

// stateForResult is NIP-CP §Balance's result→state table, the one mapping
// every call site uses. "" means no state is written: no answer, or NOT_FOUND
// for any proof but the entry's own key or cash secret (a connection key's
// included, SG-1).
func stateForResult(a billAnswer) string {
	switch {
	case a.OwnSpend:
		return ledger.StatusSpent
	case a.Tombstone != nil:
		switch a.Tombstone.Reason {
		case nipcash.ReasonExpired:
			return ledger.StatusExpired
		case nipcash.ReasonReclaimed:
			return ledger.StatusReclaimed
		}
		return ledger.StatusSpent
	case a.NotFound && (a.Proof == proofIdentityKey || a.Proof == proofCashSecret):
		return ledger.StatusReclaimed
	}
	return ""
}

// stateForTombstone is stateForResult for a tombstone.
func stateForTombstone(t *nipcash.Tombstone) string {
	if t == nil {
		t = &nipcash.Tombstone{Status: nipcash.StatusSpent}
	}
	return stateForResult(billAnswer{Tombstone: t})
}

// markFinal writes a definitive answer's state on an entry still held and
// saves. A final state is never overwritten, and nothing else is ever
// written from a timeout.
func markFinal(l *ledger.Ledger, entry *ledger.Entry, state string) {
	if l == nil || entry == nil || state == "" || entry.Status != ledger.StatusHeld {
		return
	}
	_ = l.SetStatus(entry.ID, state)
	l.AppendHistory(state, fmt.Sprintf("the Hub reported token %s %s", entry.ID, state))
	_ = l.Save() // best-effort: the next run asks the Hub again and gets the same answer
}

// goneCause is what a bill given state by its Hub is told as.
func goneCause(state string) error {
	switch state {
	case ledger.StatusExpired:
		return errBillExpired
	case ledger.StatusReclaimed:
		return errBillReclaimed
	}
	return errBillSpent
}

// goneError is the error for a bill whose Hub gave it the final state; expired
// and reclaimed carry it as the --json reason.
func goneError(cmd *cobra.Command, id, state string) error {
	return goneErrorf(cmd, id, state, goneCause(state))
}

// goneErrorf is goneError with the message given.
func goneErrorf(cmd *cobra.Command, id, state string, err error) error {
	if state == ledger.StatusExpired || state == ledger.StatusReclaimed {
		return output.WithReason(output.SpentError(cmd, id, err), state)
	}
	return output.SpentError(cmd, id, err)
}

// recordGone marks a held entry from its Hub's tombstone (NIP-CASH §Answering
// About a Destroyed Bill), so `wallet balance` and `redeem --all` stop offering
// it, and returns its error. Only ever called on a tombstone: silence proves
// nothing (NIP-CASH: a client MUST NOT report a bill spent on a timeout alone).
func recordGone(cmd *cobra.Command, l *ledger.Ledger, entry *ledger.Entry, t *nipcash.Tombstone) error {
	state := stateForTombstone(t)
	markFinal(l, entry, state)
	return goneError(cmd, entry.ID, state)
}

// markGone is recordGone without the error, for a read that succeeded.
func markGone(l *ledger.Ledger, entry *ledger.Entry, t *nipcash.Tombstone) {
	markFinal(l, entry, stateForTombstone(t))
}

// recordNotFound handles a Hub's NOT_FOUND about entry's own slice: reclaimed
// for a proof by the entry's own key or cash secret, the status_unreadable
// refusal for a connection key (nothing written, SG-1), and ok=false for any
// other proof, whose caller keeps its own error.
func recordNotFound(cmd *cobra.Command, l *ledger.Ledger, entry *ledger.Entry, proof string) (error, bool) {
	if state := stateForResult(billAnswer{NotFound: true, Proof: proof}); state != "" {
		markFinal(l, entry, state)
		return goneError(cmd, entry.ID, state), true
	}
	if proof == proofConnectionKey {
		return statusUnreadableError(cmd, entry.ID), true
	}
	return nil, false
}

// hubNotFound is a Hub's NOT_FOUND, raw or already classified.
func hubNotFound(err error) bool {
	if err == nil {
		return false
	}
	var ce *output.CLIError
	return isNotFound(err) || (errors.As(err, &ce) && ce.NWCCode == "NOT_FOUND")
}

// proofOf is the proof a call on entry is made with: --as (or CASHCTL_AS)
// when given, else the entry's own credential. A --as key cashctl cannot tie
// to the entry is another key, so its NOT_FOUND writes nothing.
func proofOf(cmd *cobra.Command, entry *ledger.Entry) string {
	if isConnectionKeySpend(cmd, entry) {
		return proofConnectionKey
	}
	if as := asCredentialValue(cmd); as != "" {
		if secret, ok := strings.CutPrefix(as, "cash:"); ok && isCashNote(entry) && secret == entry.CashSecret {
			return proofCashSecret
		}
		return proofOtherKey
	}
	switch {
	case entry.ThrowawayKey != "":
		return proofIdentityKey
	case isCashNote(entry):
		return proofCashSecret
	case entry.IdentityRequired != nil && !*entry.IdentityRequired:
		return proofOtherKey
	}
	// A pubkey-mode slice is addressed to one identity key. After an identity
	// rotation the key now signing may no longer be that key: a NOT_FOUND to a
	// DIFFERENT key proves nothing about the slice (R-CP-RECLAIM), so only a
	// proof BY the key the slice was addressed to is the entry's own identity
	// key (reclaimed); anything else is other_key, whose NOT_FOUND writes no
	// terminal state. A legacy/empty IdentityPubkey cannot be tied to the signer
	// either ("unknown, can't tie"), so it must fail SAFE to other_key — never
	// terminal reclaimed — exactly like a rotated key (NIP-CASH:1589-1590 / :3718).
	if entry.IdentityPubkey == "" {
		return proofOtherKey
	}
	if signer, err := signerPubKeyHex(cmd, entry); err != nil || signer != entry.IdentityPubkey {
		return proofOtherKey
	}
	return proofIdentityKey
}

// proofOfCall is proofOf for a call already placed, from its authorizer.
func proofOfCall(c *ledger.Call, entry *ledger.Entry) string {
	switch {
	case entry != nil && entry.ConnectionKeyPlatform != "":
		return proofConnectionKey
	case c.AuthorizerKind == ledger.AuthLocalIdentity, c.AuthorizerKind == ledger.AuthThrowawayKey:
		return proofIdentityKey
	case c.AuthorizerKind == ledger.AuthCashSecret && entry != nil && isCashNote(entry):
		return proofCashSecret
	}
	return proofOtherKey
}

// markRefusedNotFound gives a cash_transfer refused NOT_FOUND, and never
// applied, its source's state (stateForResult): reclaimed when the call was
// authorized by the entry's own key or cash secret. A consolidate's NOT_FOUND
// does not say which source it is about, so it writes nothing.
// It returns the state written, "" for none; the caller saves.
func markRefusedNotFound(l *ledger.Ledger, c *ledger.Call, code string) string {
	if code != "NOT_FOUND" || c.Method != nipcash.MethodCashTransfer || len(c.SourceEntryIDs) != 1 {
		return ""
	}
	e, ok := l.Find(c.SourceEntryIDs[0])
	if !ok || e.Status != ledger.StatusHeld {
		return ""
	}
	state := stateForResult(billAnswer{NotFound: true, Proof: proofOfCall(c, e)})
	if state != "" {
		_ = l.SetStatus(e.ID, state)
		l.AppendHistory(state, fmt.Sprintf("the Hub reported token %s %s (NOT_FOUND)", e.ID, state))
	}
	return state
}

// alreadySpentError refuses an explicitly named entry that is no longer held.
//
// Spent, the same answer the Hub's tombstone gives, for a bill reported spent,
// expired or reclaimed and for one this wallet redeemed or merged away: those
// statuses are written only on the Hub's own answer (NIP-CASH §Answering About
// a Destroyed Bill). Asking it again would only add a network call, and past
// the window its silence. not_found for a bill transferred away: it still
// exists on the Hub, under its new holder, and is simply no longer yours.
func alreadySpentError(cmd *cobra.Command, id, status string) error {
	switch status {
	case ledger.StatusExpired, ledger.StatusReclaimed:
		return output.WithReason(output.SpentError(cmd, id, fmt.Errorf("token %q was already %s, so it is not yours any more", id, spentStatusDescription(status))), status)
	case ledger.StatusSpent, ledger.StatusRedeemed, ledger.StatusConsolidated:
		return output.SpentError(cmd, id, fmt.Errorf("token %q was already %s, so it is spent: its value has already moved", id, spentStatusDescription(status)))
	}
	return output.NotFoundError(cmd, id, fmt.Errorf("token %q was already %s, so it is no longer yours", id, spentStatusDescription(status)))
}

// expiryPtr is a Hub's expires_at as the ledger stores it: nil for a bill that
// never expires (absent, null or 0 on the wire).
func expiryPtr(e nipcash.Expiry) *int64 {
	if e.Never() {
		return nil
	}
	at := e.At
	return &at
}
