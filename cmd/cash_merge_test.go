package cmd

import (
	"context"
	"testing"

	"github.com/ohstr/nmilat/nipcash"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/ledger"
)

// mergeLedger is a fresh on-disk ledger holding cash notes a, b and a pubkey bill.
func mergeLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	l, err := ledger.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []ledger.Entry{
		{Token: "tok-a", WalletPubkey: "pa", AmountMillis: ptrTo(uint64(1000)), IdentityRequired: ptrTo(false), CashSecret: "sa"},
		{Token: "tok-b", WalletPubkey: "pb", AmountMillis: ptrTo(uint64(2000)), IdentityRequired: ptrTo(false), CashSecret: "sb"},
		{Token: "tok-p", WalletPubkey: "pp", AmountMillis: ptrTo(uint64(3000)), IdentityRequired: ptrTo(true)},
	} {
		if _, err := l.Add(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	return l
}

func withMoveFn(t *testing.T, fn func(*ledger.Entry, nipcash.Credential, nipcash.Target) (*nipcash.CashTransferResult, error)) {
	t.Helper()
	orig := moveCashFn
	moveCashFn = fn
	t.Cleanup(func() { moveCashFn = orig })
}

func ids(l *ledger.Ledger) []string {
	var out []string
	for _, e := range l.Entries {
		out = append(out, e.ID)
	}
	return out
}

// I-26: each cash note is moved onto the key with its own secret; once moved,
// the key spends it, and the pubkey bill is left alone.
func TestMoveCashNotesToKey_MovesOnlyCashNotes(t *testing.T) {
	l := mergeLedger(t)
	priv, pub, _ := newThrowawayKey()
	var creds []nipcash.Credential
	withMoveFn(t, func(e *ledger.Entry, cred nipcash.Credential, _ nipcash.Target) (*nipcash.CashTransferResult, error) {
		if e.PendingThrowawayKey != priv {
			t.Errorf("%s moved before its key was written down", e.ID)
		}
		creds = append(creds, cred)
		return &nipcash.CashTransferResult{}, nil
	})
	moved, err := moveCashNotesToKey(&cobra.Command{}, l, ids(l), priv, pub)
	if err != nil || len(moved) != 2 || len(creds) != 2 {
		t.Fatalf("moved %v (%d calls), err %v; want both cash notes", moved, len(creds), err)
	}
	fresh, _ := ledger.Load()
	for _, e := range fresh.Entries[:2] {
		if e.ThrowawayKey != priv || e.CashSecret != "" || e.PendingThrowawayKey != "" {
			t.Errorf("%s not recorded as moved: %+v", e.ID, e)
		}
		if cred, _ := resolveCredential(&cobra.Command{}, &e); cred == nil {
			t.Errorf("%s has no credential after the move", e.ID)
		}
	}
	if fresh.Entries[2].ThrowawayKey != "" {
		t.Error("the pubkey bill was moved")
	}
}

// A refusal on the second note moves the first back to a fresh cash note.
func TestMoveCashNotesToKey_RefusalUndoesEarlierMoves(t *testing.T) {
	l := mergeLedger(t)
	priv, pub, _ := newThrowawayKey()
	calls := 0
	withMoveFn(t, func(e *ledger.Entry, cred nipcash.Credential, target nipcash.Target) (*nipcash.CashTransferResult, error) {
		calls++
		if calls == 2 {
			return nil, &relayclient.WalletError{Code: "RESTRICTED", Message: "no"}
		}
		return &nipcash.CashTransferResult{}, nil
	})
	if _, err := moveCashNotesToKey(&cobra.Command{}, l, ids(l), priv, pub); err == nil {
		t.Fatal("a refused move reported success")
	}
	fresh, _ := ledger.Load()
	a, b := fresh.Entries[0], fresh.Entries[1]
	if calls != 3 {
		t.Errorf("moves placed = %d, want 3 (a, b refused, a moved back)", calls)
	}
	if a.ThrowawayKey != "" || a.CashSecret == "" || a.CashSecret == "sa" || !isCashNote(&a) {
		t.Errorf("the moved note is not a fresh cash note again: %+v", a)
	}
	if b.PendingThrowawayKey != "" || b.CashSecret != "sb" {
		t.Errorf("the refused note changed: %+v", b)
	}
}

// No answer: the key stays pending, and a later run settles it from what the
// Hub accepts — never by resending.
func TestMovesWithNoAnswer_AreSettledNotResent(t *testing.T) {
	l := mergeLedger(t)
	priv, pub, _ := newThrowawayKey()
	withMoveFn(t, func(*ledger.Entry, nipcash.Credential, nipcash.Target) (*nipcash.CashTransferResult, error) {
		return nil, context.DeadlineExceeded
	})
	if _, err := moveCashNotesToKey(&cobra.Command{}, l, ids(l), priv, pub); err == nil {
		t.Fatal("an unanswered move reported success")
	}
	fresh, _ := ledger.Load()
	if fresh.Entries[0].PendingThrowawayKey != priv {
		t.Fatalf("the pending key was dropped on no answer: %+v", fresh.Entries[0])
	}

	orig := probeCredentialFn
	t.Cleanup(func() { probeCredentialFn = orig })
	probeCredentialFn = func(e *ledger.Entry, cred nipcash.Credential, _ string) (bool, error) {
		return cred == nipcash.BySigning(priv), nil // the move did happen
	}
	withMoveFn(t, func(*ledger.Entry, nipcash.Credential, nipcash.Target) (*nipcash.CashTransferResult, error) {
		t.Error("settling resent a move")
		return nil, nil
	})
	settlePendingMoves(fresh)
	again, _ := ledger.Load()
	if again.Entries[0].ThrowawayKey != priv || again.Entries[0].PendingThrowawayKey != "" {
		t.Errorf("a confirmed move was not promoted: %+v", again.Entries[0])
	}
	if n, _ := pendingMerge(again); n != 1 {
		t.Errorf("pendingMerge = %d, want the one moved note", n)
	}
}
