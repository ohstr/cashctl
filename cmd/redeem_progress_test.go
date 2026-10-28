package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/ohstr/nmilat/nipcash/transport"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// withRedeemProgress answers every own-row read with fn.
func withRedeemProgress(t *testing.T, fn func(e *ledger.Entry) (nipcash.RedeemProgress, *nipcash.Tombstone, error)) {
	t.Helper()
	orig := redeemProgressFn
	t.Cleanup(func() { redeemProgressFn = orig })
	redeemProgressFn = func(_ context.Context, _ *cobra.Command, e *ledger.Entry) (nipcash.RedeemProgress, *nipcash.Tombstone, error) {
		return fn(e)
	}
}

func withInvoiceSettled(t *testing.T, settled bool) {
	t.Helper()
	orig := invoiceSettledFn
	t.Cleanup(func() { invoiceSettledFn = orig })
	invoiceSettledFn = func(context.Context, *cobra.Command, string, string) (bool, error) { return settled, nil }
}

func progress(p nipcash.RedeemProgress, tomb *nipcash.Tombstone, err error) func(*ledger.Entry) (nipcash.RedeemProgress, *nipcash.Tombstone, error) {
	return func(*ledger.Entry) (nipcash.RedeemProgress, *nipcash.Tombstone, error) { return p, tomb, err }
}

// redeemingLedger is mergeLedger with tok-a's redeem left open: claimed, into
// the wallet "savings", over an invoice that expired at invoiceExpiry.
func redeemingLedger(t *testing.T, invoiceExpiry int64) (*ledger.Ledger, string) {
	t.Helper()
	l := mergeLedger(t)
	e := &l.Entries[0]
	withRedeemProgress(t, progress(nipcash.RedeemInFlight, nil, nil))
	inv := testRedeemInvoice(t, invoiceExpiry-600, 600, 9)
	err := resolveUndecidedRedeem(context.Background(), testCmdWithFlags(true, true), l, e, inv, "savings", nil)
	if ce := output.AsCLIError(err); ce == nil || ce.Code != output.CodeIndeterminate || ce.Reason != reasonRedeemInFlight {
		t.Fatalf("in-flight redeem: %v, want exit 8 redeem_in_flight", err)
	}
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	fresh, _ := ledger.Load()
	return fresh, e.ID
}

// T-139: live → redeeming keeps the invoice's hash, expiry and wallet, out of
// every balance and pick.
func TestRedeemProgress_InFlightIsRedeeming(t *testing.T) {
	exp := time.Now().Unix() + 3000
	l, id := redeemingLedger(t, exp)
	e, _ := l.Find(id)
	if e.Status != ledger.StatusRedeeming || e.RedeemPaymentHash == "" || e.RedeemInto != "savings" ||
		e.RedeemInvoiceExpiresAt == nil || *e.RedeemInvoiceExpiresAt != exp {
		t.Fatalf("entry %+v; want redeeming with the invoice's hash, expiry and wallet", e)
	}
	for _, h := range l.Held() {
		if h.ID == id {
			t.Error("a redeeming bill is counted as held")
		}
	}
	if m, n := redeemingTotal(l); n != 1 || m != 1000 {
		t.Errorf("being redeemed %d (%d), want 1000 (1)", m, n)
	}
}

// T-139: every other operation refuses a redeeming bill with redeem_in_flight.
func TestRedeemProgress_RedeemingIsRefusedEverywhere(t *testing.T) {
	l, id := redeemingLedger(t, time.Now().Unix()+3000)
	if ce := output.AsCLIError(refuseLocked(testCmdWithFlags(true, true), l, []string{id})); ce == nil || ce.Reason != reasonRedeemInFlight {
		t.Errorf("refuseLocked: %v, want redeem_in_flight", ce)
	}
	if _, err := ledger.Load(); err != nil {
		t.Fatal(err)
	}
	// The same token received again is not reactivated.
	if _, err := l.Add(ledger.Entry{Token: "tok-a", WalletPubkey: hexWallet("pa")}); !errors.Is(err, ledger.ErrAlreadyHeld) {
		t.Errorf("receiving a redeeming token again: %v, want ErrAlreadyHeld", err)
	}
	cmd := newTestProtectCmd(true, true)
	_ = cmd.Flags().Set("token", id)
	if _, _, err := resolveUnprotectedCashHoldings(cmd, l, nil); output.AsCLIError(err) == nil || output.AsCLIError(err).Reason != reasonRedeemInFlight {
		t.Errorf("protect: %v, want redeem_in_flight", err)
	}
}

// T-139: the edges out of redeeming, as cash status / cash settle read them.
func TestSettleRedeeming_Edges(t *testing.T) {
	cmd := testCmdWithFlags(true, true)
	t.Run("released", func(t *testing.T) {
		l, id := redeemingLedger(t, time.Now().Unix()+3000)
		withRedeemProgress(t, progress(nipcash.RedeemReleased, nil, nil))
		if _, still := settleRedeeming(cmd, l); still != 0 {
			t.Errorf("still %d", still)
		}
		fresh, _ := ledger.Load()
		if e, _ := fresh.Find(id); e.Status != ledger.StatusHeld || e.RedeemPaymentHash != "" {
			t.Errorf("released: %+v, want held and cleared", e)
		}
	})
	t.Run("tombstone", func(t *testing.T) {
		l, id := redeemingLedger(t, time.Now().Unix()+3000)
		withRedeemProgress(t, progress(nipcash.RedeemDone, &nipcash.Tombstone{Status: "spent", Reason: "redeemed"}, nil))
		settleRedeeming(cmd, l)
		fresh, _ := ledger.Load()
		if e, _ := fresh.Find(id); e.Status != ledger.StatusRedeemed {
			t.Errorf("tombstone redeemed: %+v", e)
		}
	})
	t.Run("still in flight", func(t *testing.T) {
		l, id := redeemingLedger(t, time.Now().Unix()+3000)
		withRedeemProgress(t, progress(nipcash.RedeemInFlight, nil, nil))
		withInvoiceSettled(t, false)
		if _, still := settleRedeeming(cmd, l); still != 1 {
			t.Errorf("still %d, want 1", still)
		}
		fresh, _ := ledger.Load()
		if e, _ := fresh.Find(id); e.Status != ledger.StatusRedeeming {
			t.Errorf("%+v, want redeeming", e)
		}
	})
	t.Run("wallet reports it paid", func(t *testing.T) {
		l, id := redeemingLedger(t, time.Now().Unix()+3000)
		withRedeemProgress(t, progress(nipcash.RedeemInFlight, nil, nil))
		withInvoiceSettled(t, true)
		settleRedeeming(cmd, l)
		fresh, _ := ledger.Load()
		if e, _ := fresh.Find(id); e.Status != ledger.StatusRedeemed || e.RedeemPresumedAt != nil {
			t.Errorf("%+v, want redeemed", e)
		}
	})
	t.Run("presumed paid then re-opened", func(t *testing.T) {
		l, id := redeemingLedger(t, time.Now().Unix()-presumedPaidAfter-10)
		withRedeemProgress(t, progress(nipcash.RedeemInFlight, nil, nil))
		withInvoiceSettled(t, false)
		settleRedeeming(cmd, l)
		fresh, _ := ledger.Load()
		e, _ := fresh.Find(id)
		if e.Status != ledger.StatusRedeemed || e.RedeemPresumedAt == nil {
			t.Fatalf("%+v, want presumed paid", e)
		}
		withRedeemProgress(t, progress(nipcash.RedeemReleased, nil, nil))
		settleRedeeming(cmd, fresh)
		again, _ := ledger.Load()
		if e, _ := again.Find(id); e.Status != ledger.StatusHeld || e.RedeemPresumedAt != nil {
			t.Errorf("%+v, want held again once the row shows unclaimed", e)
		}
	})
}

// T-139: a redeem answered INTERNAL is read once, never resent; unclaimed keeps
// the bill live with the code's own error.
func TestApplyRedeemResults_InternalIsUndecided(t *testing.T) {
	l := mergeLedger(t)
	e := &l.Entries[0]
	plans := []redeemPlan{{Index: 0, Entry: e, Invoice: testRedeemInvoice(t, time.Now().Unix(), 600, 3)}}
	outcomes := []redeemOutcome{{EntryID: e.ID}}
	results := []nipcashclient.RedeemOutcome{batchOutcome(e.ID, nipcashclient.OutcomeError, nil,
		&transport.ResultError{Code: "INTERNAL", Message: "payment in flight"})}
	applyRedeemResults(testCmdWithFlags(true, true), l, plans, results, outcomes, "savings", nil)
	if !outcomes[0].undecided || outcomes[0].coded == nil {
		t.Fatalf("INTERNAL outcome %+v, want undecided with its code kept", outcomes[0])
	}
	withRedeemProgress(t, progress(nipcash.RedeemReleased, nil, nil))
	err := resolveUndecidedRedeem(context.Background(), testCmdWithFlags(true, true), l, e, plans[0].Invoice, "savings", outcomes[0].coded)
	if err != outcomes[0].coded || e.Status != ledger.StatusHeld {
		t.Errorf("released after INTERNAL: %v, status %s; want the code's error and the bill live", err, e.Status)
	}
}

// R2-C3 / SG-1: a connection-key spend is known from the entry or from the
// credential given; its unreadable own row is reported as status_unreadable,
// never as spent or not yours.
func TestIsConnectionKeySpend(t *testing.T) {
	cmd := testCmdWithFlags(true, true)
	if !isConnectionKeySpend(cmd, &ledger.Entry{ConnectionKeyPlatform: "nostr"}) {
		t.Error("an entry recorded connection-key-bound is not a connection-key spend")
	}
	if isConnectionKeySpend(cmd, &ledger.Entry{}) {
		t.Error("a plain entry is a connection-key spend")
	}
	t.Setenv(asCredentialEnv, "connection-key:"+strings.Repeat("11", 32)+",nostr,ext,att.json")
	if !isConnectionKeySpend(cmd, &ledger.Entry{}) {
		t.Error("a connection-key credential in CASHCTL_AS is not a connection-key spend")
	}
	if ce := output.AsCLIError(statusUnreadableError(cmd, "tok")); ce == nil || ce.Reason != reasonStatusUnreadable || ce.Code != output.CodeNotFound {
		t.Errorf("statusUnreadableError: %+v", ce)
	}
}

// R2-C3: a redeeming connection-key bill whose row its Hub won't show stays
// redeeming (NOT_FOUND is not spent) and says so; one whose read needs its
// credential says how to give it.
func TestSettleRedeeming_UnreadableRowSaysWhy(t *testing.T) {
	cmd := testCmdWithFlags(true, true)
	l, id := redeemingLedger(t, time.Now().Unix()+3000)
	e, _ := l.Find(id)
	e.ConnectionKeyPlatform = "nostr"
	_ = l.Save()
	withRedeemProgress(t, progress(0, nil, nipcash.ErrClaimNotFound))
	out, still := settleRedeeming(cmd, l)
	if still != 1 || len(out) != 1 || !strings.HasPrefix(out[0].Detail, reasonStatusUnreadable) {
		t.Fatalf("still %d, out %+v: want one redeeming bill reported status_unreadable", still, out)
	}
	fresh, _ := ledger.Load()
	if e, _ := fresh.Find(id); e.Status != ledger.StatusRedeeming {
		t.Errorf("status %s, want still redeeming", e.Status)
	}
	withRedeemProgress(t, progress(0, nil, output.InvocationError(cmd, errors.New("pass --as"))))
	if out, _ := settleRedeeming(cmd, fresh); len(out) != 1 || !strings.Contains(out[0].Detail, "CASHCTL_AS") {
		t.Errorf("out %+v: want the credential hint", out)
	}
}

// R2-C3: an explicit --invoice redeem of a connection-key bill reads its own
// row before sending; a row the Hub won't show refuses it. Other bills are
// not read here.
func TestRequireOwnRow_ConnectionKeyInvoiceNeedsItsRow(t *testing.T) {
	cmd := testCmdWithFlags(true, true)
	orig := ownRowQuoteFn
	t.Cleanup(func() { ownRowQuoteFn = orig })
	read := 0
	ownRowQuoteFn = func(cmd *cobra.Command, _ *ledger.Ledger, e *ledger.Entry, _ *nipcashclient.Client) (redeemQuote, error) {
		read++
		return redeemQuote{}, statusUnreadableError(cmd, e.ID)
	}
	if q, err := requireOwnRow(cmd, nil, &ledger.Entry{ID: "plain"}, nil); q != nil || err != nil || read != 0 {
		t.Errorf("plain bill: %v, %v, read %d: want no read", q, err, read)
	}
	_, err := requireOwnRow(cmd, nil, &ledger.Entry{ID: "ck", ConnectionKeyPlatform: "nostr"}, nil)
	if read != 1 || output.AsCLIError(err).Reason != reasonStatusUnreadable {
		t.Errorf("connection-key bill: %v, read %d: want refused status_unreadable after one read", err, read)
	}
}
