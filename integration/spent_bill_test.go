//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/ohstr/nmilat/nipcash"
)

// requireBillSpentAway asserts that a bill whose value has moved away is gone
// from the Hub — read from the Hub's own records, not from the bill's
// connection.
//
// This used to dial the bill and infer destruction from what came back: a
// "spent" tombstone, or a timeout taken to mean silence. Both inferences are
// now unavailable to a holder. Bill methods are served over the private
// transport, which authorizes per recipient and omits anything it cannot serve,
// so a destroyed bill and a live bill this caller has no slice of produce the
// same non-answer. Worse, so does a Hub that is merely slow — which made
// "timed out, therefore destroyed" a check that passes when the Hub is down.
//
// The operator's view has none of those ambiguities: a bill is destroyed when
// every slice under its wallet pubkey has moved to the archive. That is the
// actual claim these tests make, stated directly.
//
// What a NORMAL client — one with no admin API — can and cannot observe is a
// real property worth testing, but it is a different property, and it belongs
// in a test of its own rather than smuggled into every test that merely needs
// to know a bill is gone. See TestSpentBill_PlainClientObservesNothing.
//
// Still retried rather than asserted once, because the Hub deletes the bill
// AFTER answering the request that emptied it: for a moment afterwards its
// slices are still live.
func requireBillSpentAway(t *testing.T, admin *adminClient, hubAppID uint, token, what string) {
	t.Helper()

	// Callers hold whatever string they were given, which for a cash-mode bill
	// carries the "#secret" suffix; only the token itself decodes.
	tokenOnly, _ := nipcash.SplitCashSliceString(token)
	tok, err := nipcash.Decode(tokenOnly)
	if err != nil {
		t.Fatalf("%s: decode token: %v", what, err)
	}

	const deleteWindow = 20 * time.Second
	deadline := time.Now().Add(deleteWindow)
	for {
		rows, err := admin.listCashWalletClaims(hubAppID)
		if err != nil {
			t.Fatalf("%s: list cash wallet claims: %v", what, err)
		}
		var known, live int
		for _, r := range rows {
			if r.WalletPubkey != tok.WalletPubkey {
				continue
			}
			known++
			if !r.Archived {
				live++
			}
		}
		if known > 0 && live == 0 {
			return
		}
		if time.Now().After(deadline) {
			if live > 0 {
				t.Fatalf("%s: %s after this bill's value moved away the Hub still holds %d live slice(s) on wallet %s — the bill was not destroyed",
					what, deleteWindow, live, tok.WalletPubkey)
			}
			t.Fatalf("%s: the Hub has no record at all, live or archived, of wallet %s — expected an archived bill",
				what, tok.WalletPubkey)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
