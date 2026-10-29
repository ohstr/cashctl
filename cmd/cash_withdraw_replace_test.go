package cmd

import (
	"errors"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// lostWithdrawal sends a withdrawal whose reply is lost and makes its call due
// for a lookup, which answers unknown.
func lostWithdrawal(t *testing.T, reused bool) (*fakeWithdrawHub, *ledger.Call, ledger.Withdrawal) {
	t.Helper()
	f, _ := setUpWithdraw(t)
	handle := runWithdrawQuote(t)["quote"].(string)
	f.answer = func(pc nipcash.PendingCall) (*nipcash.Payout, error) {
		return nil, &nipcashclient.IndeterminateError{Call: pc, Cause: errors.New("no reply")}
	}
	if _, err := runWithdrawSend(t, nil, map[string]string{"quote": handle, "yes": "true"}); output.ExitCode(err) != 8 {
		t.Fatalf("lost reply: %v", err)
	}
	l, _ := ledger.Load()
	c, _ := l.FindCall(ledger.CallIDFor(f.sent[0].RequestID))
	c.Deadline, c.SawReused = time.Now().Unix()-1, reused
	_ = l.Save()
	f.lookup = func(nipcash.PendingCall) (nipcashclient.LookupOutcome, *nipcash.Payout, error) {
		return nipcashclient.LookupOutcome{Status: nipcash.LookupUnknown}, nil, nil
	}
	w, _, _ := ledger.WithdrawalForCall(c.CallID)
	return f, c, w
}

func remintSecretOf(pc nipcash.PendingCall) string {
	for _, s := range pc.Secrets {
		if s.Role == ledger.RoleRemint {
			return s.Secret
		}
	}
	return ""
}

// NIP-CP §Writing (Withdrawals): a lost withdrawal is re-placed under its
// derived id with the same quote, notify and re-mint secret, and is the same
// withdrawal.
func TestWithdraw_ReplacementIsTheSameWithdrawalUnderTheDerivedID(t *testing.T) {
	f, c, w := lostWithdrawal(t, false)
	f.answer = nil
	settleNow(t)
	if len(f.sent) != 2 {
		t.Fatalf("sent %d, want a re-placement", len(f.sent))
	}
	first, again := f.sent[0], f.sent[1]
	derived, _ := nipcash.DerivedRequestID(first.RequestID)
	if again.RequestID != derived || again.DerivedFrom != first.RequestID || again.Attempt != 1 {
		t.Errorf("re-placed as %s (from %s, attempt %d), want the derived id of %s", again.RequestID, again.DerivedFrom, again.Attempt, first.RequestID)
	}
	if again.Quote != first.Quote || again.Notify != first.Notify || remintSecretOf(again) == "" || remintSecretOf(again) != remintSecretOf(first) {
		t.Errorf("the re-placement changed the quote, notify or re-mint secret")
	}
	w2, ok, _ := ledger.FindWithdrawal(w.WithdrawalID)
	if !ok || w2.QuoteID != w.QuoteID || w2.Notify != w.Notify || w2.Address != w.Address {
		t.Errorf("withdrawal after re-placement %+v", w2)
	}
	l, _ := ledger.Load()
	if parent, _ := l.FindCall(c.CallID); parent.State != ledger.CallReplaced {
		t.Errorf("parent %s/%s, want replaced", parent.State, parent.Detail)
	}
}

// The re-placements of one call are capped, and a derived id already placed
// is never sent again.
func TestWithdraw_ReplacementCapAndDerivedAlreadyPlaced(t *testing.T) {
	t.Run("at the cap", func(t *testing.T) {
		f, c, _ := lostWithdrawal(t, false)
		l, _ := ledger.Load()
		parent, _ := l.FindCall(c.CallID)
		parent.Attempt = nipcashclient.MaxReplaceAttempts
		_ = l.Save()
		l = settleNow(t)
		parent, _ = l.FindCall(c.CallID)
		if len(f.sent) != 1 || parent.State != ledger.CallStuck || parent.Detail != "replace_cap" {
			t.Errorf("sent %d, parent %s/%s; want stuck at the cap, nothing sent", len(f.sent), parent.State, parent.Detail)
		}
	})
	t.Run("one below the cap", func(t *testing.T) {
		f, c, _ := lostWithdrawal(t, false)
		f.answer = nil
		l, _ := ledger.Load()
		parent, _ := l.FindCall(c.CallID)
		parent.Attempt = nipcashclient.MaxReplaceAttempts - 1
		_ = l.Save()
		settleNow(t)
		if len(f.sent) != 2 || f.sent[1].Attempt != nipcashclient.MaxReplaceAttempts {
			t.Errorf("sent %d; want the last re-placement", len(f.sent))
		}
	})
	t.Run("derived id already placed", func(t *testing.T) {
		f, c, _ := lostWithdrawal(t, false)
		derived, _ := nipcash.DerivedRequestID(c.RequestID)
		l, _ := ledger.Load()
		if _, err := l.AddCall(ledger.Call{RequestID: derived, Method: nipcash.MethodCashWithdraw, State: ledger.CallLookup,
			DeviceID: c.DeviceID, Deadline: time.Now().Unix() + 600, Replaces: c.RequestID, Attempt: 1}); err != nil {
			t.Fatal(err)
		}
		_ = l.Save()
		l = settleNow(t)
		parent, _ := l.FindCall(c.CallID)
		if len(f.sent) != 1 || parent.State != ledger.CallReplaced || parent.Detail != "derived_already_placed" {
			t.Errorf("sent %d, parent %s/%s; want closed as replaced, nothing sent", len(f.sent), parent.State, parent.Detail)
		}
	})
}

// `cash withdraw settle --replace`: a withdrawal the Hub says never arrived
// after REQUEST_REUSED is sent again only on the user's word, under the
// derived id; under --json that word is --yes.
func TestWithdraw_SettleReplace(t *testing.T) {
	f, c, w := lostWithdrawal(t, true)
	l := settleNow(t)
	if parent, _ := l.FindCall(c.CallID); parent.Detail != "confirm_replace" || len(f.sent) != 1 {
		t.Fatalf("after REQUEST_REUSED: %s/%s, sent %d", parent.State, parent.Detail, len(f.sent))
	}
	run := func(flags map[string]string) error {
		cmd := withdrawCmd(newCashWithdrawSettleCmd(), flags)
		var err error
		captureStreams(t, func() { err = cmd.RunE(cmd, nil) })
		return err
	}
	if err := run(map[string]string{"replace": w.WithdrawalID}); output.ExitCode(err) != 2 || len(f.sent) != 1 {
		t.Fatalf("--replace under --json without --yes: %v, sent %d", err, len(f.sent))
	}
	if err := run(map[string]string{"replace": "000000000000", "yes": "true"}); output.ExitCode(err) != 4 {
		t.Errorf("--replace of no withdrawal: %v", err)
	}
	f.answer = nil
	if err := run(map[string]string{"replace": w.WithdrawalID, "yes": "true"}); err != nil {
		t.Fatalf("--replace --yes: %v", err)
	}
	derived, _ := nipcash.DerivedRequestID(f.sent[0].RequestID)
	if len(f.sent) != 2 || f.sent[1].RequestID != derived || f.sent[1].Quote != f.sent[0].Quote {
		t.Fatalf("sent %d; want the same quote under the derived id", len(f.sent))
	}
	// Nothing is left to replace: a second --replace is a conflict, not a send.
	if err := run(map[string]string{"replace": w.WithdrawalID, "yes": "true"}); output.ExitCode(err) != 5 || len(f.sent) != 2 {
		t.Errorf("a second --replace: %v, sent %d", err, len(f.sent))
	}
}
