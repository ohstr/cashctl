package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nipcash"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/credential"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
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
		{Token: "tok-a", WalletPubkey: hexWallet("pa"), AmountMillis: ptrTo(uint64(1000)), IdentityRequired: ptrTo(false), CashSecret: "sa"},
		{Token: "tok-b", WalletPubkey: hexWallet("pb"), AmountMillis: ptrTo(uint64(2000)), IdentityRequired: ptrTo(false), CashSecret: "sb"},
		{Token: "tok-p", WalletPubkey: hexWallet("pp"), AmountMillis: ptrTo(uint64(3000)), IdentityRequired: ptrTo(true)},
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
	hooks := installFakeCaller(t)
	hooks.transfer = func(e *ledger.Entry, p nipcash.CashTransferParams) (*nipcash.CashTransferResult, error) {
		return fn(e, p.Credential, p.To)
	}
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
			return nil, &relayclient.WalletError{Code: "BAD_REQUEST", Message: "no"}
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

// Moving a note back to cash after a refused merge spins it off: its wallet now
// has had two recipients, so the Hub moves the value to a new wallet and
// tombstones the old slice. The entry must follow it, or the note's value is
// held on the Hub while the ledger tracks a spent bill.
func TestMoveCashNotesToKey_RefusedMergeFollowsTheSpunOffNote(t *testing.T) {
	l := mergeLedger(t)
	priv, pub, _ := newThrowawayKey()
	calls := 0
	withMoveFn(t, func(e *ledger.Entry, _ nipcash.Credential, target nipcash.Target) (*nipcash.CashTransferResult, error) {
		calls++
		switch {
		case calls == 2:
			return nil, &relayclient.WalletError{Code: "BAD_REQUEST", Message: "no"}
		case !nipcash.IsPubkeyTarget(target):
			return &nipcash.CashTransferResult{NewWalletPubkey: hexWallet("pa2"), NewWalletToken: "tok-a2"}, nil
		}
		return &nipcash.CashTransferResult{}, nil
	})
	if _, err := moveCashNotesToKey(&cobra.Command{}, l, ids(l), priv, pub); err == nil {
		t.Fatal("a refused move reported success")
	}
	fresh, _ := ledger.Load()
	a := fresh.Entries[0]
	if a.Token != "tok-a2" || a.WalletPubkey != hexWallet("pa2") || !isCashNote(&a) || a.Status != ledger.StatusHeld {
		t.Errorf("the spun-off note is not tracked at its new wallet: %+v", a)
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
	probeCredentialFn = func(e *ledger.Entry, cred nipcash.Credential) (bool, error) {
		return cred == nipcash.BySigning(priv), nil // the move did happen
	}
	// Settling may move the note on (back to a fresh cash note, as a leftover), but
	// never resends the original move, which the note's old secret authorized.
	withMoveFn(t, func(_ *ledger.Entry, cred nipcash.Credential, _ nipcash.Target) (*nipcash.CashTransferResult, error) {
		if cred == nipcash.BySecret("sa") {
			t.Error("settling resent the original move")
		}
		if cred != nipcash.BySigning(priv) {
			t.Errorf("the leftover was spent with %v, want the promoted key", cred)
		}
		return &nipcash.CashTransferResult{}, nil
	})
	settlePendingMoves(&cobra.Command{}, fresh)
	again, _ := ledger.Load()
	a := again.Entries[0]
	if a.PendingThrowawayKey != "" || a.ThrowawayKey != "" || !isCashNote(&a) || a.CashSecret == "sa" {
		t.Errorf("the confirmed move was not promoted and turned into a fresh cash note: %+v", a)
	}
	if n, _ := pendingMerge(again); n != 0 {
		t.Errorf("pendingMerge = %d, want 0 once nothing is left on a key", n)
	}
}

// Whatever a merge leaves on a throwaway key — a split's change, an interim bill —
// is turned back into a fresh cash note the next time a command starts, and is
// then no longer reported as a merge left midway.
func TestSettlePendingMoves_TurnsLeftoversIntoCashNotes(t *testing.T) {
	l := mergeLedger(t)
	priv, _, _ := newThrowawayKey()
	l.Entries[0].IdentityRequired, l.Entries[0].CashSecret, l.Entries[0].ThrowawayKey = ptrTo(true), "", priv
	l.Entries[1].PendingThrowawayKey = priv // a move still unsettled: not a leftover
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	orig := probeCredentialFn
	t.Cleanup(func() { probeCredentialFn = orig })
	probeCredentialFn = func(*ledger.Entry, nipcash.Credential) (bool, error) { return false, context.DeadlineExceeded }
	var spentWith []nipcash.Credential
	withMoveFn(t, func(_ *ledger.Entry, cred nipcash.Credential, target nipcash.Target) (*nipcash.CashTransferResult, error) {
		if _, ok := target.(*nipcash.CashTarget); !ok {
			t.Errorf("a leftover was moved to %T, want a fresh cash note", target)
		}
		spentWith = append(spentWith, cred)
		return &nipcash.CashTransferResult{}, nil
	})
	settlePendingMoves(&cobra.Command{}, l)
	fresh, _ := ledger.Load()
	a := fresh.Entries[0]
	if len(spentWith) != 1 || spentWith[0] != nipcash.BySigning(priv) {
		t.Errorf("moves = %v, want exactly the leftover, spent with its key", spentWith)
	}
	if !isCashNote(&a) || a.ThrowawayKey != "" {
		t.Errorf("the leftover is not a cash note again: %+v", a)
	}
	if a.CashProtection != ledger.CashProtected {
		t.Errorf("the settled cash note's protection = %q, want %q: its secret was generated here", a.CashProtection, ledger.CashProtected)
	}
	if n, _ := pendingMerge(fresh); n != 1 {
		t.Errorf("pendingMerge = %d, want only the unsettled move", n)
	}
}

// movedLedger is a ledger holding two notes already moved onto priv's key, the
// sources for them, and fakes for every Hub call mergeThenSend makes.
func movedLedger(t *testing.T) (l *ledger.Ledger, priv, pub string, tokens, ids []string, sources []nipcash.Source) {
	t.Helper()
	l = mergeLedger(t)
	priv, pub, _ = newThrowawayKey()
	for i := range l.Entries[:2] {
		e := &l.Entries[i]
		e.ThrowawayKey, e.CashSecret, e.IdentityRequired = priv, "", ptrTo(true)
		tokens, ids = append(tokens, e.Token), append(ids, e.ID)
		sources = append(sources, nipcash.Source{WalletPubkey: e.WalletPubkey, Amount: *e.AmountMillis, Credential: nipcash.BySigning(priv)})
	}
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	origCap := hubConsolidateCapFn
	hubConsolidateCapFn = func(string) int { return 48 }
	t.Cleanup(func() { hubConsolidateCapFn = origCap })
	withFakeAttemptCashConsolidate(t, func(string, []nipcash.Source, nipcash.Target) (*nipcash.CashConsolidateResult, error) {
		return &nipcash.CashConsolidateResult{AmountMillis: 3000, NewWalletToken: "tok-merged", NewWalletPubkey: hexWallet("pm")}, nil
	})
	return l, priv, pub, tokens, ids, sources
}

func mergedEntry(t *testing.T) *ledger.Entry {
	t.Helper()
	fresh, err := ledger.Load()
	if err != nil {
		t.Fatal(err)
	}
	for i := range fresh.Entries {
		if fresh.Entries[i].Token == "tok-merged" {
			return &fresh.Entries[i]
		}
	}
	return nil
}

// I-26 overshoot: the merged bill is saved, with its key, BEFORE the send leg, so
// a refused or unanswered send leaves a recoverable bill: refused (BAD_REQUEST,
// final) turns it into a cash note at once; unanswered keeps its call open, the
// bill held on its key, and it is not spent again until that call is settled.
func TestMergeThenSend_TheMergedBillSurvivesAFailedSend(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sendErr error
	}{
		{"refused", &relayclient.WalletError{Code: "BAD_REQUEST", Message: "no"}},
		{"unanswered", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, priv, pub, tokens, ids, sources := movedLedger(t)
			installFakeCaller(t).transfer = func(e *ledger.Entry, p nipcash.CashTransferParams) (*nipcash.CashTransferResult, error) {
				if p.SplitAmount == nil {
					return &nipcash.CashTransferResult{}, nil // a leftover turned back into a cash note
				}
				if m := mergedEntry(t); m == nil || m.ThrowawayKey != priv {
					t.Errorf("the send leg ran before the merged bill was saved with its key: %+v", m)
				}
				return nil, tc.sendErr
			}

			_, rHex, _ := newThrowawayKey()
			target := credential.ResolvedTarget{Target: nipcash.Pubkey(rHex)}
			var err error
			withCapturedOutput(func() {
				err = mergeThenSend(testCmdWithFlags(true, true), l, tokens, sources, ids, 3000, 1000, priv, pub, "r", target)
			})
			if err == nil {
				t.Fatal("a failed send reported success")
			}
			m := mergedEntry(t)
			if m == nil || m.Status != ledger.StatusHeld {
				t.Fatalf("the merged bill is not saved and held: %+v", m)
			}
			fresh, _ := ledger.Load()
			if tc.name == "refused" {
				if !isCashNote(m) {
					t.Errorf("a refused send left the merged bill on the key: %+v", m)
				}
				return
			}
			if m.ThrowawayKey != priv {
				t.Errorf("an unanswered send's bill left its key: %+v", m)
			}
			if c, open := fresh.OpenCallFor(m.ID); !open || c.Purpose != purposeSplit {
				t.Errorf("an unanswered send has no open call on the merged bill: %+v", c)
			}
			settlePendingMoves(testCmdWithFlags(true, true), fresh)
			if m := mergedEntry(t); m.ThrowawayKey != priv {
				t.Errorf("the next command moved a bill whose send is still open: %+v", m)
			}
		})
	}
}

// I-26 exact sum: one cash_consolidate straight to the recipient, no send leg,
// and the handoff kept (I-27).
func TestMergeThenSend_ExactSumConsolidatesStraightToTheRecipient(t *testing.T) {
	l, priv, pub, tokens, ids, sources := movedLedger(t)
	_, rHex, _ := newThrowawayKey()
	recipient := nipcash.Pubkey(rHex)
	var gotTarget nipcash.Target
	withFakeAttemptCashConsolidate(t, func(_ string, _ []nipcash.Source, target nipcash.Target) (*nipcash.CashConsolidateResult, error) {
		gotTarget = target
		return &nipcash.CashConsolidateResult{AmountMillis: 3000, NewWalletToken: "tok-merged", NewWalletPubkey: hexWallet("pm")}, nil
	})
	installFakeCaller(t).transfer = func(*ledger.Entry, nipcash.CashTransferParams) (*nipcash.CashTransferResult, error) {
		t.Error("an exact sum placed a send leg")
		return nil, nil
	}
	var err error
	withCapturedOutput(func() {
		err = mergeThenSend(testCmdWithFlags(true, true), l, tokens, sources, ids, 3000, 3000, priv, pub, "r", credential.ResolvedTarget{Target: recipient})
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotTarget != recipient {
		t.Errorf("consolidated to %v, want the recipient", gotTarget)
	}
	fresh, _ := ledger.Load()
	if len(fresh.Handoffs) != 1 || fresh.Handoffs[0].Kind != ledger.HandoffGift || fresh.Handoffs[0].Handoff != "tok-merged" {
		t.Errorf("handoff not kept: %+v", fresh.Handoffs)
	}
	if len(fresh.Held()) != 1 { // only the untouched pubkey bill
		t.Errorf("held %d, want the merged notes gone", len(fresh.Held()))
	}
}

// keyBillLedger is a fresh on-disk ledger holding one bill left on a throwaway
// key by a merge, registered on h. Returns the ledger, its id and the key.
func keyBillLedger(t *testing.T, h *fakeBillHub) (*ledger.Ledger, string, string) {
	t.Helper()
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	l, err := ledger.Load()
	if err != nil {
		t.Fatal(err)
	}
	priv, _, _ := newThrowawayKey()
	e, err := l.Add(ledger.Entry{Token: "tok-k", WalletPubkey: "pk-k", AmountMillis: ptrTo(uint64(1000)),
		IdentityRequired: ptrTo(true), ThrowawayKey: priv})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	h.bill("tok-k", nipcash.BySigning(priv), 1000)
	return l, e.ID, priv
}

// T-05 (B3a): a note moved off its key whose final save fails is handed over
// as <token>#<secret>, never swallowed: after a carve, disk names a token the
// Hub no longer has.
func TestToCashNotes_ReportsANoteMovedButNotSaved(t *testing.T) {
	h := newFakeBillHub(t)
	h.carve = true
	l, id, _ := keyBillLedger(t, h)
	h.afterCommit = func() { failSaves(t) }

	err := undoMoves(testCmdWithFlags(true, false), l, []string{id})
	rec := output.AsCLIError(err).Recovery
	live := h.liveTokens()
	if len(live) != 1 || !strings.Contains(rec, live[0]+"#") {
		t.Fatalf("recovery %q does not name the carved bill %v (err %v)", rec, live, err)
	}
	secret := rec[strings.Index(rec, live[0]+"#")+len(live[0])+1:]
	if h.owner(live[0]) != nipcash.BySecret(secret) {
		t.Errorf("recovery secret does not spend the carved bill")
	}
}

// toCashNotes never moves a bill with a call still open, even when named: the
// Hub has not said what that call did.
func TestToCashNotes_SkipsABillWithAnOpenCall(t *testing.T) {
	h := newFakeBillHub(t)
	l, id, _ := keyBillLedger(t, h)
	rid, _ := nipcash.NewRequestID()
	if _, err := l.AddCall(ledger.Call{RequestID: rid, Method: nipcash.MethodCashTransfer, Purpose: purposeProtect,
		State: ledger.CallLookup, SourceEntryIDs: []string{id}}); err != nil {
		t.Fatal(err)
	}
	if converted, unsaved := toCashNotes(testCmdWithFlags(true, true), l, []string{id}); len(converted)+len(unsaved) != 0 || h.moveCount() != 0 {
		t.Errorf("converted %v unsaved %v after %d moves; want the bill left alone", converted, unsaved, h.moveCount())
	}
}

// A refusal whose undo then fails to save keeps its own classification and
// carries the moved note's recovery.
func TestWithRecovery_KeepsTheClassification(t *testing.T) {
	cmd := testCmdWithFlags(true, false)
	classified := output.UsageError(cmd, errors.New("refused"))
	got := withRecovery(classified, unsavedNotesError(cmd, []string{"tok#sec"}))
	ce := output.AsCLIError(got)
	if ce.Code != output.CodeUsage || !strings.Contains(ce.Recovery, "tok#sec") {
		t.Errorf("code %q recovery %q; want usage, with tok#sec", ce.Code, ce.Recovery)
	}
}
