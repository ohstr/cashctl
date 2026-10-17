package cmd

import (
	"encoding/hex"
	"errors"
	"testing"

	"github.com/ohstr/nmilat/nipcash"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/ledger"
)

// After a consolidate reply nobody could read, only a
// source whose Hub answers with a spent tombstone is written off as merged.
// A live roster, an error, or an undecodable token leaves the entry held.
func TestReconcileAmbiguousSources_OnlyATombstoneMarksASourceGone(t *testing.T) {
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	l, err := ledger.Load()
	if err != nil {
		t.Fatal(err)
	}
	add := func(pubByte byte) string {
		amt := uint64(1000)
		tok := rawBech32(t, "lokicash", []tlv{{0, n(pubByte, 32)}, tlvRelay, tlvSecret})
		e, err := l.Add(ledger.Entry{Token: tok, WalletPubkey: hex.EncodeToString(n(pubByte, 32)), AmountMillis: &amt})
		if err != nil {
			t.Fatal(err)
		}
		return e.ID
	}
	spent, live, failed := add(0x01), add(0x02), add(0x03)
	bad, err := l.Add(ledger.Entry{Token: "not-a-token", WalletPubkey: "bad"})
	if err != nil {
		t.Fatal(err)
	}

	answers := map[string]func() (*nipcash.CashStatusResult, error){
		spent: func() (*nipcash.CashStatusResult, error) {
			return &nipcash.CashStatusResult{Error: nipcash.ErrorSpent}, nil
		},
		live:   func() (*nipcash.CashStatusResult, error) { return &nipcash.CashStatusResult{}, nil },
		failed: func() (*nipcash.CashStatusResult, error) { return nil, errors.New("timeout") },
		bad.ID: func() (*nipcash.CashStatusResult, error) {
			return &nipcash.CashStatusResult{Error: nipcash.ErrorSpent}, nil
		},
	}
	real := sourceCashStatusFn
	t.Cleanup(func() { sourceCashStatusFn = real })
	sourceCashStatusFn = func(_ *cobra.Command, e *ledger.Entry) (*nipcash.CashStatusResult, error) {
		return answers[e.ID]()
	}

	gone := reconcileAmbiguousSources(&cobra.Command{}, l, []string{spent, live, failed, bad.ID})
	if len(gone) != 1 || gone[0] != spent {
		t.Fatalf("confirmed gone = %v, want only the tombstoned source %s", gone, spent)
	}
	fresh, err := ledger.Load()
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{
		spent: ledger.StatusConsolidated, live: ledger.StatusHeld, failed: ledger.StatusHeld, bad.ID: ledger.StatusHeld,
	} {
		if e, _ := fresh.Find(id); e.Status != want {
			t.Errorf("%s: saved status = %q, want %q", id, e.Status, want)
		}
	}
}
