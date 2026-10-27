package cmd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// lostProtect is a ledger whose protect of tok-a reached the Hub and lost its
// reply: one open call, with the new secret written ahead.
func lostProtect(t *testing.T) (*ledger.Ledger, string, *ledger.Call, *fakeBillHub) {
	t.Helper()
	h := newFakeBillHub(t)
	l, ids := protectLedger(t, h)
	h.dropReplies = 1
	e, _ := l.Find(ids[0])
	if st, _ := protectRekeyOnly(newTestReceiveCmd(), l, e, true); !st.PendingSecretUnresolved {
		t.Fatalf("protect with its reply lost: %+v", st)
	}
	l, _ = reload(t, ids[0])
	c, open := l.OpenCallFor(ids[0])
	if !open {
		t.Fatal("no open call after the lost reply")
	}
	// Its deadline is past: due for a lookup.
	c.Deadline = time.Now().Unix() - 1
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	return l, ids[0], c, h
}

// withLookup answers every lookup with fn.
func withLookup(t *testing.T, fn func(c *ledger.Call) (nipcashclient.LookupOutcome, error)) *int {
	t.Helper()
	n := 0
	orig := lookupCallFn
	t.Cleanup(func() { lookupCallFn = orig })
	lookupCallFn = func(_ context.Context, _ *cobra.Command, _ *ledger.Ledger, c *ledger.Call) (nipcashclient.LookupOutcome, error) {
		n++
		return fn(c)
	}
	return &n
}

func settleNow(t *testing.T) *ledger.Ledger {
	t.Helper()
	l, err := ledger.Load()
	if err != nil {
		t.Fatal(err)
	}
	settleCalls(testCmdWithFlags(true, true), l, settleOptions{})
	fresh, err := ledger.Load()
	if err != nil {
		t.Fatal(err)
	}
	return fresh
}

// T-14: a lookup answered done applies the result the Hub kept and closes the
// call; the secret moves onto the bill and leaves the call.
func TestSettle_DoneAppliesTheKeptResult(t *testing.T) {
	_, id, c, _ := lostProtect(t)
	secret := c.Secrets[0].Secret
	result, _ := json.Marshal(map[string]any{"amount_millis": 1000, "identity_type": "cash"})
	withLookup(t, func(*ledger.Call) (nipcashclient.LookupOutcome, error) {
		return nipcashclient.LookupOutcome{Status: nipcash.LookupDone, Result: result}, nil
	})
	l := settleNow(t)
	e, _ := l.Find(id)
	got, _ := l.FindCall(c.CallID)
	if e.CashSecret != secret || e.CashProtection != ledger.CashProtected {
		t.Errorf("entry after done: secret match %v, protection %q", e.CashSecret == secret, e.CashProtection)
	}
	if got.State != ledger.CallDone || len(got.Secrets) != 0 {
		t.Errorf("call after done: %+v", got)
	}
}

// T-14: unknown after a lost reply re-places the call under its derived id, never
// its own; the parent is replaced, the derived call carries on.
func TestSettle_UnknownReplacesUnderTheDerivedID(t *testing.T) {
	_, id, c, h := lostProtect(t)
	// The Hub never saw it: the fake rolls the first move back.
	h.bill("tok-a", nipcash.BySecret("stok-a"), 1000)
	withLookup(t, func(*ledger.Call) (nipcashclient.LookupOutcome, error) {
		return nipcashclient.LookupOutcome{Status: nipcash.LookupUnknown}, nil
	})
	l := settleNow(t)
	parent, _ := l.FindCall(c.CallID)
	if parent.State != ledger.CallReplaced {
		t.Fatalf("parent %+v, want replaced", parent)
	}
	derived, _ := nipcash.DerivedRequestID(c.RequestID)
	child, ok := l.FindCall(ledger.CallIDFor(derived))
	if !ok || child.State != ledger.CallDone || child.Attempt != 1 || child.Replaces != c.RequestID {
		t.Fatalf("derived call %+v (found %v), want done, attempt 1, replacing the parent", child, ok)
	}
	e, _ := l.Find(id)
	if e.CashProtection != ledger.CashProtected || h.owner("tok-a") != nipcash.BySecret(e.CashSecret) {
		t.Error("the re-placed protect did not land and apply")
	}
	if h.moveCount() != 2 {
		t.Errorf("%d moves; want the original and exactly one re-placement", h.moveCount())
	}
}

// T-16: a call the Hub answered with a code, then said it never saw, is closed
// not applied with that code: never re-placed.
func TestSettle_UnknownAfterACodeClosesItNotApplied(t *testing.T) {
	_, _, c, h := lostProtect(t)
	l, _ := ledger.Load()
	call, _ := l.FindCall(c.CallID)
	call.FirstError = "RESTRICTED"
	_ = l.Save()
	withLookup(t, func(*ledger.Call) (nipcashclient.LookupOutcome, error) {
		return nipcashclient.LookupOutcome{Status: nipcash.LookupUnknown}, nil
	})
	l = settleNow(t)
	got, _ := l.FindCall(c.CallID)
	if got.State != ledger.CallNotApplied || got.Detail != "RESTRICTED" || h.moveCount() != 1 {
		t.Errorf("call %+v after %d moves; want not_applied with RESTRICTED, never re-placed", got, h.moveCount())
	}
}

// T-14/T-17/T-18: pending waits for retry_after; expired is kept with its
// secrets and never re-placed; an unanswered lookup is never unknown, and only
// past deadline + H is it stuck; another copy's call is shown, never re-placed.
func TestSettle_WaitExpiredUnansweredForeign(t *testing.T) {
	t.Run("pending", func(t *testing.T) {
		_, _, c, _ := lostProtect(t)
		at := time.Now().Unix() + 30
		withLookup(t, func(*ledger.Call) (nipcashclient.LookupOutcome, error) {
			return nipcashclient.LookupOutcome{Status: nipcash.LookupPending, RetryAfter: at}, nil
		})
		got, _ := settleNow(t).FindCall(c.CallID)
		if got.State != ledger.CallPending || got.RetryAfter == nil || *got.RetryAfter != at {
			t.Errorf("call %+v, want pending until %d", got, at)
		}
	})
	t.Run("expired", func(t *testing.T) {
		_, _, c, h := lostProtect(t)
		withLookup(t, func(*ledger.Call) (nipcashclient.LookupOutcome, error) {
			return nipcashclient.LookupOutcome{Status: nipcash.LookupExpired}, nil
		})
		got, _ := settleNow(t).FindCall(c.CallID)
		if got.State != ledger.CallExpired || len(got.Secrets) != 1 || h.moveCount() != 1 {
			t.Errorf("call %+v; want expired, secrets kept, never re-placed", got)
		}
	})
	t.Run("unanswered", func(t *testing.T) {
		_, _, c, _ := lostProtect(t)
		withLookup(t, func(*ledger.Call) (nipcashclient.LookupOutcome, error) {
			return nipcashclient.LookupOutcome{NotServed: true}, nil
		})
		l := settleNow(t)
		if got, _ := l.FindCall(c.CallID); !got.Open() || got.State == ledger.CallStuck {
			t.Errorf("an unanswered lookup within the window: %+v", got)
		}
		call, _ := l.FindCall(c.CallID)
		call.Deadline = time.Now().Unix() - call.SentH - 10
		_ = l.Save()
		if got, _ := settleNow(t).FindCall(c.CallID); got.State != ledger.CallStuck || got.Detail != "lookup_unanswered" {
			t.Errorf("unanswered past deadline + H: %+v; want stuck lookup_unanswered", got)
		}
	})
	t.Run("foreign", func(t *testing.T) {
		l, _, c, h := lostProtect(t)
		call, _ := l.FindCall(c.CallID)
		call.DeviceID = strings.Repeat("99", 32)
		_ = l.Save()
		withLookup(t, func(*ledger.Call) (nipcashclient.LookupOutcome, error) {
			return nipcashclient.LookupOutcome{Status: nipcash.LookupUnknown}, nil
		})
		if got, _ := settleNow(t).FindCall(c.CallID); !got.Open() || h.moveCount() != 1 {
			t.Errorf("another copy's call: %+v after %d moves; want left open, never re-placed", got, h.moveCount())
		}
	})
}

// T-14: a derived id already recorded (another run placed it) closes the parent
// only; at the re-placement cap the call is stuck.
func TestSettle_NeverReplacesTwice(t *testing.T) {
	t.Run("derived recorded", func(t *testing.T) {
		_, _, c, h := lostProtect(t)
		derived, _ := nipcash.DerivedRequestID(c.RequestID)
		if err := ledger.InsertDerived(derived, c.RequestID, nil, time.Now().Unix()+3600); err != nil {
			t.Fatal(err)
		}
		withLookup(t, func(*ledger.Call) (nipcashclient.LookupOutcome, error) {
			return nipcashclient.LookupOutcome{Status: nipcash.LookupUnknown}, nil
		})
		got, _ := settleNow(t).FindCall(c.CallID)
		if got.State != ledger.CallReplaced || h.moveCount() != 1 {
			t.Errorf("call %+v after %d moves; want replaced, nothing sent", got, h.moveCount())
		}
	})
	t.Run("cap", func(t *testing.T) {
		l, _, c, h := lostProtect(t)
		call, _ := l.FindCall(c.CallID)
		call.Attempt = nipcashclient.MaxReplaceAttempts
		_ = l.Save()
		withLookup(t, func(*ledger.Call) (nipcashclient.LookupOutcome, error) {
			return nipcashclient.LookupOutcome{Status: nipcash.LookupUnknown}, nil
		})
		got, _ := settleNow(t).FindCall(c.CallID)
		if got.State != ledger.CallStuck || got.Detail != "replace_cap" || h.moveCount() != 1 {
			t.Errorf("call %+v; want stuck replace_cap", got)
		}
	})
}

// A call placed under --as cannot be looked up without it: stuck, naming the
// flag.
func TestSettle_AsCredentialNeedsTheFlag(t *testing.T) {
	l, _, c, _ := lostProtect(t)
	call, _ := l.FindCall(c.CallID)
	call.AuthorizerKind = ledger.AuthAsCredential
	_ = l.Save()
	orig := lookupCallFn
	t.Cleanup(func() { lookupCallFn = orig })
	got, _ := settleNow(t).FindCall(c.CallID)
	if got.State != ledger.CallStuck || got.Detail != "needs_credential" {
		t.Errorf("call %+v; want stuck needs_credential", got)
	}
}

// `cash settle`: exit 0 with nothing open, 8 while a call of this file stays
// open; every call is in --json's calls with its next step.
func TestRunCashSettle_ExitAndReport(t *testing.T) {
	_, _, c, _ := lostProtect(t)
	withLookup(t, func(*ledger.Call) (nipcashclient.LookupOutcome, error) {
		return nipcashclient.LookupOutcome{NotServed: true}, nil
	})
	cmd := newCashSettleCmd()
	cmd.Flags().Bool("json", true, "")
	cmd.Flags().Bool("yes", true, "")
	var err error
	stdout, _ := captureStreams(t, func() { err = runCashSettle(cmd, nil) })
	if output.ExitCode(err) != 8 {
		t.Fatalf("exit %d, want 8 while the call is open", output.ExitCode(err))
	}
	var doc struct {
		Calls []settleRow `json:"calls"`
	}
	if jErr := json.Unmarshal([]byte(stdout), &doc); jErr != nil || len(doc.Calls) != 1 || doc.Calls[0].Call != c.CallID || doc.Calls[0].NextStep == "" {
		t.Fatalf("calls %+v (%v)", doc.Calls, jErr)
	}

	result, _ := json.Marshal(map[string]any{"amount_millis": 1000, "identity_type": "cash"})
	withLookup(t, func(*ledger.Call) (nipcashclient.LookupOutcome, error) {
		return nipcashclient.LookupOutcome{Status: nipcash.LookupDone, Result: result}, nil
	})
	captureStreams(t, func() { err = runCashSettle(cmd, nil) })
	if err != nil {
		t.Errorf("once settled: %v, want exit 0", err)
	}
}

// T-02: wallet protect settles a call whose answer was lost before selecting:
// a protect that landed is reported re-keyed, with no new call.
func TestRunWalletProtect_SettlesALandedReKeyFirst(t *testing.T) {
	_, id, c, h := lostProtect(t)
	result, _ := json.Marshal(map[string]any{"amount_millis": 1000, "identity_type": "cash"})
	withLookup(t, func(*ledger.Call) (nipcashclient.LookupOutcome, error) {
		return nipcashclient.LookupOutcome{Status: nipcash.LookupDone, Result: result}, nil
	})
	var err error
	stdout, _ := captureStreams(t, func() { err = runWalletProtect(newTestProtectCmd(true, false), nil) })
	if err != nil || !strings.Contains(stdout, `"rekeyed"`) {
		t.Fatalf("wallet protect: %v\n%s", err, stdout)
	}
	if h.moveCount() != 1 {
		t.Errorf("%d moves; the landed re-key needs no new one", h.moveCount())
	}
	l, _ := ledger.Load()
	e, _ := l.Find(id)
	if got, _ := l.FindCall(c.CallID); got.State != ledger.CallDone || e.CashProtection != ledger.CashProtected {
		t.Errorf("call %s, entry protection %q; want done and protected", got.State, e.CashProtection)
	}
}
