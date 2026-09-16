package cmd

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nipcash"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

func TestResolveConsolidateSources_PositionalArgs(t *testing.T) {
	got, err := resolveConsolidateSources([]string{"tok-a1b2", "tok-c3d4"}, "", heldEntries(3))
	if err != nil {
		t.Fatalf("resolveConsolidateSources() error = %v", err)
	}
	want := []string{"tok-a1b2", "tok-c3d4"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestResolveConsolidateSources_SourcesFlag(t *testing.T) {
	got, err := resolveConsolidateSources(nil, "tok-a1b2,tok-c3d4", heldEntries(3))
	if err != nil {
		t.Fatalf("resolveConsolidateSources() error = %v", err)
	}
	want := []string{"tok-a1b2", "tok-c3d4"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestResolveConsolidateSources_NeitherDefaultsToEveryHeldToken(t *testing.T) {
	held := heldEntries(3)
	got, err := resolveConsolidateSources(nil, "", held)
	if err != nil {
		t.Fatalf("resolveConsolidateSources() error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d sources, want 3 (every held token)", len(got))
	}
	for i, e := range held {
		if got[i] != e.ID {
			t.Errorf("got[%d] = %q, want %q", i, got[i], e.ID)
		}
	}
}

func TestResolveConsolidateSources_NeitherHeldNoneReturnsEmpty(t *testing.T) {
	got, err := resolveConsolidateSources(nil, "", nil)
	if err != nil {
		t.Fatalf("resolveConsolidateSources() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func TestResolveConsolidateSources_BothGivenErrors(t *testing.T) {
	if _, err := resolveConsolidateSources([]string{"tok-a1b2"}, "tok-c3d4", heldEntries(2)); err == nil {
		t.Error("expected an error when both positional args and --sources are given")
	}
}

// --- doCashConsolidate: the dial-candidate retry policy, exercised with no
// network via attemptCashConsolidateFn (a package-var seam, the same
// pattern prompt.go's own stdin uses for PromptLine/Confirm) instead of a
// real dial. Round 2 (docs/private/audit-round2-expiration-matrix.md)
// verified this loop only live; these pin the same contracts at the unit
// level.

func withFakeAttemptCashConsolidate(t *testing.T, fn func(dialToken string, sources []nipcash.Source, target nipcash.Target) (*nipcash.CashConsolidateResult, error)) {
	t.Helper()
	orig := attemptCashConsolidateFn
	attemptCashConsolidateFn = fn
	t.Cleanup(func() { attemptCashConsolidateFn = orig })
}

func testTarget() nipcash.Target {
	return nipcash.Pubkey(strings.Repeat("a1", 32))
}

func TestDoCashConsolidate_SucceedsOnFirstCandidate(t *testing.T) {
	calls := 0
	expiresAt := int64(12345)
	withFakeAttemptCashConsolidate(t, func(dialToken string, sources []nipcash.Source, target nipcash.Target) (*nipcash.CashConsolidateResult, error) {
		calls++
		return &nipcash.CashConsolidateResult{AmountMillis: 3000, NewWalletToken: "merged-token", NewWalletPubkey: "merged-pub", ExpiresAt: &expiresAt}, nil
	})

	cmd := &cobra.Command{}
	l := &ledger.Ledger{}
	newEntry, gotExpiresAt, err := doCashConsolidate(cmd, l, []string{"dial-a", "dial-b"}, nil, []string{"tok-a", "tok-b"}, testTarget())
	if err != nil {
		t.Fatalf("doCashConsolidate() error = %v", err)
	}
	if newEntry == nil || newEntry.Token != "merged-token" {
		t.Fatalf("newEntry = %+v, want the merged token", newEntry)
	}
	if newEntry.AmountMillis == nil || *newEntry.AmountMillis != 3000 {
		t.Errorf("newEntry.AmountMillis = %v, want 3000", newEntry.AmountMillis)
	}
	if gotExpiresAt == nil || *gotExpiresAt != expiresAt {
		t.Errorf("expiresAt = %v, want %d", gotExpiresAt, expiresAt)
	}
	if calls != 1 {
		t.Errorf("attemptCashConsolidateFn called %d times, want 1 (first candidate already succeeded)", calls)
	}
}

func TestDoCashConsolidate_RetriesOnExpiredThenSucceeds(t *testing.T) {
	var dialed []string
	withFakeAttemptCashConsolidate(t, func(dialToken string, sources []nipcash.Source, target nipcash.Target) (*nipcash.CashConsolidateResult, error) {
		dialed = append(dialed, dialToken)
		if dialToken == "dial-expired" {
			return nil, &relayclient.WalletError{Method: "cash_consolidate", Code: "EXPIRED", Message: "expired"}
		}
		return &nipcash.CashConsolidateResult{AmountMillis: 5000, NewWalletToken: "merged"}, nil
	})

	cmd := &cobra.Command{}
	l := &ledger.Ledger{}
	newEntry, _, err := doCashConsolidate(cmd, l, []string{"dial-expired", "dial-healthy"}, nil, nil, testTarget())
	if err != nil {
		t.Fatalf("doCashConsolidate() error = %v, want success via the healthy sibling", err)
	}
	if newEntry == nil || newEntry.Token != "merged" {
		t.Fatalf("newEntry = %+v, want the merged token from the healthy sibling", newEntry)
	}
	if want := []string{"dial-expired", "dial-healthy"}; !reflect.DeepEqual(dialed, want) {
		t.Errorf("dialed %v, want %v (retry only after an EXPIRED decline, in order)", dialed, want)
	}
}

// TestDoCashConsolidate_AllCandidatesExpiredFailsClassified mirrors
// TestCashConsolidate_AllSourcesExpired_ClassifiedAsAuth's live coverage:
// every candidate exhausted must still fail cleanly as the real classified
// EXPIRED/auth error, not hang, loop, or misreport as something else (e.g.
// "internal").
func TestDoCashConsolidate_AllCandidatesExpiredFailsClassified(t *testing.T) {
	calls := 0
	withFakeAttemptCashConsolidate(t, func(dialToken string, sources []nipcash.Source, target nipcash.Target) (*nipcash.CashConsolidateResult, error) {
		calls++
		return nil, &relayclient.WalletError{Method: "cash_consolidate", Code: "EXPIRED", Message: "expired"}
	})

	cmd := &cobra.Command{}
	l := &ledger.Ledger{}
	newEntry, expiresAt, err := doCashConsolidate(cmd, l, []string{"a", "b", "c"}, nil, nil, testTarget())
	if err == nil {
		t.Fatal("doCashConsolidate() error = nil, want the real EXPIRED error once every candidate is exhausted")
	}
	if newEntry != nil || expiresAt != nil {
		t.Errorf("got newEntry=%v expiresAt=%v, want both nil on failure", newEntry, expiresAt)
	}
	if calls != 3 {
		t.Errorf("attemptCashConsolidateFn called %d times, want 3 (every candidate tried once before giving up)", calls)
	}
	ce := output.AsCLIError(err)
	if ce.Code != output.CodeAuth || ce.NWCCode != "EXPIRED" {
		t.Errorf("classified = {code: %s, nwc_code: %s}, want {auth, EXPIRED} — AGENTS.md's error table for an expired-wallet decline", ce.Code, ce.NWCCode)
	}
}

// TestDoCashConsolidate_NonExpiredFailureStopsImmediately confirms a
// decline unrelated to which connection placed the call (e.g. a plain
// bad-request) is never retried through a sibling, even with candidates
// left — only an EXPIRED decline is about the dial choice itself.
func TestDoCashConsolidate_NonExpiredFailureStopsImmediately(t *testing.T) {
	calls := 0
	withFakeAttemptCashConsolidate(t, func(dialToken string, sources []nipcash.Source, target nipcash.Target) (*nipcash.CashConsolidateResult, error) {
		calls++
		return nil, &relayclient.WalletError{Code: "RESTRICTED"}
	})

	cmd := &cobra.Command{}
	l := &ledger.Ledger{}
	_, _, err := doCashConsolidate(cmd, l, []string{"a", "b"}, nil, nil, testTarget())
	if err == nil {
		t.Fatal("doCashConsolidate() error = nil, want the RESTRICTED failure")
	}
	if calls != 1 {
		t.Errorf("attemptCashConsolidateFn called %d times, want 1 (a non-EXPIRED decline is about the request itself, never worth retrying via a sibling connection)", calls)
	}
}

// TestDoCashConsolidate_ZeroDialCandidatesErrors guards the defensive
// check added for this otherwise-unreachable (via runCashConsolidate's own
// call site — sources/dialCandidates are always built in lockstep) edge
// case: falling through with nothing to dial must not silently report
// (nil, nil, nil) "success".
func TestDoCashConsolidate_ZeroDialCandidatesErrors(t *testing.T) {
	calls := 0
	withFakeAttemptCashConsolidate(t, func(dialToken string, sources []nipcash.Source, target nipcash.Target) (*nipcash.CashConsolidateResult, error) {
		calls++
		return nil, nil
	})

	cmd := &cobra.Command{}
	l := &ledger.Ledger{}
	newEntry, expiresAt, err := doCashConsolidate(cmd, l, nil, nil, nil, testTarget())
	if err == nil {
		t.Fatal("doCashConsolidate(no dial candidates) = nil error, want an error instead of a silent (nil, nil, nil) 'success'")
	}
	if newEntry != nil || expiresAt != nil {
		t.Errorf("got newEntry=%v expiresAt=%v, want both nil", newEntry, expiresAt)
	}
	if calls != 0 {
		t.Errorf("attemptCashConsolidateFn called %d times, want 0", calls)
	}
}
