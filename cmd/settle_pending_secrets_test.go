package cmd

import (
	"context"
	"testing"

	"github.com/ohstr/nmilat/nipcash"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
)

// withProbes swaps both Hub probes: live answers whether cred spends a bill, and
// amount what that bill answers for (0 when it does not).
func withProbes(t *testing.T, live func(e *ledger.Entry, cred nipcash.Credential) (bool, error), amount func(e *ledger.Entry, cred nipcash.Credential) (uint64, bool, error)) {
	t.Helper()
	origLive, origAmount := probeCredentialFn, probeAmountFn
	t.Cleanup(func() { probeCredentialFn, probeAmountFn = origLive, origAmount })
	probeCredentialFn = func(e *ledger.Entry, cred nipcash.Credential) (bool, error) { return live(e, cred) }
	probeAmountFn = func(e *ledger.Entry, cred nipcash.Credential) (uint64, bool, error) {
		if amount == nil {
			return 0, false, context.DeadlineExceeded
		}
		return amount(e, cred)
	}
}

func noMoves(t *testing.T) {
	t.Helper()
	withMoveFn(t, func(e *ledger.Entry, _ nipcash.Credential, _ nipcash.Target) (*nipcash.CashTransferResult, error) {
		t.Errorf("settling moved %s", e.ID)
		return nil, context.DeadlineExceeded
	})
}

func settleFresh(t *testing.T, l *ledger.Ledger) ledger.Entry {
	t.Helper()
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	fresh, err := ledger.Load()
	if err != nil {
		t.Fatal(err)
	}
	settlePendingMoves(&cobra.Command{}, fresh)
	again, err := ledger.Load()
	if err != nil {
		t.Fatal(err)
	}
	return again.Entries[0]
}

// #97: a protect re-key whose reply was lost, and which did land, is promoted on
// the next command: the new secret, marked protected.
func TestSettlePendingMoves_PromotesAPendingSecretThatLanded(t *testing.T) {
	l := mergeLedger(t)
	l.Entries[0].PendingCashSecret, l.Entries[0].CashProtection = "sn", ledger.CashShared
	noMoves(t)
	withProbes(t, func(_ *ledger.Entry, cred nipcash.Credential) (bool, error) {
		return cred == nipcash.BySecret("sn"), nil
	}, nil)
	e := settleFresh(t, l)
	if e.CashSecret != "sn" || e.PendingCashSecret != "" || e.CashProtection != ledger.CashProtected {
		t.Errorf("got secret %q pending %q protection %q; want sn, none, protected", e.CashSecret, e.PendingCashSecret, e.CashProtection)
	}
}

// One that did not land is dropped: the current secret still spends the bill.
func TestSettlePendingMoves_DropsAPendingSecretThatDidNotLand(t *testing.T) {
	l := mergeLedger(t)
	l.Entries[0].PendingCashSecret, l.Entries[0].CashProtection = "sn", ledger.CashShared
	noMoves(t)
	withProbes(t, func(_ *ledger.Entry, cred nipcash.Credential) (bool, error) {
		return cred == nipcash.BySecret("sa"), nil
	}, nil)
	e := settleFresh(t, l)
	if e.CashSecret != "sa" || e.PendingCashSecret != "" || e.CashProtection != ledger.CashShared {
		t.Errorf("got secret %q pending %q protection %q; want sa, none, shared", e.CashSecret, e.PendingCashSecret, e.CashProtection)
	}
}

// No answer, or neither secret answering, decides nothing: both are kept.
func TestSettlePendingMoves_KeepsAPendingSecretWithoutAnAnswer(t *testing.T) {
	for name, probe := range map[string]func(*ledger.Entry, nipcash.Credential) (bool, error){
		"no answer":       func(*ledger.Entry, nipcash.Credential) (bool, error) { return false, context.DeadlineExceeded },
		"neither answers": func(*ledger.Entry, nipcash.Credential) (bool, error) { return false, nil },
	} {
		t.Run(name, func(t *testing.T) {
			l := mergeLedger(t)
			l.Entries[0].PendingCashSecret = "sn"
			noMoves(t)
			withProbes(t, probe, nil)
			if e := settleFresh(t, l); e.CashSecret != "sa" || e.PendingCashSecret != "sn" {
				t.Errorf("got secret %q pending %q; want both kept", e.CashSecret, e.PendingCashSecret)
			}
		})
	}
}

// A move off a throwaway key onto a cash note whose reply was lost: the note
// may now answer only to the pending secret. Settling must never treat it as a
// leftover and move it again, which would overwrite that secret with a fresh one
// and, once the old key's move is refused, drop the only secret that spends it.
func TestSettlePendingMoves_NeverOverwritesAPendingSecretOnAKeyBill(t *testing.T) {
	l := mergeLedger(t)
	priv, _, _ := newThrowawayKey()
	e0 := &l.Entries[0]
	e0.ThrowawayKey, e0.CashSecret, e0.IdentityRequired, e0.PendingCashSecret = priv, "", ptrTo(true), "sn"
	noMoves(t)
	withProbes(t, func(*ledger.Entry, nipcash.Credential) (bool, error) { return false, context.DeadlineExceeded }, nil)
	if e := settleFresh(t, l); e.PendingCashSecret != "sn" || e.ThrowawayKey != priv {
		t.Errorf("got pending %q key %q; want the pending secret and key kept", e.PendingCashSecret, e.ThrowawayKey)
	}
}

// The same bill, once the Hub answers to the pending secret: an ordinary
// protected cash note, off the key.
func TestSettlePendingMoves_PromotesAKeyBillsLandedMove(t *testing.T) {
	l := mergeLedger(t)
	priv, _, _ := newThrowawayKey()
	e0 := &l.Entries[0]
	e0.ThrowawayKey, e0.CashSecret, e0.IdentityRequired, e0.PendingCashSecret = priv, "", ptrTo(true), "sn"
	noMoves(t)
	withProbes(t, func(_ *ledger.Entry, cred nipcash.Credential) (bool, error) {
		return cred == nipcash.BySecret("sn"), nil
	}, nil)
	e := settleFresh(t, l)
	if e.CashSecret != "sn" || e.PendingCashSecret != "" || e.ThrowawayKey != "" || !isCashNote(&e) || e.CashProtection != ledger.CashProtected {
		t.Errorf("not promoted to a protected cash note: %+v", e)
	}
}

// A destination secret parked for a send is cleared only when the send cannot
// have happened: the source is still held and answers for its full amount.
func TestSettlePendingMoves_ClearsADestinationParkOnlyWhenNothingMoved(t *testing.T) {
	for name, tc := range map[string]struct {
		amount   func(*ledger.Entry, nipcash.Credential) (uint64, bool, error)
		wantKept bool
		notHeld  bool
	}{
		"full amount": {amount: func(_ *ledger.Entry, cred nipcash.Credential) (uint64, bool, error) {
			return 1000, cred == nipcash.BySecret("sa"), nil
		}},
		"split off": {amount: func(*ledger.Entry, nipcash.Credential) (uint64, bool, error) { return 600, true, nil }, wantKept: true},
		"gone":      {amount: func(*ledger.Entry, nipcash.Credential) (uint64, bool, error) { return 0, false, nil }, wantKept: true},
		"no answer": {amount: func(*ledger.Entry, nipcash.Credential) (uint64, bool, error) {
			return 0, false, context.DeadlineExceeded
		}, wantKept: true},
		"source merged": {amount: func(*ledger.Entry, nipcash.Credential) (uint64, bool, error) { return 1000, true, nil }, wantKept: true, notHeld: true},
	} {
		t.Run(name, func(t *testing.T) {
			l := mergeLedger(t)
			l.Entries[0].PendingDestinationCashSecret = "dest"
			if tc.notHeld {
				l.Entries[0].Status = ledger.StatusConsolidated
			}
			noMoves(t)
			withProbes(t, func(*ledger.Entry, nipcash.Credential) (bool, error) { return false, context.DeadlineExceeded }, tc.amount)
			e := settleFresh(t, l)
			if kept := e.PendingDestinationCashSecret == "dest"; kept != tc.wantKept {
				t.Errorf("destination secret kept = %v, want %v", kept, tc.wantKept)
			}
		})
	}
}

// T-06 (B3b): a move off a throwaway key whose answer never came, and which
// did not happen: the key still spends the bill, so the pending secret is
// dropped instead of stranding the note forever.
func TestSettlePendingSecret_ProbesTheThrowawayKey(t *testing.T) {
	h := newFakeBillHub(t)
	l, id, priv := keyBillLedger(t, h)
	e, _ := l.Find(id)
	e.PendingCashSecret = "sn"
	if !settlePendingSecret(e) {
		t.Fatal("settlePendingSecret decided nothing although the key still answers")
	}
	if e.PendingCashSecret != "" || e.ThrowawayKey != priv {
		t.Errorf("pending %q key kept %v; want the pending secret dropped, the key kept", e.PendingCashSecret, e.ThrowawayKey == priv)
	}
	if len(h.probes) == 0 || h.probes[len(h.probes)-1].cred != nipcash.BySigning(priv) {
		t.Error("the throwaway key was never probed")
	}
}
