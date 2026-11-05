package cmd

// CASHCTL-0005 regression, value level: order-mint and consolidate ingestion.
//
// Both sites persisted a Hub-supplied amount_millis with no maxSaneAmountMillis
// ceiling. A value in 2^63..2^64-1 (a valid wire uint64) overflows to a negative
// int64 in summarizeHeldTokens' running total, so `wallet balance` reported a
// NEGATIVE total for the WHOLE wallet off one poisoned bill.
//
// These assert the behaviour the fix adds: an out-of-range order bill is REFUSED
// before l.Add, a consolidate result past the ceiling is DROPPED (the merge has
// already happened, so it degrades to no figure — the cash_transfer.go remainder
// pattern), neither is stored, and the whole-wallet total stays non-negative. The
// structural companion is auditC_ledger_amount_invariant_test.go; the zap half is
// zap_amount_ceiling_test.go.

import (
	"math"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/ohstr/nmilat/nipCO"
	"github.com/ohstr/nmilat/nipcash"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/testsupport/testmint"
)

// amountPastMaxInt64 is 2^63: one past math.MaxInt64, so int64(...) of it is
// negative (math.MinInt64) at the first cast — the exact poison these findings
// describe.
const amountPastMaxInt64 = uint64(math.MaxInt64) + 1

// assertWalletTotalSane reloads the on-disk ledger and fails if the held total
// went negative, or any held line rendered a negative amount — the whole-wallet
// corruption these findings are about.
func assertWalletTotalSane(t *testing.T, note string) {
	t.Helper()
	l, err := ledger.Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	lines, total, _ := summarizeHeldTokens(l.Held(), 0)
	if total < 0 {
		t.Errorf("wallet total = %d after %s, want it non-negative", total, note)
	}
	for _, ln := range lines {
		if ln.AmountMloki < 0 {
			t.Errorf("held line %s renders %d (negative) after %s — an int64 cast overflowed",
				ln.Name, ln.AmountMloki, note)
		}
	}
}

// Order-mint (cash_order.go applyOrderBills) holds each freshly minted bill to the
// maxSaneAmountMillis ceiling before l.Add: a bill past MaxInt64 is refused, so
// nothing is persisted and the whole-wallet total cannot go negative.
func TestIngestCeiling_OrderMint_RefusesAmountPastMaxInt64(t *testing.T) {
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	l, err := ledger.Load()
	if err != nil {
		t.Fatalf("ledger.Load: %v", err)
	}
	priv, xonly := testmint.Key(t)
	wallet := testmint.Hex32(t)
	tok := testmint.Bill(t, nipcash.Token{HRP: "lokicash", WalletPubkey: wallet, Secret: testmint.Hex32(t),
		RelayURLs: []string{"wss://hub.example"}, IdentityRequired: ptrTo(false)}, priv)
	order := &nipCO.Order{State: nipCO.OrderMinted, AmountMillis: amountPastMaxInt64, Bills: []nipCO.OrderBill{
		{WalletPubkey: wallet, Token: nipcash.TokenOrTombstone{Token: tok}, AmountMillis: amountPastMaxInt64},
	}}
	o := &ledger.Order{OrderID: "o-overflow", RecipientKind: "self", HubIdentity: xonly}

	err = applyOrderBills(testCmdWithFlags(true, false), l, &ledger.Call{CallID: "c-overflow"}, o, order)
	if err == nil {
		t.Fatal("applyOrderBills accepted an order bill past MaxInt64 — it would store it " +
			"Verified:true and poison the whole-wallet total")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Fatalf("applyOrderBills error = %v, want the ceiling refusal (not the Hub-identity check)", err)
	}
	if _, total, _ := summarizeHeldTokens(l.Held(), 0); total < 0 {
		t.Errorf("wallet total = %d after a refused order bill, want non-negative", total)
	}
}

// Consolidate's merged-note amount comes from the Hub's reply after the merge has
// already happened, so a figure past the ceiling is dropped (the note degrades to
// no figure) rather than refused — mirroring cash_transfer.go's remainder.
func TestIngestCeiling_Consolidate_DropsAmountPastMaxInt64(t *testing.T) {
	l, ids, sources := consolidateFixture(t)
	// A healthy bystander that stays held: the overflow poisons the whole-wallet
	// total, so the damage was never confined to the merged note.
	bystander := uint64(5000)
	if _, err := l.Add(ledger.Entry{Token: "lokicash1consolidatebystander", AmountMillis: &bystander}); err != nil {
		t.Fatalf("Add bystander: %v", err)
	}
	withFakeAttemptCashConsolidate(t, func(string, []nipcash.Source, nipcash.Target) (*nipcash.CashConsolidateResult, error) {
		return &nipcash.CashConsolidateResult{
			AmountMillis: amountPastMaxInt64, NewWalletToken: "merged-overflow", NewWalletPubkey: hexWallet("merged"),
		}, nil
	})

	newEntry, _, err := doCashConsolidate(&cobra.Command{}, l, []string{"dial-a", "dial-b"}, sources, ids, testTarget(), true)
	if err != nil {
		t.Fatalf("doCashConsolidate: %v", err)
	}
	if newEntry == nil {
		t.Fatal("newEntry = nil, want the merged note still recorded")
	}
	if newEntry.AmountMillis != nil {
		t.Errorf("merged amount = %d, want nil (a figure past MaxInt64 is dropped, not stored)", *newEntry.AmountMillis)
	}
	if err := l.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	assertWalletTotalSane(t, "a consolidate result past MaxInt64")
}
