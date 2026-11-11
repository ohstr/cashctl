package pocket

import (
	"context"
	"errors"
	"testing"

	"github.com/ohstr/nmilat/nipcash"

	"github.com/ohstr/cashctl/internal/billform"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/testsupport/testmint"
)

// addBillAndCall adds a held bill and a write-ahead call spending it, returning
// the call's request id. Each uses a distinct amount so the token (and its w) is
// unique.
func addBillAndCall(t *testing.T, s *LedgerStore, amt uint64) string {
	t.Helper()
	tok := testmint.Bill(t, nipcash.Token{HRP: "lokicash", AttestedAmountMillis: &amt}, "")
	w, err := billform.WalletPubkey(tok)
	if err != nil {
		t.Fatalf("wallet pubkey: %v", err)
	}
	e, err := s.Add(ledger.Entry{Token: tok, WalletPubkey: w, IdentityPubkey: w, AmountMillis: &amt, Status: ledger.StatusHeld})
	if err != nil {
		t.Fatalf("add entry: %v", err)
	}
	rid, err := nipcash.NewRequestID()
	if err != nil {
		t.Fatalf("request id: %v", err)
	}
	if _, err := s.AddCall(ledger.Call{
		RequestID:      rid,
		Method:         "cash_transfer",
		Purpose:        "transfer",
		SourceEntryIDs: []string{e.ID},
		ParamsJSON:     "{}",
	}); err != nil {
		t.Fatalf("add call: %v", err)
	}
	return rid
}

// TestLedgerStoreConcurrentSaveRefused is the #14 funds-safety guard: a second
// cashctl process whose store loaded before a concurrent write must have its
// Save REFUSED with ledger.ErrConcurrentUpdate, and must NOT erase the concurrent
// process's written-ahead call (the stranding bug the old ledger's version token
// + refuseForeignOpenCall prevented, dropped by the pocket's blind delete-all).
func TestLedgerStoreConcurrentSaveRefused(t *testing.T) {
	priv, _ := testKey(t)
	dir := t.TempDir()
	ctx := context.Background()

	// A seeds a bill + call and saves → disk store_version advances past 0.
	a, err := OpenLedgerStore(ctx, dir, priv, "flokicoin")
	if err != nil {
		t.Fatalf("open a: %v", err)
	}
	aRID := addBillAndCall(t, a, 1000)
	if err := a.Save(); err != nil {
		t.Fatalf("a save: %v", err)
	}
	a.Close()

	// B and C both open now — both load the SAME current store_version.
	b, err := OpenLedgerStore(ctx, dir, priv, "flokicoin")
	if err != nil {
		t.Fatalf("open b: %v", err)
	}
	defer b.Close()
	c, err := OpenLedgerStore(ctx, dir, priv, "flokicoin")
	if err != nil {
		t.Fatalf("open c: %v", err)
	}
	defer c.Close()

	// B writes-ahead a new call and saves → succeeds (it loaded the current version).
	bRID := addBillAndCall(t, b, 2000)
	if err := b.Save(); err != nil {
		t.Fatalf("b save should succeed: %v", err)
	}

	// C, loaded at the pre-B version, now saves a different change. Its blind
	// delete-all would erase B's written-ahead call — the CAS must refuse it.
	_ = addBillAndCall(t, c, 3000)
	if err := c.Save(); !errors.Is(err, ledger.ErrConcurrentUpdate) {
		t.Fatalf("c stale save: want ErrConcurrentUpdate, got %v", err)
	}

	// Reopen: A's and B's written-ahead calls both survive; C's clobber was refused.
	d, err := OpenLedgerStore(ctx, dir, priv, "flokicoin")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d.Close()
	if _, ok := d.FindCall(ledger.CallIDFor(aRID)); !ok {
		t.Fatal("A's write-ahead call was lost")
	}
	if _, ok := d.FindCall(ledger.CallIDFor(bRID)); !ok {
		t.Fatal("B's write-ahead call was clobbered by C's stale save — the #14 stranding bug is NOT fixed")
	}
}

// TestLedgerStoreSequentialSavesAdvanceVersion confirms the common single-process
// path is unaffected: repeated Saves on one store (write-ahead then apply) each
// advance the version and succeed, and a fresh reopen can keep saving.
func TestLedgerStoreSequentialSavesAdvanceVersion(t *testing.T) {
	priv, _ := testKey(t)
	dir := t.TempDir()
	ctx := context.Background()

	s, err := OpenLedgerStore(ctx, dir, priv, "flokicoin")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = addBillAndCall(t, s, 1000)
	if err := s.Save(); err != nil {
		t.Fatalf("first save: %v", err)
	}
	// A second save on the SAME store (e.g. write-ahead then apply within one
	// command) must succeed — the store advanced its own captured version.
	_ = addBillAndCall(t, s, 2000)
	if err := s.Save(); err != nil {
		t.Fatalf("second save on same store: %v", err)
	}
	s.Close()

	// A later command (fresh open) loads the current version and can still save.
	s2, err := OpenLedgerStore(ctx, dir, priv, "flokicoin")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	_ = addBillAndCall(t, s2, 3000)
	if err := s2.Save(); err != nil {
		t.Fatalf("save after reopen: %v", err)
	}
}
