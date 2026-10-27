package cmd

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// withPlacementWait turns the in-invocation wait on, without sleeping.
func withPlacementWait(t *testing.T) *[]time.Duration {
	t.Helper()
	var waited []time.Duration
	origOn, origSleep := placementWaitEnabled, placementSleep
	t.Cleanup(func() { placementWaitEnabled, placementSleep = origOn, origSleep })
	placementWaitEnabled = true
	placementSleep = func(d time.Duration) { waited = append(waited, d) }
	return &waited
}

// CC-A3: a coded refusal waits for the deadline, and a lookup answered unknown
// closes the call not applied with that code's own exit, not exit 8.
func TestPlacement_CodedRefusalWaitsAndClosesWithItsCode(t *testing.T) {
	l, ids, sources := consolidateFixture(t)
	waited := withPlacementWait(t)
	withFakeAttemptCashConsolidate(t, func(string, []nipcash.Source, nipcash.Target) (*nipcash.CashConsolidateResult, error) {
		return nil, &relayclient.WalletError{Code: "RESTRICTED"}
	})
	n := withLookup(t, func(*ledger.Call) (nipcashclient.LookupOutcome, error) {
		return nipcashclient.LookupOutcome{Status: nipcash.LookupUnknown}, nil
	})
	_, _, err := doCashConsolidate(&cobra.Command{}, l, []string{"dial-a"}, sources, ids, testTarget(), true)
	if ce := output.AsCLIError(err); ce == nil || ce.Code != output.CodeAuth {
		t.Fatalf("err %v, want RESTRICTED's own auth exit", err)
	}
	if len(*waited) != 1 || (*waited)[0] > maxPlacementWait || *n != 1 {
		t.Errorf("waited %v, %d lookups; want one wait to the deadline and one lookup", *waited, *n)
	}
	fresh, _ := ledger.Load()
	if _, open := fresh.OpenCallFor(ids[0]); open {
		t.Error("the refused call is still open, locking its bills")
	}
}

// CC-A3: no answer, then a lookup answered done: the result is applied as the
// reply would have been, in the same run.
func TestPlacement_LostReplyLookedUpDoneApplies(t *testing.T) {
	h := newFakeBillHub(t)
	l, ids := protectLedger(t, h)
	withPlacementWait(t)
	h.dropReplies = 1
	result, _ := json.Marshal(map[string]any{"amount_millis": 1000, "identity_type": "cash"})
	withLookup(t, func(*ledger.Call) (nipcashclient.LookupOutcome, error) {
		return nipcashclient.LookupOutcome{Status: nipcash.LookupDone, Result: result}, nil
	})
	e, _ := l.Find(ids[0])
	st, _ := protectRekeyOnly(newTestReceiveCmd(), l, e, true)
	if st.PendingSecretUnresolved || st.Status != "rekeyed" {
		t.Fatalf("protect: %+v; want it finished in the same run", st)
	}
	fresh, _ := ledger.Load()
	got, _ := fresh.Find(ids[0])
	if got.CashProtection != ledger.CashProtected {
		t.Errorf("entry %+v, want protected", got)
	}
	if _, open := fresh.OpenCallFor(ids[0]); open {
		t.Error("the call is still open after its lookup answered done")
	}
}

// CC-A3: --no-wait keeps the old exit 8 at once.
func TestPlacement_NoWaitExitsAtOnce(t *testing.T) {
	l, ids, sources := consolidateFixture(t)
	waited := withPlacementWait(t)
	withFakeAttemptCashConsolidate(t, func(string, []nipcash.Source, nipcash.Target) (*nipcash.CashConsolidateResult, error) {
		return nil, &relayclient.WalletError{Code: "RESTRICTED"}
	})
	cmd := &cobra.Command{}
	cmd.Flags().Bool("no-wait", true, "")
	_, _, err := doCashConsolidate(cmd, l, []string{"dial-a"}, sources, ids, testTarget(), true)
	if output.AsCLIError(err).Code != output.CodeIndeterminate || len(*waited) != 0 {
		t.Errorf("err %v after %d waits; want exit 8 at once", err, len(*waited))
	}
}
