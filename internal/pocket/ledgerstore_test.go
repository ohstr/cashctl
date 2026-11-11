package pocket

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ohstr/nmilat/nipcash"

	"github.com/ohstr/cashctl/internal/billform"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/testsupport/testmint"
)

func openStore(t *testing.T) *LedgerStore {
	t.Helper()
	priv, _ := testKey(t)
	s, err := OpenLedgerStore(context.Background(), t.TempDir(), priv, "flokicoin")
	if err != nil {
		t.Fatalf("open ledgerstore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestLedgerStoreReloadByteIdentical is the A' core: the embedded in-memory
// ledger.Ledger is mutated through its real methods (Add/AddCall/AddHandoff),
// Save persists it to the NIP-44-sealed faithful tables, and a reopen
// reconstructs the SAME ledger — the cashctl-assigned entry id, the
// Call.SourceEntryIDs→entry linkage (the double-spend hazard the old random ids
// posed), the json:"-" secret, and the handoff all survive, and no secret is
// cleartext on disk.
func TestLedgerStoreReloadByteIdentical(t *testing.T) {
	priv, _ := testKey(t)
	dir := t.TempDir()
	ctx := context.Background()

	s, err := OpenLedgerStore(ctx, dir, priv, "flokicoin")
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	amt := uint64(4200)
	tok := testmint.Bill(t, nipcash.Token{HRP: "lokicash", AttestedAmountMillis: &amt}, "")
	w, err := billform.WalletPubkey(tok)
	if err != nil {
		t.Fatalf("wallet pubkey: %v", err)
	}
	e1, err := s.Add(ledger.Entry{Token: tok, WalletPubkey: w, IdentityPubkey: w, AmountMillis: &amt, Status: ledger.StatusHeld})
	if err != nil {
		t.Fatalf("add entry: %v", err)
	}
	if e1.ID == "" {
		t.Fatal("embedded ledger assigned no id")
	}

	rid, err := nipcash.NewRequestID()
	if err != nil {
		t.Fatalf("request id: %v", err)
	}
	const secret = "sekret-authorizer-xyz"
	if _, err := s.AddCall(ledger.Call{
		RequestID:        rid,
		Method:           "cash_transfer",
		Purpose:          "transfer",
		SourceEntryIDs:   []string{e1.ID}, // the id→linkage A' must preserve
		AuthorizerSecret: secret,          // a json:"-" secret, must survive + be encrypted
		ParamsJSON:       "{}",
	}); err != nil {
		t.Fatalf("add call: %v", err)
	}

	s.AddHandoff(ledger.Handoff{Kind: "cash", Handoff: "note#handoffsecret", AmountMillis: 100, Target: "bob"})

	if err := s.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	s.Close()

	// Reopen the same dir: the in-memory ledger must come back identical.
	s2, err := OpenLedgerStore(ctx, dir, priv, "flokicoin")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	if len(s2.Entries) != 1 {
		t.Fatalf("reloaded %d entries, want 1", len(s2.Entries))
	}
	ge := s2.Entries[0]
	if ge.ID != e1.ID || ge.Token != tok || ge.WalletPubkey != w ||
		ge.AmountMillis == nil || *ge.AmountMillis != amt || ge.Status != ledger.StatusHeld {
		t.Errorf("entry not faithful: %+v (want id=%s token=%s amt=%d)", ge, e1.ID, tok, amt)
	}

	if len(s2.Calls) != 1 {
		t.Fatalf("reloaded %d calls, want 1", len(s2.Calls))
	}
	gc := s2.Calls[0]
	if gc.RequestID != rid || gc.Method != "cash_transfer" || gc.Purpose != "transfer" ||
		gc.AuthorizerSecret != secret || len(gc.SourceEntryIDs) != 1 || gc.SourceEntryIDs[0] != e1.ID {
		t.Errorf("call not faithful: %+v (want rid=%s src=[%s] secret=%s)", gc, rid, e1.ID, secret)
	}

	if len(s2.Handoffs) != 1 || s2.Handoffs[0].Handoff != "note#handoffsecret" {
		t.Errorf("handoff not faithful: %+v", s2.Handoffs)
	}

	// Find still resolves by the preserved id, OpenCallFor by the source linkage.
	if _, ok := s2.Find(e1.ID); !ok {
		t.Errorf("Find(%s): not found after reload", e1.ID)
	}
	if _, ok := s2.OpenCallFor(e1.ID); !ok {
		t.Errorf("OpenCallFor(%s): the source→call linkage was lost", e1.ID)
	}

	// Encrypted at rest: neither secret appears cleartext in the cache file.
	data, err := os.ReadFile(filepath.Join(dir, "pocket-cache.db"))
	if err != nil {
		t.Fatalf("read cache db: %v", err)
	}
	if bytes.Contains(data, []byte(secret)) {
		t.Error("authorizer secret is cleartext on disk (risk 17)")
	}
	if bytes.Contains(data, []byte("handoffsecret")) {
		t.Error("handoff secret is cleartext on disk (risk 17)")
	}
}

// TestLedgerStoreReloadEmpty: a fresh pocket reopens as an empty ledger.
func TestLedgerStoreReloadEmpty(t *testing.T) {
	s := openStore(t)
	if len(s.Entries) != 0 || len(s.Calls) != 0 || len(s.Handoffs) != 0 {
		t.Fatalf("fresh ledger not empty: %d/%d/%d", len(s.Entries), len(s.Calls), len(s.Handoffs))
	}
	if err := s.Save(); err != nil {
		t.Fatalf("save empty: %v", err)
	}
}

// TestLedgerStoreDerivedIDGate exercises the derived-id cross-process lock over
// pocket-cache.db (stage 3a-i), overriding the embedded ledger's cashctl.db one:
// a first insert records the id, a second returns ErrDerivedRecorded, the parent
// and recorded? queries answer, and RemoveDerived frees it to be placed again.
func TestLedgerStoreDerivedIDGate(t *testing.T) {
	s := openStore(t)
	const id, parent = "derived-1", "parent-1"

	if ok, err := s.DerivedRecorded(id); err != nil || ok {
		t.Fatalf("DerivedRecorded before insert = %v,%v want false,nil", ok, err)
	}
	if err := s.InsertDerived(id, parent, nil, 999); err != nil {
		t.Fatalf("first InsertDerived = %v, want nil", err)
	}
	if err := s.InsertDerived(id, parent, nil, 999); !errors.Is(err, ledger.ErrDerivedRecorded) {
		t.Fatalf("second InsertDerived = %v, want ErrDerivedRecorded", err)
	}
	if ok, err := s.DerivedRecorded(id); err != nil || !ok {
		t.Fatalf("DerivedRecorded after insert = %v,%v want true,nil", ok, err)
	}
	if got, err := s.DerivedParent(id); err != nil || got != parent {
		t.Fatalf("DerivedParent = %q,%v want %q,nil", got, err, parent)
	}
	if got, err := s.DerivedParent("absent"); err != nil || got != "" {
		t.Fatalf("DerivedParent(absent) = %q,%v want \"\",nil", got, err)
	}
	if err := s.RemoveDerived(id); err != nil {
		t.Fatalf("RemoveDerived = %v, want nil", err)
	}
	if ok, _ := s.DerivedRecorded(id); ok {
		t.Fatal("DerivedRecorded after remove = true, want false (freed to re-place)")
	}
}
