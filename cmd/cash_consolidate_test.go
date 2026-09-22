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

// --- mergeableMinterGroups / pickMinterGroups: the no-args/no---sources
// default's auto-detection — grouping held tokens by minter (only
// same-minter sources can actually be merged) instead of naively trying
// to merge everything, and letting an interactive session choose which
// group(s) to process when more than one qualifies.

// groupableEntry builds a pubkey-mode, consolidation-eligible held entry
// (ledger.GroupableForConsolidation's own contract: known amount, known
// minter, not cashMode, not connection-key-bound) for a given minter.
func groupableEntry(id, minter string, amountMillis uint64) ledger.Entry {
	return ledger.Entry{
		ID:           id,
		Status:       ledger.StatusHeld,
		AmountMillis: ptrTo(amountMillis),
		MinterPubkey: ptrTo(minter),
	}
}

func TestMergeableMinterGroups_NothingHeldIsEmpty(t *testing.T) {
	if got := mergeableMinterGroups(nil); len(got) != 0 {
		t.Errorf("mergeableMinterGroups(nil) = %v, want empty", got)
	}
}

func TestMergeableMinterGroups_SingletonMinterExcluded(t *testing.T) {
	held := []ledger.Entry{groupableEntry("tok-a", "minter-a", 1000)}
	if got := mergeableMinterGroups(held); len(got) != 0 {
		t.Errorf("mergeableMinterGroups(1 entry, 1 minter) = %v, want empty (nothing to merge a singleton into)", got)
	}
}

func TestMergeableMinterGroups_TwoSameMinterTokensGroup(t *testing.T) {
	held := []ledger.Entry{
		groupableEntry("tok-a", "minter-a", 1000),
		groupableEntry("tok-b", "minter-a", 2000),
	}
	got := mergeableMinterGroups(held)
	if len(got) != 1 {
		t.Fatalf("mergeableMinterGroups() = %d groups, want 1", len(got))
	}
	group, ok := got["minter-a"]
	if !ok || len(group) != 2 {
		t.Fatalf("got[\"minter-a\"] = %v, want both entries", group)
	}
}

func TestMergeableMinterGroups_TwoDifferentMintersBothGroup(t *testing.T) {
	held := []ledger.Entry{
		groupableEntry("tok-a", "minter-a", 1000),
		groupableEntry("tok-b", "minter-a", 2000),
		groupableEntry("tok-c", "minter-b", 500),
		groupableEntry("tok-d", "minter-b", 700),
	}
	got := mergeableMinterGroups(held)
	if len(got) != 2 {
		t.Fatalf("mergeableMinterGroups() = %d groups, want 2 (one per minter)", len(got))
	}
	if len(got["minter-a"]) != 2 || len(got["minter-b"]) != 2 {
		t.Errorf("got = %v, want 2 entries in each minter's group", got)
	}
}

func TestMergeableMinterGroups_MixOfSingletonAndGroupableExcludesSingleton(t *testing.T) {
	held := []ledger.Entry{
		groupableEntry("tok-a", "minter-a", 1000), // alone under minter-a
		groupableEntry("tok-b", "minter-b", 500),
		groupableEntry("tok-c", "minter-b", 700),
	}
	got := mergeableMinterGroups(held)
	if len(got) != 1 {
		t.Fatalf("mergeableMinterGroups() = %d groups, want 1 (minter-a's lone token excluded)", len(got))
	}
	if _, ok := got["minter-a"]; ok {
		t.Error("minter-a (a singleton) present in result, want excluded")
	}
}

func TestMergeableMinterGroups_CashAndConnectionKeyEntriesExcluded(t *testing.T) {
	cashMode := groupableEntry("tok-a", "minter-a", 1000)
	cashMode.IdentityRequired = ptrTo(false)
	connKey := groupableEntry("tok-b", "minter-a", 1000)
	connKey.ConnectionKeyPlatform = "some-platform"
	held := []ledger.Entry{cashMode, connKey, groupableEntry("tok-c", "minter-a", 1000)}

	got := mergeableMinterGroups(held)
	if len(got) != 0 {
		t.Errorf("mergeableMinterGroups() = %v, want empty (cash/connection-key entries aren't groupable, leaving only 1 eligible entry for minter-a)", got)
	}
}

func TestPickMinterGroups_JSONModeReturnsAllWithoutPrompting(t *testing.T) {
	cmd := testCmdWithFlags(true, false)
	groups := map[string][]ledger.Entry{
		"minter-a": {groupableEntry("tok-a", "minter-a", 1000), groupableEntry("tok-b", "minter-a", 1000)},
		"minter-b": {groupableEntry("tok-c", "minter-b", 500), groupableEntry("tok-d", "minter-b", 500)},
	}
	got, err := pickMinterGroups(cmd, groups)
	if err != nil {
		t.Fatalf("pickMinterGroups() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("pickMinterGroups() = %d groups, want 2 (every qualifying group, no terminal to prompt from)", len(got))
	}
}

func TestPickMinterGroups_YesFlagReturnsAllWithoutPrompting(t *testing.T) {
	cmd := testCmdWithFlags(false, true)
	groups := map[string][]ledger.Entry{
		"minter-a": {groupableEntry("tok-a", "minter-a", 1000), groupableEntry("tok-b", "minter-a", 1000)},
		"minter-b": {groupableEntry("tok-c", "minter-b", 500), groupableEntry("tok-d", "minter-b", 500)},
	}
	got, err := pickMinterGroups(cmd, groups)
	if err != nil {
		t.Fatalf("pickMinterGroups() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("pickMinterGroups() = %d groups, want 2", len(got))
	}
}

func TestPickMinterGroups_SingleGroupNeedsNoPrompt(t *testing.T) {
	// No stdin queued at all — if this fell through to PromptLine, reading
	// from an empty reader would hang/EOF instead of just returning.
	cmd := testCmdWithFlags(false, false)
	groups := map[string][]ledger.Entry{
		"minter-a": {groupableEntry("tok-a", "minter-a", 1000), groupableEntry("tok-b", "minter-a", 1000)},
	}
	got, err := pickMinterGroups(cmd, groups)
	if err != nil {
		t.Fatalf("pickMinterGroups() error = %v", err)
	}
	if len(got) != 1 || len(got[0]) != 2 {
		t.Fatalf("pickMinterGroups() = %v, want the single group untouched", got)
	}
}

func TestPickMinterGroups_InteractiveBareEnterPicksAll(t *testing.T) {
	stubStdin(t, "\n")
	cmd := testCmdWithFlags(false, false)
	groups := map[string][]ledger.Entry{
		"minter-a": {groupableEntry("tok-a", "minter-a", 1000), groupableEntry("tok-b", "minter-a", 1000)},
		"minter-b": {groupableEntry("tok-c", "minter-b", 500), groupableEntry("tok-d", "minter-b", 500)},
	}
	var got [][]ledger.Entry
	var err error
	withStdoutSuppressed(func() {
		got, err = pickMinterGroups(cmd, groups)
	})
	if err != nil {
		t.Fatalf("pickMinterGroups() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("pickMinterGroups(bare Enter) = %d groups, want 2 (all)", len(got))
	}
}

func TestPickMinterGroups_InteractivePicksSpecificByNumber(t *testing.T) {
	stubStdin(t, "2\n")
	cmd := testCmdWithFlags(false, false)
	groups := map[string][]ledger.Entry{
		"minter-a": {groupableEntry("tok-a", "minter-a", 1000), groupableEntry("tok-b", "minter-a", 1000)},
		"minter-b": {groupableEntry("tok-c", "minter-b", 500), groupableEntry("tok-d", "minter-b", 500)},
	}
	var got [][]ledger.Entry
	var err error
	withStdoutSuppressed(func() {
		got, err = pickMinterGroups(cmd, groups)
	})
	if err != nil {
		t.Fatalf("pickMinterGroups() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("pickMinterGroups(\"2\") = %d groups, want 1", len(got))
	}
	// sortedMinterKeys orders alphabetically: minter-a=1, minter-b=2.
	if got[0][0].MinterPubkey == nil || *got[0][0].MinterPubkey != "minter-b" {
		t.Errorf("pickMinterGroups(\"2\") picked minter %v, want minter-b", got[0][0].MinterPubkey)
	}
}

func TestPickMinterGroups_InvalidChoiceErrors(t *testing.T) {
	stubStdin(t, "9\n")
	cmd := testCmdWithFlags(false, false)
	groups := map[string][]ledger.Entry{
		"minter-a": {groupableEntry("tok-a", "minter-a", 1000), groupableEntry("tok-b", "minter-a", 1000)},
		"minter-b": {groupableEntry("tok-c", "minter-b", 500), groupableEntry("tok-d", "minter-b", 500)},
	}
	var err error
	withStdoutSuppressed(func() {
		_, err = pickMinterGroups(cmd, groups)
	})
	if err == nil {
		t.Fatal("expected an error for an out-of-range choice")
	}
}

// TestPickMinterGroups_NeverPrintsRawID is the consolidate-side regression
// guard docs/private/wallet-abstraction-plan.md calls for: a human
// choosing among minter groups sees index + token count + total amount,
// never a raw ledger.Entry.ID (mirrors TestPickHeldToken_NeverPrintsRawID
// in cash_redeem_test.go for the identical concern in the single-select
// picker).
func TestPickMinterGroups_NeverPrintsRawID(t *testing.T) {
	stubStdin(t, "1\n")
	cmd := testCmdWithFlags(false, false)
	groups := map[string][]ledger.Entry{
		"minter-a": {groupableEntry("tok-a", "minter-a", 1000), groupableEntry("tok-b", "minter-a", 1000)},
		"minter-b": {groupableEntry("tok-c", "minter-b", 500), groupableEntry("tok-d", "minter-b", 500)},
	}
	printed := withCapturedOutput(func() {
		_, _ = pickMinterGroups(cmd, groups)
	})
	if strings.Contains(printed, "tok-") {
		t.Fatalf("pickMinterGroups printed a raw ledger ID:\n%s", printed)
	}
	if !strings.Contains(printed, "2 tokens") {
		t.Fatalf("pickMinterGroups didn't print the expected group summary:\n%s", printed)
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
	newEntry, gotExpiresAt, err := doCashConsolidate(cmd, l, []string{"dial-a", "dial-b"}, nil, []string{"tok-a", "tok-b"}, testTarget(), true)
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
	newEntry, _, err := doCashConsolidate(cmd, l, []string{"dial-expired", "dial-healthy"}, nil, nil, testTarget(), true)
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
	newEntry, expiresAt, err := doCashConsolidate(cmd, l, []string{"a", "b", "c"}, nil, nil, testTarget(), true)
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
	_, _, err := doCashConsolidate(cmd, l, []string{"a", "b"}, nil, nil, testTarget(), true)
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
	newEntry, expiresAt, err := doCashConsolidate(cmd, l, nil, nil, nil, testTarget(), true)
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

// TestDoCashConsolidate_ThirdPartyPubkeyTargetNotSavedToLedger locks in the
// "must not save someone else's gift" guard doCashConsolidate's own doc
// comment describes (cash_consolidate.go: "the caller doesn't own the
// resulting wallet and can't redeem it themselves, so it must not be saved
// as one of the caller's own held tokens") — every other doCashConsolidate
// test in this file passes isSelfTarget=true, so this exact branch
// (isSelfTarget=false, a real pubkey target) had no unit coverage at all
// before this test: a regression here (e.g. the isSelfTarget||isCashTarget
// check getting inverted or dropped) would have shipped silently, leaving a
// consolidate-to-a-third-party phantom-save the caller's own held funds.
func TestDoCashConsolidate_ThirdPartyPubkeyTargetNotSavedToLedger(t *testing.T) {
	withFakeAttemptCashConsolidate(t, func(dialToken string, sources []nipcash.Source, target nipcash.Target) (*nipcash.CashConsolidateResult, error) {
		return &nipcash.CashConsolidateResult{AmountMillis: 3000, NewWalletToken: "gift-token", NewWalletPubkey: "gift-pub"}, nil
	})

	cmd := &cobra.Command{}
	l := &ledger.Ledger{}
	newEntry, _, err := doCashConsolidate(cmd, l, []string{"dial-a"}, nil, []string{"tok-a", "tok-b"}, testTarget(), false)
	if err != nil {
		t.Fatalf("doCashConsolidate() error = %v", err)
	}
	// Still returned, for display/hand-off to the recipient — just not
	// persisted as one of the caller's own.
	if newEntry == nil || newEntry.Token != "gift-token" {
		t.Fatalf("newEntry = %+v, want the gift token (still returned for hand-off)", newEntry)
	}
	if len(l.Entries) != 0 {
		t.Errorf("l.Entries = %+v, want empty — a non-self, identity-bound consolidate target must never be saved as the caller's own held token", l.Entries)
	}
	if _, ok := l.FindByToken("gift-token"); ok {
		t.Error("the gift token was found in the caller's own ledger — it belongs to the recipient, not the caller")
	}
}

// --- sharedMinter: what a derived token (a split's remainder, a
// consolidate's merged output) inherits as its MinterPubkey. Must never
// claim a minter that wasn't verified for every source.

func TestSharedMinter(t *testing.T) {
	a, b := "minter-a", "minter-b"
	cases := []struct {
		name    string
		entries []ledger.Entry
		want    *string
	}{
		{"all verified and agree", []ledger.Entry{{MinterPubkey: &a}, {MinterPubkey: &a}}, &a},
		{"single verified source", []ledger.Entry{{MinterPubkey: &b}}, &b},
		{"one unverified source poisons it", []ledger.Entry{{MinterPubkey: &a}, {}}, nil},
		{"different minters", []ledger.Entry{{MinterPubkey: &a}, {MinterPubkey: &b}}, nil},
		{"no entries", nil, nil},
	}
	for _, c := range cases {
		got := sharedMinter(c.entries)
		switch {
		case got == nil && c.want == nil:
		case got == nil || c.want == nil || *got != *c.want:
			t.Errorf("%s: sharedMinter() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSharedMinter_ReturnsACopy(t *testing.T) {
	// The result must not alias a source entry's own pointer: the derived
	// entry outlives (and is saved independently of) its sources.
	a := "minter-a"
	entries := []ledger.Entry{{MinterPubkey: &a}}
	got := sharedMinter(entries)
	if got == entries[0].MinterPubkey {
		t.Error("sharedMinter() returned the source entry's own pointer, want an independent copy")
	}
}

func TestSharedMinterOfIDs(t *testing.T) {
	l := &ledger.Ledger{}
	m := "minter-a"
	e1, _ := l.Add(ledger.Entry{Token: "t1", MinterPubkey: &m})
	e2, _ := l.Add(ledger.Entry{Token: "t2", MinterPubkey: &m})
	if got := sharedMinterOfIDs(l, []string{e1.ID, e2.ID}); got == nil || *got != m {
		t.Errorf("sharedMinterOfIDs() = %v, want %q", got, m)
	}
	if got := sharedMinterOfIDs(l, []string{e1.ID, "no-such-id"}); got != nil {
		t.Errorf("sharedMinterOfIDs() with an unknown id = %v, want nil", *got)
	}
}
