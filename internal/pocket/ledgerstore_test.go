package pocket

import (
	"context"
	"errors"
	"testing"

	"github.com/ohstr/nmilat/nipCP"
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
	t.Cleanup(s.Close)
	return s
}

// TestLedgerStoreAddReadRoundTrip is the core: a real held bill added through the
// adapter comes back out of the held-bill reads with its funds-relevant fields
// intact (token, wallet pubkey, amount, status, identity).
func TestLedgerStoreAddReadRoundTrip(t *testing.T) {
	s := openStore(t)
	amt := uint64(4200)
	tokStr := testmint.Bill(t, nipcash.Token{HRP: "lokicash", AttestedAmountMillis: &amt}, "")
	w, err := billform.WalletPubkey(tokStr)
	if err != nil {
		t.Fatalf("wallet pubkey: %v", err)
	}

	in := ledger.Entry{Token: tokStr, WalletPubkey: w, IdentityPubkey: w}
	if _, err := s.Add(in); err != nil {
		t.Fatalf("add: %v", err)
	}

	got, ok := s.FindByToken(tokStr)
	if !ok {
		t.Fatal("FindByToken: not found after Add")
	}
	if got.Token != tokStr {
		t.Errorf("token = %q, want %q", got.Token, tokStr)
	}
	if got.WalletPubkey != w {
		t.Errorf("wallet pubkey = %q, want %q", got.WalletPubkey, w)
	}
	if got.AmountMillis == nil || *got.AmountMillis != amt {
		t.Errorf("amount = %v, want %d", got.AmountMillis, amt)
	}
	if got.Status != ledger.StatusHeld {
		t.Errorf("status = %q, want held", got.Status)
	}
	if got.IdentityPubkey != w {
		t.Errorf("identity pubkey = %q, want %q", got.IdentityPubkey, w)
	}

	if held := s.Held(); len(held) != 1 {
		t.Fatalf("Held() = %d entries, want 1", len(held))
	}
	if _, ok := s.Find(ledgerID(w)); !ok {
		t.Errorf("Find(%q): not found", ledgerID(w))
	}
}

func TestLedgerStoreStatusMapping(t *testing.T) {
	cases := []struct {
		state  nipCP.State
		status string
	}{
		{nipCP.StateLive, ledger.StatusHeld},
		{nipCP.StateSpent, ledger.StatusSpent},
		{nipCP.StateExpired, ledger.StatusExpired},
		{nipCP.StateReclaimed, ledger.StatusReclaimed},
	}
	for _, c := range cases {
		if got := statusFromState(c.state); got != c.status {
			t.Errorf("statusFromState(%s) = %q, want %q", c.state, got, c.status)
		}
	}
	// A held status round-trips to live.
	if got := stateFromStatus(ledger.StatusHeld); got != nipCP.StateLive {
		t.Errorf("stateFromStatus(held) = %s, want live", got)
	}
	// The spending statuses map to spent.
	for _, st := range []string{ledger.StatusTransferred, ledger.StatusConsolidated, ledger.StatusWithdrawn, ledger.StatusSpent} {
		if got := stateFromStatus(st); got != nipCP.StateSpent {
			t.Errorf("stateFromStatus(%s) = %s, want spent", st, got)
		}
	}
}

func TestLedgerStoreToLedgerEntry(t *testing.T) {
	s := &LedgerStore{}
	exp := int64(1790000000)
	mint := "02" + "ab" + "cd" // not validated by toLedgerEntry; carried through
	e := nipCP.Entry{
		W: "aa11", Amount: 7, State: nipCP.StateLive, Cash: "lokicash1xyz",
		Identity: nipCP.EntryIdentity{Type: nipCP.IdentityPubkey, Value: "bb22"},
		ExpiresAt: &exp, Mint: mint,
	}
	le := s.toLedgerEntry(e)
	if le.ID != ledgerID("aa11") || le.Token != "lokicash1xyz" || le.WalletPubkey != "aa11" {
		t.Errorf("basic fields: %+v", le)
	}
	if le.AmountMillis == nil || *le.AmountMillis != 7 {
		t.Errorf("amount = %v", le.AmountMillis)
	}
	if le.Status != ledger.StatusHeld || le.IdentityPubkey != "bb22" {
		t.Errorf("status/identity: %q %q", le.Status, le.IdentityPubkey)
	}
	if le.ExpiresAt == nil || *le.ExpiresAt != exp {
		t.Errorf("expires = %v", le.ExpiresAt)
	}
	if le.MinterPubkey == nil || *le.MinterPubkey != mint {
		t.Errorf("minter = %v", le.MinterPubkey)
	}
	// Verified now follows the token's Hub-signed provenance (finding fix), not
	// an unconditional true. This synthetic token carries none, so it maps
	// unverified — a fresh/remainder bill must not be reported as Hub-confirmed.
	if le.Verified {
		t.Error("an unverifiable token must map Verified=false, not an assumed true")
	}
}

func TestLedgerStoreLedgerIDStable(t *testing.T) {
	w := "deadbeefcafe"
	if a, b := ledgerID(w), ledgerID(w); a != b {
		t.Errorf("ledgerID not stable: %q vs %q", a, b)
	}
	if ledgerID("ab") == ledgerID("cd") {
		t.Error("distinct w should give distinct ids")
	}
}

// TestLedgerStoreCallsSeparatedFromBills verifies AllEntries excludes pending-
// call rows and AllCalls returns them, mapped.
func TestLedgerStoreCallsSeparatedFromBills(t *testing.T) {
	s := openStore(t)
	// A live bill, folded straight into the cache.
	bill := nipCP.Entry{
		W: "aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11a",
		Rev: 1, State: nipCP.StateLive, CheckedAt: 1, Amount: 9, Chain: "flokicoin",
		Identity: nipCP.EntryIdentity{Type: nipCP.IdentityPubkey, Value: "aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11a"},
		Cash: "lokicash1bill",
	}
	if _, err := s.p.Cache.Fold(bill); err != nil {
		t.Fatalf("fold bill: %v", err)
	}
	// A pending call (IsCall via RequestID == W).
	call := nipCP.Entry{
		W: "req0", Rev: 1, State: nipCP.StatePending, CheckedAt: 1, Amount: 5, Chain: "flokicoin",
		RequestID: "req0", Deadline: 123, Target: "tgt", Device: "dev",
	}
	if _, err := s.p.Cache.Fold(call); err != nil {
		t.Fatalf("fold call: %v", err)
	}

	bills := s.AllEntries()
	if len(bills) != 1 || bills[0].Token != "lokicash1bill" {
		t.Fatalf("AllEntries = %+v, want only the bill", bills)
	}
	calls := s.AllCalls()
	if len(calls) != 1 {
		t.Fatalf("AllCalls = %d, want 1", len(calls))
	}
	// Only the cache-projected fields are populated here; Deadline/Target/Device
	// live in the sealed wrap (re-Opened by the settle-family migration), so a
	// cache-sourced call carries them empty — see toLedgerCall's full mapping in
	// TestToLedgerCall.
	c := calls[0]
	if c.RequestID != "req0" || c.CallID != ledger.CallIDFor("req0") ||
		c.AmountMillis != 5 || c.Chain != "flokicoin" {
		t.Errorf("call mapping (projected fields): %+v", c)
	}
	if _, ok := s.FindCall(ledger.CallIDFor("req0")); !ok {
		t.Error("FindCall: not found")
	}
}

// TestToLedgerCall checks the full pending-entry → ledger.Call mapping (as the
// settle family will feed it from a re-Opened wrap, with every field present).
func TestToLedgerCall(t *testing.T) {
	e := nipCP.Entry{
		W: "req0", RequestID: "req0", State: nipCP.StatePending,
		Deadline: 123, Target: "tgt", Device: "dev", Amount: 5, Chain: "flokicoin",
	}
	c := toLedgerCall(e)
	if c.CallID != ledger.CallIDFor("req0") || c.RequestID != "req0" || c.Deadline != 123 ||
		c.Target != "tgt" || c.DeviceID != "dev" || c.AmountMillis != 5 || c.Chain != "flokicoin" ||
		c.State != ledger.CallWritten {
		t.Errorf("toLedgerCall: %+v", c)
	}
	done := toLedgerCall(nipCP.Entry{W: "r", RequestID: "r", State: nipCP.StateDone})
	if done.State != ledger.CallDone {
		t.Errorf("done state = %q, want %q", done.State, ledger.CallDone)
	}
}

func TestLedgerStoreSaveNoop(t *testing.T) {
	s := openStore(t)
	if err := s.Save(); err != nil {
		t.Errorf("Save() = %v, want nil (pocket writes are immediate)", err)
	}
}

// TestLedgerStoreStubsPanic confirms the funds-mutating stubs fail loudly (never
// a silent wrong answer), so a premature stage-3 wiring is caught.
func TestLedgerStoreStubsPanic(t *testing.T) {
	s := &LedgerStore{}
	mustPanic(t, "SetStatus", func() { _ = s.SetStatus("id", "spent") })
	mustPanic(t, "AddCall", func() { _, _ = s.AddCall(ledger.Call{}) })
	mustPanic(t, "AddHandoff", func() { s.AddHandoff(ledger.Handoff{}) })
	mustPanic(t, "StageOrder", func() { s.StageOrder(ledger.Order{}) })
}

func mustPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s did not panic (stubs must fail loudly)", name)
		}
	}()
	fn()
}

// TestLedgerStoreDerivedIDGate exercises the derived-id cross-process lock over
// pocket-cache.db (stage 3a-i): a first insert records the id, a second returns
// ErrDerivedRecorded, the parent and recorded? queries answer, and RemoveDerived
// frees it to be placed again.
func TestLedgerStoreDerivedIDGate(t *testing.T) {
	s := openStore(t)
	const id, parent = "derived-1", "parent-1"

	if ok, err := s.DerivedRecorded(id); err != nil || ok {
		t.Fatalf("DerivedRecorded before insert = %v,%v want false,nil", ok, err)
	}
	if err := s.InsertDerived(id, parent, nil, 999); err != nil {
		t.Fatalf("first InsertDerived = %v, want nil", err)
	}
	// The lock: a second insert of the same id must report already-recorded.
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
