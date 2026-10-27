package cmd

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	nipcwclient "github.com/ohstr/nmilat/nipcw/client"

	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/crashpoint"
	"github.com/ohstr/cashctl/internal/ledger"
)

// T-13: cashctl stops dead at each commit point (the hook panics, as the
// crashpoints build SIGKILLs); what was saved before it is enough to finish the
// call from the Hub's answer, never by sending it again under its own id.

// crashAt runs fn and reports whether it stopped at point.
func crashAt(t *testing.T, point string, fn func()) (crashed bool) {
	t.Helper()
	restore := crashpoint.SetHook(func(name string) {
		if name == point {
			panic(point)
		}
	})
	defer restore()
	defer func() {
		if r := recover(); r != nil {
			if r != point {
				panic(r)
			}
			crashed = true
		}
	}()
	fn()
	return false
}

// openCallDue reloads the ledger and makes its one open call due.
func openCallDue(t *testing.T) *ledger.Call {
	t.Helper()
	l, err := ledger.Load()
	if err != nil {
		t.Fatal(err)
	}
	var open *ledger.Call
	for i := range l.Calls {
		if l.Calls[i].Open() {
			open = &l.Calls[i]
		}
	}
	if open == nil {
		t.Fatal("no open call after the crash")
	}
	open.Deadline = time.Now().Unix() - 1
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	return open
}

func TestCrashMatrix_Protect(t *testing.T) {
	done, _ := json.Marshal(map[string]any{"amount_millis": 1000, "identity_type": "cash"})
	for _, tc := range []struct {
		point     string
		committed bool // the Hub applied it before the crash
	}{
		{"calls.after_writeahead", false}, // C1: never published
		{"calls.after_send", true},        // C2: published, the Hub committed
		{"calls.before_apply", true},      // C3: the reply came, not applied
	} {
		t.Run(tc.point, func(t *testing.T) {
			h := newFakeBillHub(t)
			l, ids := protectLedger(t, h)
			e, _ := l.Find(ids[0])
			if !crashAt(t, tc.point, func() { _, _ = protectRekeyOnly(newTestReceiveCmd(), l, e, true) }) {
				t.Fatalf("did not stop at %s", tc.point)
			}
			c := openCallDue(t)
			if len(c.Secrets) != 1 {
				t.Fatalf("open call %+v, want its new secret kept", c)
			}
			secret := c.Secrets[0].Secret
			if tc.committed != (h.moveCount() == 1) {
				t.Fatalf("moves %d at %s", h.moveCount(), tc.point)
			}
			withLookup(t, func(*ledger.Call) (nipcashclient.LookupOutcome, error) {
				if tc.committed {
					return nipcashclient.LookupOutcome{Status: nipcash.LookupDone, Result: done}, nil
				}
				return nipcashclient.LookupOutcome{Status: nipcash.LookupUnknown}, nil
			})
			fresh := settleNow(t)
			got, _ := fresh.Find(ids[0])
			if got.CashProtection != ledger.CashProtected || h.owner("tok-a") != nipcash.BySecret(got.CashSecret) {
				t.Errorf("the bill is not spent by the secret it holds after settling")
			}
			if tc.committed && got.CashSecret != secret {
				t.Error("a committed protect did not keep the secret written ahead")
			}
			if h.moveCount() != 1 {
				t.Errorf("%d moves, want exactly one at the Hub", h.moveCount())
			}
			for _, call := range fresh.Calls {
				if call.Open() {
					t.Errorf("call %s still open (%s/%s)", call.CallID, call.State, call.Detail)
				}
			}
		})
	}
}

func TestCrashMatrix_Mint(t *testing.T) {
	for _, tc := range []struct {
		point     string
		committed bool
	}{
		{"calls.after_writeahead", false},
		{"calls.after_send", true},
		{"calls.before_apply", true},
	} {
		t.Run(tc.point, func(t *testing.T) {
			f, hub := setUpCashHub(t)
			if !crashAt(t, tc.point, func() { _, _ = runMint(t, hub, nil) }) {
				t.Fatalf("did not stop at %s", tc.point)
			}
			c := openCallDue(t)
			if len(c.Secrets) != 1 {
				t.Fatalf("open mint %+v, want its cash secret kept", c)
			}
			f.lookup = func(_ nipcash.PendingCall, p nipcash.MintCashParams) (nipcashclient.LookupOutcome, error) {
				if tc.committed {
					return nipcashclient.LookupOutcome{Status: nipcash.LookupDone, Result: f.doneResult(p)}, nil
				}
				return nipcashclient.LookupOutcome{Status: nipcash.LookupUnknown}, nil
			}
			l := settleNow(t)
			got, _ := l.FindCall(c.CallID)
			switch {
			case tc.committed && (got.State != ledger.CallDone || len(l.Handoffs) != 1):
				t.Errorf("committed mint: %s/%s, %d handoffs; want done and handed out", got.State, got.Detail, len(l.Handoffs))
			case !tc.committed && (got.State != ledger.CallStuck || got.Detail != "confirm_replace"):
				t.Errorf("unsent mint: %s/%s, want stuck/confirm_replace (never re-placed on its own)", got.State, got.Detail)
			}
			if f.mints > 1 {
				t.Errorf("%d mints sent", f.mints)
			}
		})
	}
}

func TestCrashMatrix_Join(t *testing.T) {
	for _, tc := range []struct {
		point     string
		committed bool
	}{
		{"joins.after_writeahead", false}, // J1
		{"calls.after_send", true},        // J2
	} {
		t.Run(tc.point, func(t *testing.T) {
			f := &fakeCircleHub{deadline: time.Now().Unix() - 1}
			setUpCircleHub(t, f)
			if !crashAt(t, tc.point, func() { _, _, _ = runJoinCLI(t, testCircleHub(t), nil) }) {
				t.Fatalf("did not stop at %s", tc.point)
			}
			joins := allJoins(t)
			if len(joins) != 1 || joins[0].State != config.JoinWritten || joins[0].ConnectionSecret == "" {
				t.Fatalf("joins %+v, want one written with its secret", joins)
			}
			f.lookup = func(pj nipcwclient.PendingJoin) (nipcwclient.JoinAction, error) {
				if tc.committed {
					return nipcwclient.JoinAction{Kind: nipcwclient.JoinClose, Response: walletResponse(pj)}, nil
				}
				return nipcwclient.JoinAction{Kind: nipcwclient.JoinMayRejoin}, nil
			}
			settleJoins(newCashSettleCmd(), "")
			joins = allJoins(t)
			s, _ := config.Load()
			switch {
			case tc.committed && (joins[0].State != config.JoinDone || len(s.Connections) != 1):
				t.Errorf("committed join: %+v, %d wallets", joins[0], len(s.Connections))
			case !tc.committed && (joins[0].State != config.JoinNotApplied || len(s.Connections) != 0):
				t.Errorf("unsent join: %+v, %d wallets", joins[0], len(s.Connections))
			}
		})
	}
}
