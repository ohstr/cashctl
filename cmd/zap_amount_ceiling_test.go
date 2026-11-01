package cmd

// CASHCTL-0006 regression, value level: zap-receive and zap-claim ingestion.
//
// Both sites persisted a Hub/minter-supplied amount with no maxSaneAmountMillis
// ceiling. A zap `amount` tag in 2^63..2^64-1 (a valid wire uint64) overflows to
// a negative int64 in summarizeHeldTokens' running total, so `wallet balance`
// reported a NEGATIVE total for the WHOLE wallet off one poisoned zap.
//
// Shares amountPastMaxInt64 and assertWalletTotalSane with
// amount_ceiling_ingest_test.go (the CASHCTL-0005 half).

import (
	"testing"

	"github.com/ohstr/nmilat/nipcash"

	"github.com/ohstr/cashctl/internal/ledger"
)

// Zap-receive (zap_verify.go receiveZap) now passes the minter-supplied amount
// through receive's own validateClaimedAmount before l.Add, so an out-of-range
// figure is refused outright rather than stored Verified:true.
func TestIngestCeiling_ZapReceive_RefusesOverflow(t *testing.T) {
	c := testCmdWithFlags(true, false)
	minter := "minterpubkey"
	if err := validateClaimedAmount(c, nipcash.Token{}, amountPastMaxInt64, &minter); err == nil {
		t.Fatal("validateClaimedAmount accepted an amount past MaxInt64 — zap-receive " +
			"would store it Verified:true and poison the whole-wallet total")
	}
}

// Zap-claim (zap_verify.go applyZapClaimResult) records what a connection-key
// claim's cash_transfer did; those reply amounts (the moved amount, and any
// remainder) are Hub-supplied and were stored unbounded. The transfer has
// happened, so an out-of-range figure is dropped, in both reply shapes.
func TestIngestCeiling_ZapClaim_DropsAmountPastMaxInt64(t *testing.T) {
	l := auditCLedger(t)
	bystander := uint64(5000)
	if _, err := l.Add(ledger.Entry{Token: "lokicash1zapbystander", AmountMillis: &bystander}); err != nil {
		t.Fatalf("Add bystander: %v", err)
	}

	// In-place: the Hub answers with an in-place move for a figure past the ceiling
	// that is not the zap amount, so it is not a valid zap and must not be stored.
	inPlace, err := l.Add(ledger.Entry{Token: "zap-inplace", WalletPubkey: hexWallet("zi"), IdentityRequired: ptrTo(true)})
	if err != nil {
		t.Fatalf("Add in-place target: %v", err)
	}
	if _, notZap := applyZapClaimResult(l, inPlace, 1500, &nipcash.CashTransferResult{AmountMillis: amountPastMaxInt64}); notZap == "" {
		t.Error("want a notZap note: an in-place move that is not the zap amount is not a valid zap")
	}
	if inPlace.AmountMillis != nil {
		t.Errorf("in-place entry amount = %d, want nil (a figure past MaxInt64 is dropped, not stored)", *inPlace.AmountMillis)
	}

	// Split: a spin-off bill plus a remainder, both past the ceiling.
	split, err := l.Add(ledger.Entry{Token: "zap-split", WalletPubkey: hexWallet("zs"), IdentityRequired: ptrTo(true)})
	if err != nil {
		t.Fatalf("Add split target: %v", err)
	}
	rem := amountPastMaxInt64
	applyZapClaimResult(l, split, 1500, &nipcash.CashTransferResult{
		AmountMillis:          amountPastMaxInt64,
		NewWalletToken:        "zap-spinoff",
		NewWalletPubkey:       hexWallet("spin"),
		RemainderWalletToken:  "zap-remainder",
		RemainderWalletPubkey: hexWallet("rem"),
		RemainingAmountMillis: &rem,
	})

	if err := l.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	assertWalletTotalSane(t, "a zap claim result past MaxInt64")
}
