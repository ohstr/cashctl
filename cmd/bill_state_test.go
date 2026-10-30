package cmd

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/ohstr/nmilat/nipcash/transport"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/ohstr/nmilat/utils"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/identity"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// K-POCKET-7: every case of the shared result→state table (nmilat
// testdata/vectors/pocket.json, copied to testdata/vectors/result_to_state.json)
// maps the same through stateForResult. nmilat's non-definitive "unknown" is
// cashctl's "no state written".
func TestStateForResult_SharedVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/vectors/result_to_state.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Cases []struct {
			Name       string             `json:"name"`
			Result     string             `json:"result"`
			Tombstone  *nipcash.Tombstone `json:"tombstone"`
			Proof      string             `json:"proof"`
			State      string             `json:"state"`
			Definitive bool               `json:"definitive"`
		} `json:"resultToState"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Cases) == 0 {
		t.Fatal("no resultToState cases")
	}
	for _, c := range file.Cases {
		var a billAnswer
		switch c.Result {
		case "own_spend":
			a.OwnSpend = true
		case "tombstone":
			a.Tombstone = c.Tombstone
		case "not_found":
			a.NotFound, a.Proof = true, c.Proof
		case "no_answer":
		default:
			t.Fatalf("%s: unknown result %q", c.Name, c.Result)
		}
		want := c.State
		if !c.Definitive {
			want = ""
		}
		if got := stateForResult(a); got != want {
			t.Errorf("%s: state = %q, want %q", c.Name, got, want)
		}
	}
}

// TestProofOf_RotatedStoredKeyIsNotReclaim is R-CP-RECLAIM: on the no-`--as`
// path, a pubkey-mode entry's NOT_FOUND is classified reclaimed ONLY when the
// slice was addressed to the key now signing. After an identity rotation a
// NOT_FOUND to the rotated-away key proves nothing (other_key → no terminal
// state), so a live pubkey holding is never wrongly marked reclaimed. A legacy
// entry with no recorded addressee keeps the conservative default (own key).
func TestProofOf_RotatedStoredKeyIsNotReclaim(t *testing.T) {
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	t.Setenv(asCredentialEnv, "")
	if _, err := identity.GenerateAndSaveLocal(); err != nil {
		t.Fatal(err)
	}
	priv, err := identity.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	myPub, err := utils.GetPublicKey(priv)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name      string
		addressed string // entry.IdentityPubkey, the key the slice was addressed to
		wantProof string
		wantState string
	}{
		{"own key", myPub, proofIdentityKey, ledger.StatusReclaimed},
		{"rotated key", strings.Repeat("ab", 32), proofOtherKey, ""},
		{"empty addressee (legacy) fails safe to unknown", "", proofOtherKey, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := testCmdWithFlags(false, false)
			// IdentityRequired true: an identity-bound (pubkey-mode) slice that
			// reaches proofOf's addressee check, not a cash note or the
			// identity-not-required other_key branch.
			e := &ledger.Entry{
				ID:               "tok-a",
				Status:           ledger.StatusHeld,
				IdentityRequired: ptrTo(true),
				IdentityPubkey:   tc.addressed,
			}
			if got := proofOf(cmd, e); got != tc.wantProof {
				t.Fatalf("proofOf = %q, want %q", got, tc.wantProof)
			}
			if got := stateForResult(billAnswer{NotFound: true, Proof: tc.wantProof}); got != tc.wantState {
				t.Errorf("stateForResult(not_found, %s) = %q, want %q", tc.wantProof, got, tc.wantState)
			}
		})
	}
}

// statusLedger saves held entries made by mk (one per call) and returns them.
//
// The default entry is a pubkey-mode slice addressed to the local identity: the
// key a no-`--as`, no-throwaway signer resolves to (signerPubKeyHex ->
// localPubKeyHex). So its NOT_FOUND is a proof BY the key the slice was
// addressed to — the entry's own identity key, reclaimed (R-CP-RECLAIM). mk may
// override IdentityPubkey (or short-circuit the addressee check entirely with a
// cash secret, connection key or --as) for cases that must not reclaim.
func statusLedger(t *testing.T, mk ...func(*ledger.Entry)) (*ledger.Ledger, []string) {
	t.Helper()
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	t.Setenv(asCredentialEnv, "")
	if _, err := identity.GenerateAndSaveLocal(); err != nil {
		t.Fatal(err)
	}
	priv, err := identity.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	localPub, err := utils.GetPublicKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	l, err := ledger.Load()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i, f := range mk {
		amt := uint64(1000 * (i + 1))
		in := ledger.Entry{Token: "tok" + string(rune('a'+i)), WalletPubkey: "pub" + string(rune('a'+i)), AmountMillis: &amt, IdentityPubkey: localPub}
		if f != nil {
			f(&in)
		}
		e, err := l.Add(in)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, e.ID)
	}
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	return l, ids
}

// withStatus answers every cash_status with fn.
func withStatus(t *testing.T, fn func(cmd *cobra.Command, e *ledger.Entry) (*nipcash.CashStatusResult, error)) {
	t.Helper()
	real := fetchCashStatusFn
	t.Cleanup(func() { fetchCashStatusFn = real })
	fetchCashStatusFn = func(cmd *cobra.Command, _ bool, e *ledger.Entry) (*nipcash.CashStatusResult, error) {
		return fn(cmd, e)
	}
}

func hubNotFoundErr(cmd *cobra.Command) error {
	return output.NWCErrorForCashToken(cmd, &relayclient.WalletError{Method: "cash_status", Code: "NOT_FOUND", Message: "no slice"})
}

func savedStatus(t *testing.T, id string) string {
	t.Helper()
	l, err := ledger.Load()
	if err != nil {
		t.Fatal(err)
	}
	e, ok := l.Find(id)
	if !ok {
		t.Fatalf("entry %s gone", id)
	}
	return e.Status
}

func heldTotal(t *testing.T) int64 {
	t.Helper()
	l, err := ledger.Load()
	if err != nil {
		t.Fatal(err)
	}
	_, total, _ := summarizeHeldTokens(l.Held(), time.Now().Unix())
	return total
}

func runStatusFor(t *testing.T, jsonMode bool, id string) (string, error) {
	t.Helper()
	cmd := newTestStatusCmd(jsonMode, false)
	if id != "" {
		_ = cmd.Flags().Set("token", id)
	}
	var runErr error
	stdout, _ := captureStreams(t, func() { runErr = runCashStatus(cmd, nil) })
	return stdout, runErr
}

func wantReason(t *testing.T, err error, code output.ErrorCode, reason string) {
	t.Helper()
	ce := output.AsCLIError(err)
	if ce.Code != code || ce.Reason != reason {
		t.Errorf("error = %q/%q (%v), want %q/%q", ce.Code, ce.Reason, err, code, reason)
	}
}

// A3-CTL-1 (1): NOT_FOUND for a proof by the entry's own key is reclaimed, and
// the balance drops by its amount.
func TestRunCashStatus_IdentityKeyNotFoundIsReclaimed(t *testing.T) {
	_, ids := statusLedger(t, nil, nil)
	before := heldTotal(t)
	withStatus(t, func(cmd *cobra.Command, e *ledger.Entry) (*nipcash.CashStatusResult, error) {
		return nil, hubNotFoundErr(cmd)
	})
	_, err := runStatusFor(t, true, ids[0])
	wantReason(t, err, output.CodeSpent, ledger.StatusReclaimed)
	if got := savedStatus(t, ids[0]); got != ledger.StatusReclaimed {
		t.Errorf("status = %q, want reclaimed", got)
	}
	if after := heldTotal(t); before-after != 1000 {
		t.Errorf("held balance %d -> %d, want a drop of 1000", before, after)
	}
}

// (2): NOT_FOUND for a cash note's own secret is reclaimed.
func TestRunCashStatus_CashSecretNotFoundIsReclaimed(t *testing.T) {
	_, ids := statusLedger(t, func(e *ledger.Entry) { e.IdentityRequired, e.CashSecret = ptrTo(false), "aa" })
	withStatus(t, func(cmd *cobra.Command, e *ledger.Entry) (*nipcash.CashStatusResult, error) {
		return nil, hubNotFoundErr(cmd)
	})
	_, _ = runStatusFor(t, true, ids[0])
	if got := savedStatus(t, ids[0]); got != ledger.StatusReclaimed {
		t.Errorf("status = %q, want reclaimed", got)
	}
}

// (3), SG-1: a connection key's NOT_FOUND writes nothing and stays
// status_unreadable; so does a --as key cashctl cannot tie to the entry.
func TestRunCashStatus_ConnectionKeyOrOtherKeyNotFoundWritesNothing(t *testing.T) {
	_, ids := statusLedger(t, func(e *ledger.Entry) { e.ConnectionKeyPlatform = "nostr" }, nil)
	withStatus(t, func(cmd *cobra.Command, e *ledger.Entry) (*nipcash.CashStatusResult, error) {
		return nil, hubNotFoundErr(cmd)
	})
	_, err := runStatusFor(t, true, ids[0])
	wantReason(t, err, output.CodeNotFound, reasonStatusUnreadable)
	if got := savedStatus(t, ids[0]); got != ledger.StatusHeld {
		t.Errorf("connection-key status = %q, want held", got)
	}
	t.Setenv(asCredentialEnv, "pubkey:"+"11")
	_, err = runStatusFor(t, true, ids[1])
	if ce := output.AsCLIError(err); ce.Code != output.CodeNotFound || ce.Reason != "" {
		t.Errorf("--as other key: %q/%q, want plain not_found", ce.Code, ce.Reason)
	}
	if got := savedStatus(t, ids[1]); got != ledger.StatusHeld {
		t.Errorf("--as other key status = %q, want held", got)
	}
}

// (4): a timeout or transport error never writes a final state.
func TestRunCashStatus_TimeoutWritesNothing(t *testing.T) {
	_, ids := statusLedger(t, nil)
	withStatus(t, func(cmd *cobra.Command, e *ledger.Entry) (*nipcash.CashStatusResult, error) {
		return nil, classifyCashTokenNWCErr(cmd, context.DeadlineExceeded)
	})
	_, err := runStatusFor(t, true, ids[0])
	if output.AsCLIError(err).Code != output.CodeNetwork {
		t.Errorf("error %v, want network", err)
	}
	if got := savedStatus(t, ids[0]); got != ledger.StatusHeld {
		t.Errorf("status = %q, want held", got)
	}
}

// (5): a tombstone maps by its reason: reclaimed and expired to themselves,
// redeemed (the bill ended later, by another recipient) to spent.
func TestRunCashStatus_TombstoneMapsByReason(t *testing.T) {
	for reason, want := range map[string]string{
		nipcash.ReasonReclaimed: ledger.StatusReclaimed,
		nipcash.ReasonExpired:   ledger.StatusExpired,
		nipcash.ReasonRedeemed:  ledger.StatusSpent,
		"":                      ledger.StatusSpent,
	} {
		_, ids := statusLedger(t, nil)
		withStatus(t, func(*cobra.Command, *ledger.Entry) (*nipcash.CashStatusResult, error) {
			return &nipcash.CashStatusResult{Tombstone: &nipcash.Tombstone{Status: "spent", Reason: reason}}, nil
		})
		_, err := runStatusFor(t, true, ids[0])
		if got := savedStatus(t, ids[0]); got != want {
			t.Errorf("reason %q: status = %q, want %q", reason, got, want)
		}
		wantR := ""
		if want != ledger.StatusSpent {
			wantR = want
		}
		wantReason(t, err, output.CodeSpent, wantR)
	}
}

// (6): a final state is never overwritten.
func TestRecordNotFound_NeverOverwritesAFinalState(t *testing.T) {
	l, ids := statusLedger(t, nil)
	e, _ := l.Find(ids[0])
	e.Status = ledger.StatusSpent
	_ = l.Save()
	_, _ = recordNotFound(&cobra.Command{}, l, e, proofIdentityKey)
	markGone(l, e, &nipcash.Tombstone{Status: "spent", Reason: nipcash.ReasonExpired})
	if got := savedStatus(t, ids[0]); got != ledger.StatusSpent {
		t.Errorf("status = %q, want spent kept", got)
	}
}

// (7): several bills in one run: one reclaimed row, shown as "reclaimed" in
// --json, the others unaffected, and the state persists.
func TestRunCashStatus_SeveralBillsOneReclaimed(t *testing.T) {
	_, ids := statusLedger(t, nil, nil, nil)
	withStatus(t, func(cmd *cobra.Command, e *ledger.Entry) (*nipcash.CashStatusResult, error) {
		if e.ID == ids[1] {
			return nil, hubNotFoundErr(cmd)
		}
		return &nipcash.CashStatusResult{}, nil
	})
	stdout, _ := runStatusFor(t, true, "")
	var report struct {
		Statuses []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"statuses"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	for _, r := range report.Statuses {
		want := "ok"
		if r.ID == ids[1] {
			want = ledger.StatusReclaimed
		}
		if r.Status != want {
			t.Errorf("row %s status %q, want %q", r.ID, r.Status, want)
		}
	}
	for i, id := range ids {
		want := ledger.StatusHeld
		if i == 1 {
			want = ledger.StatusReclaimed
		}
		if got := savedStatus(t, id); got != want {
			t.Errorf("entry %d status %q, want %q", i, got, want)
		}
	}
}

// A reclaimed or expired entry named again is refused locally, with its state
// as the reason.
func TestAlreadySpentError_ReclaimedAndExpired(t *testing.T) {
	for _, s := range []string{ledger.StatusReclaimed, ledger.StatusExpired} {
		wantReason(t, alreadySpentError(&cobra.Command{}, "tok", s), output.CodeSpent, s)
	}
}

// G6 redeem: cash_redeem answered NOT_FOUND for the entry's own key is
// reclaimed; for a connection key it stays held.
func TestApplyRedeemResults_NotFoundIsReclaimed(t *testing.T) {
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	t.Setenv(asCredentialEnv, "")
	// A no-`--as`, no-throwaway signer is the local identity; a pubkey-mode
	// entry addressed to it is own-key-addressed, so its NOT_FOUND is the
	// entry's own identity key (reclaimed). The connection-key case short-
	// circuits before the addressee check and stays held (SG-1).
	if _, err := identity.GenerateAndSaveLocal(); err != nil {
		t.Fatal(err)
	}
	priv, err := identity.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	localPub, err := utils.GetPublicKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	for _, ck := range []bool{false, true} {
		e := &ledger.Entry{ID: "tok-a", Status: ledger.StatusHeld}
		if ck {
			e.ConnectionKeyPlatform = "nostr"
		} else {
			e.IdentityPubkey = localPub
		}
		l := &ledger.Ledger{Entries: []ledger.Entry{*e}}
		e = &l.Entries[0]
		plans := []redeemPlan{{Index: 0, Entry: e}}
		outcomes := []redeemOutcome{{EntryID: e.ID}}
		applyRedeemResults(&cobra.Command{}, l, plans, []nipcashclient.RedeemOutcome{
			batchOutcome(e.ID, nipcashclient.OutcomeError, nil, &transport.ResultError{Code: "NOT_FOUND", Message: "no slice"}),
		}, outcomes, "savings", nil)
		want := ledger.StatusReclaimed
		if ck {
			want = ledger.StatusHeld
		}
		if e.Status != want {
			t.Errorf("connection key %v: status %q, want %q", ck, e.Status, want)
		}
	}
}

// G6 transfer: a cash_transfer answered NOT_FOUND, then looked up and never
// applied, makes its source reclaimed when its own secret authorized it; a
// call authorized with --as writes nothing.
func TestSettle_TransferNotFoundIsReclaimed(t *testing.T) {
	for _, as := range []bool{false, true} {
		_, id, c, _ := lostProtect(t)
		l, _ := ledger.Load()
		call, _ := l.FindCall(c.CallID)
		call.FirstError = "NOT_FOUND"
		if as {
			call.AuthorizerKind = ledger.AuthAsCredential
		}
		_ = l.Save()
		withLookup(t, func(*ledger.Call) (nipcashclient.LookupOutcome, error) {
			return nipcashclient.LookupOutcome{Status: nipcash.LookupUnknown}, nil
		})
		l = settleNow(t)
		e, _ := l.Find(id)
		want := ledger.StatusReclaimed
		if as {
			want = ledger.StatusHeld
		}
		if e.Status != want {
			t.Errorf("--as %v: status %q, want %q", as, e.Status, want)
		}
	}
}

// G6 lookup done: a protect whose moved bill comes back as an expired
// tombstone leaves the entry expired, not spent.
func TestApplyTransferResult_ExpiredTombstoneIsExpired(t *testing.T) {
	l := &ledger.Ledger{Entries: []ledger.Entry{{ID: "tok-a", Status: ledger.StatusHeld}}}
	c := &ledger.Call{CallID: "c1", Purpose: purposeProtect, SourceEntryIDs: []string{"tok-a"}}
	if _, err := applyTransferResult(&cobra.Command{}, l, c, &nipcash.CashTransferResult{
		NewWalletTombstone: &nipcash.Tombstone{Status: "spent", Reason: nipcash.ReasonExpired},
	}); err != nil {
		t.Fatal(err)
	}
	if got := l.Entries[0].Status; got != ledger.StatusExpired {
		t.Errorf("status %q, want expired", got)
	}
}
