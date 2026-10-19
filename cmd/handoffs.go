package cmd

import (
	"context"
	"errors"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	relayclient "github.com/ohstr/nmilat/relay/client"

	"github.com/ohstr/cashctl/internal/credential"
	"github.com/ohstr/cashctl/internal/ledger"
)

// recordHandoff keeps a handoff in the ledger (I-27): cashNote when the
// recipient is whoever holds "<token>#<secret>", otherwise token as a gift to
// an identity. A no-op when there is nothing to hand over.
func recordHandoff(l *ledger.Ledger, cashNote, token string, amountMillis uint64, target string) {
	h := ledger.Handoff{AmountMillis: amountMillis, Target: target}
	switch {
	case cashNote != "":
		h.Kind, h.Handoff = ledger.HandoffCashNote, cashNote
		token, _ = nipcash.SplitCashSliceString(cashNote)
	case token != "":
		h.Kind, h.Handoff = ledger.HandoffGift, token
	default:
		return
	}
	if tok, err := nipcash.Decode(token); err == nil {
		h.WalletPubkey = tok.WalletPubkey
	}
	l.AddHandoff(h)
}

// handoffTarget names a handoff's recipient for history: "cash" for a cash
// note, otherwise what the target resolved to (or what was typed).
func handoffTarget(toValue string, target credential.ResolvedTarget) string {
	if _, ok := target.Target.(*nipcash.CashTarget); ok {
		return "cash"
	}
	if target.Resolved != "" {
		return target.Resolved
	}
	return toValue
}

// refreshHandoffs re-checks each still-unclaimed cash note with its own secret:
// a match means nobody took it yet; a tombstone, or a secret that no longer
// matches, means it was claimed; EXPIRED means it lapsed. No answer changes
// nothing — silence says nothing about a bill (NIP-CASH §Cash Status). A gift
// to an identity stays "sent": its sender cannot see whether it was claimed.
// Saves best-effort when anything changed.
func refreshHandoffs(l *ledger.Ledger) {
	changed := false
	for i := range l.Handoffs {
		h := &l.Handoffs[i]
		if h.Kind != ledger.HandoffCashNote || h.Status != ledger.HandoffUnclaimed {
			continue
		}
		if status := checkHandoffFn(h.Handoff); status != "" {
			h.Status, h.CheckedAt = status, time.Now().UTC().Format(time.RFC3339)
			changed = true
		}
	}
	if changed {
		_ = l.Save()
	}
}

// checkHandoff asks the note's Hub about it with its own secret; "" when the
// Hub gave no usable answer.
func checkHandoff(note string) string {
	token, secret := nipcash.SplitCashSliceString(note)
	tok, err := nipcash.Decode(token)
	if err != nil || secret == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := nipcashclient.Connect(ctx, token)
	if err != nil {
		return ""
	}
	defer client.Close()
	_, err = client.CheckClaim(ctx, nipcash.BySecret(secret), tok, nipcash.NoLocalIdentity)
	var walletErr *relayclient.WalletError
	switch {
	case err == nil:
		return ledger.HandoffUnclaimed
	// A secret that no longer matches arrives as NOT_FOUND, which CheckClaim
	// returns as ErrClaimNotFound: someone took the note and re-keyed it.
	case errors.Is(err, nipcash.ErrSpent), errors.Is(err, nipcash.ErrClaimNotFound):
		return ledger.HandoffClaimed
	case errors.As(err, &walletErr) && walletErr.Code == "EXPIRED":
		return ledger.HandoffExpired
	}
	return ""
}

// checkHandoffFn is checkHandoff, swapped out in tests.
var checkHandoffFn = checkHandoff

// maskedHandoff hides an unclaimed cash note's secret, which still spends it,
// unless showSecrets: the token alone is shown, so it can still be recognized.
func maskedHandoff(h ledger.Handoff, showSecrets bool) (string, bool) {
	if showSecrets || h.Kind != ledger.HandoffCashNote || h.Status != ledger.HandoffUnclaimed {
		return h.Handoff, false
	}
	token, _ := nipcash.SplitCashSliceString(h.Handoff)
	return token + "#<hidden: --show-secrets>", true
}

// handoffJSON is a handoff as --json shows it.
func handoffJSON(h ledger.Handoff, showSecrets bool) map[string]any {
	shown, hidden := maskedHandoff(h, showSecrets)
	return map[string]any{
		"id": h.ID, "at": h.At, "kind": h.Kind, "handoff": shown, "secret_hidden": hidden,
		"amount_millis": h.AmountMillis, "target": h.Target, "status": h.Status, "checked_at": h.CheckedAt,
	}
}

// unclaimedHandoffs is every handoff not known to be claimed or expired, as of
// its last check, secrets hidden.
func unclaimedHandoffs(l *ledger.Ledger) []map[string]any {
	out := []map[string]any{}
	for _, h := range l.Handoffs {
		if h.Status == ledger.HandoffUnclaimed || h.Status == ledger.HandoffSent {
			out = append(out, handoffJSON(h, false))
		}
	}
	return out
}
