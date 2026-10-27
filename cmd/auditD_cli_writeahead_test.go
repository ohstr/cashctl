package cmd

// T-11, T-12: every bill-creating call is written to the ledger, with its exact
// deadline, target, device, authorizer and every secret it generated, BEFORE it
// is sent — asserted from inside the "Hub", through a fresh process's view of
// local state, for each call site. A kill at that instant must lose nothing.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
	"github.com/ohstr/cashctl/internal/store"
)

// auditDSeedTwoHeldCash puts two held cash-mode entries in a fresh ledger and
// returns it plus their IDs, in the order Add assigned them.
func auditDSeedTwoHeldCash(t *testing.T) (*ledger.Ledger, []string) {
	t.Helper()
	l, err := ledger.Load()
	if err != nil {
		t.Fatalf("ledger.Load() error = %v", err)
	}
	for _, tok := range []string{"lokicash1wa-a", "lokicash1wa-b"} {
		amt := uint64(1000)
		if _, addErr := l.Add(ledger.Entry{
			Token: tok, WalletPubkey: hexWallet(tok), Secret: "s-" + tok,
			AmountMillis: &amt, IdentityRequired: ptrTo(false), CashSecret: strings.Repeat("0a", 32),
			CashProtection: ledger.CashShared, Verified: true,
		}); addErr != nil {
			t.Fatalf("l.Add(%s) error = %v", tok, addErr)
		}
	}
	if err := l.Save(); err != nil {
		t.Fatalf("l.Save() error = %v", err)
	}
	return l, []string{l.Entries[0].ID, l.Entries[1].ID}
}

// inFlight is what a fresh process finds while a call is at the Hub.
type inFlight struct {
	calls []ledger.Call
}

func snapshotInFlight(t *testing.T) inFlight {
	t.Helper()
	fresh, err := ledger.Load()
	if err != nil {
		t.Fatalf("in-flight ledger.Load() error = %v", err)
	}
	var open []ledger.Call
	for _, c := range fresh.Calls {
		if c.Open() {
			open = append(open, c)
		}
	}
	return inFlight{calls: open}
}

// requireWrittenAhead checks the one open call a fresh process sees.
func (f inFlight) requireWrittenAhead(t *testing.T, site, method, purpose string, sources []string, secret string) {
	t.Helper()
	if len(f.calls) != 1 {
		t.Fatalf("%s: %d open calls on disk while the request was at the Hub, want 1", site, len(f.calls))
	}
	c := f.calls[0]
	device, _ := store.DeviceID()
	switch {
	case !nipcash.ValidRequestID(c.RequestID):
		t.Errorf("%s: request id %q", site, c.RequestID)
	case c.Method != method || c.Purpose != purpose:
		t.Errorf("%s: method/purpose %s/%s, want %s/%s", site, c.Method, c.Purpose, method, purpose)
	case c.Deadline <= time.Now().Unix():
		t.Errorf("%s: deadline %d is not ahead", site, c.Deadline)
	case c.Target == "" || c.HubIdentity == "" || c.SentH < 60:
		t.Errorf("%s: target %q hub %q H %d", site, c.Target, c.HubIdentity, c.SentH)
	case c.DeviceID != device:
		t.Errorf("%s: device %q, want this wallet file's", site, c.DeviceID)
	case c.AuthorizerKind == "":
		t.Errorf("%s: no authorizer recorded", site)
	case strings.Join(c.SourceEntryIDs, ",") != strings.Join(sources, ","):
		t.Errorf("%s: sources %v, want %v", site, c.SourceEntryIDs, sources)
	}
	if secret != "" {
		found := false
		for _, s := range c.Secrets {
			found = found || s.Secret == secret
		}
		if !found {
			t.Errorf("%s: the generated secret is not in the call's written-ahead secrets", site)
		}
	}
}

// T-12: protect, merge move, unmerge move, send and consolidate each write
// their call ahead; a success closes it and wipes its secrets.
func TestWriteAhead_EveryCallSiteWritesItsCallFirst(t *testing.T) {
	t.Run("protect", func(t *testing.T) {
		auditDTempLedger(t)
		l, ids := auditDSeedTwoHeldCash(t)
		var seen inFlight
		var secret string
		withMoveFn(t, func(_ *ledger.Entry, _ nipcash.Credential, to nipcash.Target) (*nipcash.CashTransferResult, error) {
			secret = to.(*nipcash.CashTarget).Secret()
			seen = snapshotInFlight(t)
			return &nipcash.CashTransferResult{AmountMillis: 1000}, nil
		})
		e, _ := l.Find(ids[0])
		if st, _ := protectRekeyOnly(newTestReceiveCmd(), l, e, true); st.Status != "rekeyed" {
			t.Fatalf("protect: %+v", st)
		}
		seen.requireWrittenAhead(t, "protect", nipcash.MethodCashTransfer, purposeProtect, ids[:1], secret)
		requireClosed(t, ledger.CallDone)
	})
	t.Run("merge move", func(t *testing.T) {
		auditDTempLedger(t)
		l, ids := auditDSeedTwoHeldCash(t)
		priv, pub, _ := newThrowawayKey()
		var seen inFlight
		withMoveFn(t, func(*ledger.Entry, nipcash.Credential, nipcash.Target) (*nipcash.CashTransferResult, error) {
			seen = snapshotInFlight(t)
			fresh, _ := ledger.Load()
			if e, _ := fresh.Find(ids[0]); e.PendingThrowawayKey != priv {
				t.Error("the merge key was not on disk while the move was at the Hub")
			}
			return &nipcash.CashTransferResult{AmountMillis: 1000}, nil
		})
		if _, err := moveCashNotesToKey(&cobra.Command{}, l, ids[:1], priv, pub); err != nil {
			t.Fatal(err)
		}
		seen.requireWrittenAhead(t, "merge move", nipcash.MethodCashTransfer, purposeMergeMove, ids[:1], "")
		requireClosed(t, ledger.CallDone)
	})
	t.Run("consolidate to cash", func(t *testing.T) {
		auditDTempLedger(t)
		l, ids := auditDSeedTwoHeldCash(t)
		target := mustCashTarget(t)
		var seen inFlight
		withFakeAttemptCashConsolidate(t, func(string, []nipcash.Source, nipcash.Target) (*nipcash.CashConsolidateResult, error) {
			seen = snapshotInFlight(t)
			return &nipcash.CashConsolidateResult{AmountMillis: 2000, NewWalletToken: "lokicash1merged", NewWalletPubkey: hexWallet("merged")}, nil
		})
		sources := []nipcash.Source{
			{WalletPubkey: hexWallet("lokicash1wa-a"), Amount: 1000, Credential: nipcash.BySigning(strings.Repeat("11", 32))},
			{WalletPubkey: hexWallet("lokicash1wa-b"), Amount: 1000, Credential: nipcash.BySigning(strings.Repeat("12", 32))},
		}
		e, _, err := doCashConsolidate(&cobra.Command{}, l, []string{"lokicash1wa-a"}, sources, ids, target, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Save(); err != nil {
			t.Fatal(err)
		}
		seen.requireWrittenAhead(t, "consolidate", nipcash.MethodCashConsolidate, purposeConsolidate, ids, target.Secret())
		if e.CashSecret != target.Secret() {
			t.Error("the merged note does not carry the secret")
		}
		requireClosed(t, ledger.CallDone)
	})
}

// requireClosed checks that the one call written is final and keeps no secret.
func requireClosed(t *testing.T, state string) {
	t.Helper()
	fresh, _ := ledger.Load()
	if len(fresh.Calls) != 1 || fresh.Calls[0].State != state || len(fresh.Calls[0].Secrets) != 0 || fresh.Calls[0].AuthorizerSecret != "" {
		t.Errorf("after the call: %+v; want one %s call with its secrets wiped", fresh.Calls, state)
	}
}

// T-11: a call that cannot be written ahead is not sent: the "Hub" never sees
// it, nothing is left behind, and the user is told the money has not moved.
func TestWriteAhead_AFailedWriteSendsNothing(t *testing.T) {
	auditDTempLedger(t)
	l, ids := auditDSeedTwoHeldCash(t)
	hooks := installFakeCaller(t)
	hooks.transfer = func(*ledger.Entry, nipcash.CashTransferParams) (*nipcash.CashTransferResult, error) {
		t.Error("a call whose write-ahead failed reached the Hub")
		return nil, errors.New("unreachable")
	}
	restore := failSaves(t)
	e, _ := l.Find(ids[0])
	_, _, err := moveCash(&cobra.Command{}, l, e, nipcash.BySecret(e.CashSecret), mustCashTarget(t), purposeProtect)
	restore()
	if err == nil || !strings.Contains(err.Error(), "not placed") || output.AsCLIError(err).Code != output.CodeInternal {
		t.Fatalf("err %v; want internal, not placed", err)
	}
	if fresh, _ := ledger.Load(); len(fresh.Calls) != 0 {
		t.Errorf("%d calls left on disk, want none", len(fresh.Calls))
	}
}

// A lost answer keeps the call open with its secret, and `wallet show` lists it
// in both modes: until it is settled, that secret may be the only copy.
func TestWalletShow_ListsAnOpenCallWithItsSecret(t *testing.T) {
	auditDTempLedger(t)
	l, ids := auditDSeedTwoHeldCash(t)
	var secret string
	withMoveFn(t, func(_ *ledger.Entry, _ nipcash.Credential, to nipcash.Target) (*nipcash.CashTransferResult, error) {
		secret = to.(*nipcash.CashTarget).Secret()
		return nil, context.DeadlineExceeded
	})
	e, _ := l.Find(ids[0])
	if st, _ := protectRekeyOnly(newTestReceiveCmd(), l, e, true); !st.PendingSecretUnresolved {
		t.Fatalf("protect with its answer lost: %+v", st)
	}
	for _, jsonMode := range []bool{true, false} {
		cmd := newWalletShowCmd()
		cmd.Flags().Bool("json", jsonMode, "")
		cmd.SetArgs(nil)
		printed := withCapturedOutput(func() {
			if err := cmd.Execute(); err != nil {
				t.Errorf("wallet show (json=%v): %v", jsonMode, err)
			}
		})
		if !strings.Contains(printed, secret) {
			t.Errorf("wallet show (json=%v) does not list the open call's secret:\n%s", jsonMode, printed)
		}
		if jsonMode {
			var doc struct {
				Calls []openCall `json:"open_calls"`
			}
			if err := json.Unmarshal([]byte(printed), &doc); err != nil || len(doc.Calls) != 1 || doc.Calls[0].Purpose != purposeProtect || doc.Calls[0].Device != "this" {
				t.Errorf("open_calls = %+v (%v)", doc.Calls, err)
			}
		}
	}
}
