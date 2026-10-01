package cmd

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// --- mergeableHubGroups / pickHubGroups: the no-args/no---sources
// default's auto-detection — grouping held tokens by minter (only
// same-minter sources can actually be merged) instead of naively trying
// to merge everything, and letting an interactive session choose which
// group(s) to process when more than one qualifies.

// groupableEntry builds a pubkey-mode, consolidation-eligible held entry
// (ledger.GroupableForConsolidation's own contract: known amount, known issuing
// Hub, not cashMode, not connection-key-bound) for a given Hub.
//
// The entry carries a real token, because grouping reads the issuing Hub out of the
// token rather than from a column. That is the change these tests exist to pin: a
// mint signature names the minting NODE, and one node routinely runs several Hubs,
// so same-minter grouping merged bills the Hub then refused.
func groupableEntry(id, hub string, amountMillis uint64) ledger.Entry {
	return ledger.Entry{
		ID:           id,
		Status:       ledger.StatusHeld,
		AmountMillis: ptrTo(amountMillis),
		MinterPubkey: ptrTo("minter-shared"),
		Token:        hubToken(hub),
	}
}

// hubToken builds a real token whose hub-group fingerprint derives from hub.
//
// Note every groupableEntry shares ONE minter above, deliberately: on a real
// deployment that is the normal case (one node, several Hubs), and it is exactly the
// case the old grouping got wrong. If grouping ever reverted to the minter, every
// multi-Hub test here would collapse into a single group and fail.
func hubToken(hub string) string {
	amt := uint64(1)
	tok, err := nipcash.Encode(nipcash.Token{
		HRP:                  "lokicash",
		WalletPubkey:         hex.EncodeToString(bytes.Repeat([]byte{0xab}, 32)),
		Secret:               hex.EncodeToString(bytes.Repeat([]byte{0xcd}, 32)),
		RelayURLs:            []string{"wss://relay.test"},
		MintSignature:        bytes.Repeat([]byte{0x01}, 65),
		AttestedAmountMillis: &amt,
		HubGroup:             nipcash.HubGroupFor(hub),
	})
	if err != nil {
		panic(err)
	}
	return tok
}

// hubKey is the map key groups are bucketed under: the fingerprint, hex-encoded.
func hubKey(hub string) string {
	return hex.EncodeToString(nipcash.HubGroupFor(hub))
}

func TestMergeableHubGroups_NothingHeldIsEmpty(t *testing.T) {
	if got := mergeableHubGroups(nil); len(got) != 0 {
		t.Errorf("mergeableHubGroups(nil) = %v, want empty", got)
	}
}

func TestMergeableHubGroups_SingletonHubExcluded(t *testing.T) {
	held := []ledger.Entry{groupableEntry("tok-a", "hub-a", 1000)}
	if got := mergeableHubGroups(held); len(got) != 0 {
		t.Errorf("mergeableHubGroups(1 entry, 1 Hub) = %v, want empty (nothing to merge a singleton into)", got)
	}
}

func TestMergeableHubGroups_TwoSameHubTokensGroup(t *testing.T) {
	held := []ledger.Entry{
		groupableEntry("tok-a", "hub-a", 1000),
		groupableEntry("tok-b", "hub-a", 2000),
	}
	got := mergeableHubGroups(held)
	if len(got) != 1 {
		t.Fatalf("mergeableHubGroups() = %d groups, want 1", len(got))
	}
	group, ok := got[hubKey("hub-a")]
	if !ok || len(group) != 2 {
		t.Fatalf("got[hub-a] = %v, want both entries", group)
	}
}

func TestMergeableHubGroups_TwoDifferentHubsBothGroup(t *testing.T) {
	held := []ledger.Entry{
		groupableEntry("tok-a", "hub-a", 1000),
		groupableEntry("tok-b", "hub-a", 2000),
		groupableEntry("tok-c", "hub-b", 500),
		groupableEntry("tok-d", "hub-b", 700),
	}
	got := mergeableHubGroups(held)
	if len(got) != 2 {
		t.Fatalf("mergeableHubGroups() = %d groups, want 2 (one per Hub)", len(got))
	}
	if len(got[hubKey("hub-a")]) != 2 || len(got[hubKey("hub-b")]) != 2 {
		t.Errorf("got = %v, want 2 entries in each Hub's group", got)
	}
}

func TestMergeableHubGroups_MixOfSingletonAndGroupableExcludesSingleton(t *testing.T) {
	held := []ledger.Entry{
		groupableEntry("tok-a", "hub-a", 1000), // alone under hub-a
		groupableEntry("tok-b", "hub-b", 500),
		groupableEntry("tok-c", "hub-b", 700),
	}
	got := mergeableHubGroups(held)
	if len(got) != 1 {
		t.Fatalf("mergeableHubGroups() = %d groups, want 1 (hub-a's lone token excluded)", len(got))
	}
	if _, ok := got[hubKey("hub-a")]; ok {
		t.Error("hub-a (a singleton) present in result, want excluded")
	}
}

func TestMergeableHubGroups_CashAndConnectionKeyEntriesExcluded(t *testing.T) {
	cashMode := groupableEntry("tok-a", "hub-a", 1000)
	cashMode.IdentityRequired = ptrTo(false)
	connKey := groupableEntry("tok-b", "hub-a", 1000)
	connKey.ConnectionKeyPlatform = "some-platform"
	held := []ledger.Entry{cashMode, connKey, groupableEntry("tok-c", "hub-a", 1000)}

	got := mergeableHubGroups(held)
	if len(got) != 0 {
		t.Errorf("mergeableHubGroups() = %v, want empty (cash/connection-key entries aren't groupable, leaving only 1 eligible entry for hub-a)", got)
	}
}

func TestPickHubGroups_JSONModeReturnsAllWithoutPrompting(t *testing.T) {
	cmd := testCmdWithFlags(true, false)
	groups := map[string][]ledger.Entry{
		hubKey("hub-a"): {groupableEntry("tok-a", "hub-a", 1000), groupableEntry("tok-b", "hub-a", 1000)},
		hubKey("hub-b"): {groupableEntry("tok-c", "hub-b", 500), groupableEntry("tok-d", "hub-b", 500)},
	}
	got, err := pickHubGroups(cmd, groups)
	if err != nil {
		t.Fatalf("pickHubGroups() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("pickHubGroups() = %d groups, want 2 (every qualifying group, no terminal to prompt from)", len(got))
	}
}

func TestPickHubGroups_YesFlagReturnsAllWithoutPrompting(t *testing.T) {
	cmd := testCmdWithFlags(false, true)
	groups := map[string][]ledger.Entry{
		hubKey("hub-a"): {groupableEntry("tok-a", "hub-a", 1000), groupableEntry("tok-b", "hub-a", 1000)},
		hubKey("hub-b"): {groupableEntry("tok-c", "hub-b", 500), groupableEntry("tok-d", "hub-b", 500)},
	}
	got, err := pickHubGroups(cmd, groups)
	if err != nil {
		t.Fatalf("pickHubGroups() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("pickHubGroups() = %d groups, want 2", len(got))
	}
}

func TestPickHubGroups_SingleGroupNeedsNoPrompt(t *testing.T) {
	// No stdin queued at all — if this fell through to PromptLine, reading
	// from an empty reader would hang/EOF instead of just returning.
	cmd := testCmdWithFlags(false, false)
	groups := map[string][]ledger.Entry{
		"minter-a": {groupableEntry("tok-a", "hub-a", 1000), groupableEntry("tok-b", "hub-a", 1000)},
	}
	got, err := pickHubGroups(cmd, groups)
	if err != nil {
		t.Fatalf("pickHubGroups() error = %v", err)
	}
	if len(got) != 1 || len(got[0]) != 2 {
		t.Fatalf("pickHubGroups() = %v, want the single group untouched", got)
	}
}

func TestPickHubGroups_InteractiveBareEnterPicksAll(t *testing.T) {
	stubStdin(t, "\n")
	cmd := testCmdWithFlags(false, false)
	groups := map[string][]ledger.Entry{
		hubKey("hub-a"): {groupableEntry("tok-a", "hub-a", 1000), groupableEntry("tok-b", "hub-a", 1000)},
		hubKey("hub-b"): {groupableEntry("tok-c", "hub-b", 500), groupableEntry("tok-d", "hub-b", 500)},
	}
	var got [][]ledger.Entry
	var err error
	withStdoutSuppressed(func() {
		got, err = pickHubGroups(cmd, groups)
	})
	if err != nil {
		t.Fatalf("pickHubGroups() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("pickHubGroups(bare Enter) = %d groups, want 2 (all)", len(got))
	}
}

func TestPickHubGroups_InteractivePicksSpecificByNumber(t *testing.T) {
	stubStdin(t, "2\n")
	cmd := testCmdWithFlags(false, false)
	groups := map[string][]ledger.Entry{
		hubKey("hub-a"): {groupableEntry("tok-a", "hub-a", 1000), groupableEntry("tok-b", "hub-a", 1000)},
		hubKey("hub-b"): {groupableEntry("tok-c", "hub-b", 500), groupableEntry("tok-d", "hub-b", 500)},
	}
	// Which Hub is offered as "2" depends on how the fingerprints sort, not on the
	// names this test gave them — so the expectation is derived the same way the
	// picker derives its numbering. Hard-coding it would make the test depend on the
	// hash of a string, which is not what it is asserting.
	secondKey := sortedHubKeys(groups)[1]
	wantIDs := map[string]bool{}
	for _, e := range groups[secondKey] {
		wantIDs[e.ID] = true
	}

	var got [][]ledger.Entry
	var err error
	withStdoutSuppressed(func() {
		got, err = pickHubGroups(cmd, groups)
	})
	if err != nil {
		t.Fatalf("pickHubGroups() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("pickHubGroups(\"2\") = %d groups, want 1", len(got))
	}
	if len(got[0]) != 2 {
		t.Fatalf("picked group has %d entries, want 2", len(got[0]))
	}
	for _, e := range got[0] {
		if !wantIDs[e.ID] {
			t.Errorf("pickHubGroups(\"2\") picked entry %q, which is not in the second Hub's group", e.ID)
		}
	}
}

func TestPickHubGroups_InvalidChoiceErrors(t *testing.T) {
	stubStdin(t, "9\n")
	cmd := testCmdWithFlags(false, false)
	groups := map[string][]ledger.Entry{
		hubKey("hub-a"): {groupableEntry("tok-a", "hub-a", 1000), groupableEntry("tok-b", "hub-a", 1000)},
		hubKey("hub-b"): {groupableEntry("tok-c", "hub-b", 500), groupableEntry("tok-d", "hub-b", 500)},
	}
	var err error
	withStdoutSuppressed(func() {
		_, err = pickHubGroups(cmd, groups)
	})
	if err == nil {
		t.Fatal("expected an error for an out-of-range choice")
	}
}

// TestPickHubGroups_NeverPrintsRawID is the consolidate-side regression
// guard docs/private/wallet-abstraction-plan.md calls for: a human
// choosing among minter groups sees index + token count + total amount,
// never a raw ledger.Entry.ID (mirrors TestPickHeldToken_NeverPrintsRawID
// in cash_redeem_test.go for the identical concern in the single-select
// picker).
func TestPickHubGroups_NeverPrintsRawID(t *testing.T) {
	stubStdin(t, "1\n")
	cmd := testCmdWithFlags(false, false)
	groups := map[string][]ledger.Entry{
		hubKey("hub-a"): {groupableEntry("tok-a", "hub-a", 1000), groupableEntry("tok-b", "hub-a", 1000)},
		hubKey("hub-b"): {groupableEntry("tok-c", "hub-b", 500), groupableEntry("tok-d", "hub-b", 500)},
	}
	printed := withCapturedOutput(func() {
		_, _ = pickHubGroups(cmd, groups)
	})
	if strings.Contains(printed, "tok-") {
		t.Fatalf("pickHubGroups printed a raw ledger ID:\n%s", printed)
	}
	if !strings.Contains(printed, "2 tokens") {
		t.Fatalf("pickHubGroups didn't print the expected group summary:\n%s", printed)
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

// --- consolidateGroups / printConsolidateOutcomes: the multi-group run's own
// policy — attempt every group, report every group. The regression guard here is
// for a bug with real consequences: the loop used to `return err` on the first
// failing group, so a failure on group 2 of 3 left group 1 already merged on the
// Hub, group 3 never attempted, and nothing printed but group 2's error. The
// newly minted token from group 1 was never named anywhere.

func withFakeConsolidateItems(t *testing.T, fn func(cmd *cobra.Command, l *ledger.Ledger, items []string, toFlag string, jsonMode, yesFlag bool) (*consolidateResult, error)) {
	t.Helper()
	orig := consolidateItemsFn
	consolidateItemsFn = fn
	t.Cleanup(func() { consolidateItemsFn = orig })
}

func consolidateOK(amountMillis uint64) *consolidateResult {
	return &consolidateResult{
		NewEntry:       &ledger.Entry{ID: "new-entry", Token: "merged-token", AmountMillis: ptrTo(amountMillis)},
		TargetResolved: "self",
	}
}

// twoGroups is the minimal shape that exercises the bug: two groups, each with
// two same-minter sources.
func twoGroups() [][]ledger.Entry {
	return [][]ledger.Entry{
		{groupableEntry("tok-a1", "hub-a", 1000), groupableEntry("tok-a2", "hub-a", 2000)},
		{groupableEntry("tok-b1", "hub-b", 3000), groupableEntry("tok-b2", "hub-b", 4000)},
	}
}

func TestConsolidateGroups_FailureInOneGroupDoesNotStopTheNext(t *testing.T) {
	var attempted []string
	withFakeConsolidateItems(t, func(_ *cobra.Command, _ *ledger.Ledger, items []string, _ string, _, _ bool) (*consolidateResult, error) {
		attempted = append(attempted, items[0])
		if items[0] == "tok-a1" {
			return nil, errors.New("hub said no")
		}
		return consolidateOK(7000), nil
	})

	outcomes := consolidateGroups(&cobra.Command{}, &ledger.Ledger{}, twoGroups(), "", true, true)

	if len(attempted) != 2 {
		t.Fatalf("attempted %d groups (%v), want both — a failing group must not cancel the rest", len(attempted), attempted)
	}
	if len(outcomes) != 2 {
		t.Fatalf("len(outcomes) = %d, want 2 (one per group, whatever happened)", len(outcomes))
	}
	if outcomes[0].Err == nil {
		t.Errorf("outcomes[0].Err = nil, want the first group's failure")
	}
	if outcomes[1].Result == nil {
		t.Errorf("outcomes[1].Result = nil, want the second group to have gone ahead anyway")
	}
	// The Hub fingerprint is what names a group in output; without it a failure is
	// not actionable when several groups are in play. It replaced the shared MINTER,
	// which became useless for this the moment groups became Hubs: one node runs
	// several Hubs, so every group would have reported the same minter.
	if outcomes[0].Hub != hubKey("hub-a") || outcomes[1].Hub != hubKey("hub-b") {
		t.Errorf("hubs = (%q, %q), want (%q, %q)",
			outcomes[0].Hub, outcomes[1].Hub, hubKey("hub-a"), hubKey("hub-b"))
	}
	if outcomes[0].Hub == outcomes[1].Hub {
		t.Error("both groups reported the same Hub; they are different Hubs and must be distinguishable in output")
	}
}

// TestConsolidateGroups_SucceededGroupIsStillReportedAfterALaterFailure is the
// money-losing case stated directly: group 1 merges, group 2 fails, and group 1's
// new token must still reach the output. Before the fix it was silently dropped.
func TestConsolidateGroups_SucceededGroupIsStillReportedAfterALaterFailure(t *testing.T) {
	withFakeConsolidateItems(t, func(_ *cobra.Command, _ *ledger.Ledger, items []string, _ string, _, _ bool) (*consolidateResult, error) {
		if items[0] == "tok-b1" {
			return nil, errors.New("relay went away")
		}
		return consolidateOK(3000), nil
	})

	outcomes := consolidateGroups(&cobra.Command{}, &ledger.Ledger{}, twoGroups(), "", true, true)
	out := withCapturedStdout(func() { printConsolidateOutcomes(true, outcomes) })

	if !strings.Contains(out, "merged-token") {
		t.Errorf("output does not name the token group 1 actually minted:\n%s", out)
	}
	if !strings.Contains(out, `"status": "failed"`) {
		t.Errorf("output does not report group 2 as failed:\n%s", out)
	}
	if !strings.Contains(out, `"status": "ok"`) {
		t.Errorf("output does not report group 1 as ok:\n%s", out)
	}
}

// TestConsolidateGroups_DeclineIsNeitherSuccessNorFailure pins the third state.
// A person saying no at the prompt is a choice, not a fault: it must not be
// reported as failed (which would imply something went wrong and invite a retry)
// and must not be reported as ok (which would imply a merge happened).
func TestConsolidateGroups_DeclineIsNeitherSuccessNorFailure(t *testing.T) {
	withFakeConsolidateItems(t, func(_ *cobra.Command, _ *ledger.Ledger, items []string, _ string, _, _ bool) (*consolidateResult, error) {
		if items[0] == "tok-a1" {
			return nil, nil // declined
		}
		return consolidateOK(7000), nil
	})

	outcomes := consolidateGroups(&cobra.Command{}, &ledger.Ledger{}, twoGroups(), "", false, false)
	if !outcomes[0].declined() {
		t.Errorf("outcomes[0].declined() = false, want true for a nil/nil group")
	}
	if err := firstOutcomeError(outcomes); err != nil {
		t.Errorf("firstOutcomeError = %v, want nil — a decline is not a failure and must not set an exit code", err)
	}
	out := withCapturedStdout(func() { printConsolidateOutcomes(true, outcomes) })
	if !strings.Contains(out, `"status": "declined"`) {
		t.Errorf("declined group not reported as such:\n%s", out)
	}
}

// TestPrintConsolidateOutcomes_SingleSuccessKeepsTheLegacyJSONShape guards the
// --json contract. Every existing consumer parses a top-level "new_entry"
// (integration/ reads consolidateResp["new_entry"] directly), so the common
// one-group case must keep emitting exactly that object. The richer per-group
// array is only for runs with several groups or a non-success — states where this
// command previously printed no result at all, so nothing can depend on them.
func TestPrintConsolidateOutcomes_SingleSuccessKeepsTheLegacyJSONShape(t *testing.T) {
	outcomes := []consolidateOutcome{{Hub: hubKey("hub-a"), IDs: []string{"tok-a1", "tok-a2"}, Result: consolidateOK(3000)}}
	out := withCapturedStdout(func() { printConsolidateOutcomes(true, outcomes) })

	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not one JSON object: %v\n%s", err, out)
	}
	if _, ok := got["new_entry"]; !ok {
		t.Errorf("single success lost its top-level new_entry — this breaks every --json consumer:\n%s", out)
	}
	if _, ok := got["consolidated"]; ok {
		t.Errorf("single success used the array shape, changing the established contract:\n%s", out)
	}
}

// TestFirstOutcomeError_PreservesClassification confirms the returned error is
// the group's OWN error, not a wrapper. The exit code and the --json "code"
// field both come from it, so re-wrapping would flatten a network failure (exit
// 6, retryable) into a generic internal one (exit 1, not retryable).
func TestFirstOutcomeError_PreservesClassification(t *testing.T) {
	classified := output.NetworkError(&cobra.Command{}, errors.New("relay unreachable"))
	outcomes := []consolidateOutcome{
		{Hub: hubKey("hub-a"), Result: consolidateOK(1000)},
		{Hub: hubKey("hub-b"), Err: classified},
	}
	got := firstOutcomeError(outcomes)
	if got == nil {
		t.Fatal("firstOutcomeError = nil, want the failing group's error")
	}
	if output.ExitCode(got) != output.ExitCode(classified) {
		t.Errorf("exit code = %d, want %d — classification was lost", output.ExitCode(got), output.ExitCode(classified))
	}
	if output.AsCLIError(got).Code != "network" {
		t.Errorf("code = %q, want network", output.AsCLIError(got).Code)
	}
}

// The cross-Hub refusal, recognised AFTER classification.
//
// The bug these cover is a silent one: isCrossHubSourcesDecline matches a raw
// *relayclient.WalletError, but consolidateItemsFn returns an error already run
// through NWCErrorForCashToken, which builds a fresh CLIError around a plainError
// and drops the WalletError from the chain. errors.As then never matches, the
// mapping never fires, and the user sees the raw protocol text — with no failing
// test anywhere, because nothing exercised the classified shape.
func TestIsCrossHubDeclineClassified_MatchesBothShapes(t *testing.T) {
	raw := &relayclient.WalletError{
		Method:  "cash_consolidate",
		Code:    "BAD_REQUEST",
		Message: "all sources must belong to the same Cash Hub",
	}
	if !isCrossHubDeclineClassified(raw) {
		t.Error("a raw WalletError must still match")
	}

	// The shape that actually reaches consolidateGroups.
	classified := output.NWCErrorForCashToken(&cobra.Command{}, raw)
	if !isCrossHubDeclineClassified(classified) {
		t.Error("a CLIError-classified refusal must match — this is the shape consolidateItemsFn returns, and the one that silently did not match")
	}

	// And it must not match unrelated failures.
	for _, other := range []*relayclient.WalletError{
		{Method: "cash_consolidate", Code: "QUOTA_EXCEEDED", Message: "amount exceeds the per-wallet maximum"},
		{Method: "cash_consolidate", Code: "NOT_FOUND", Message: "no slice registered for this identity"},
	} {
		if isCrossHubDeclineClassified(other) {
			t.Errorf("%s must not be read as a cross-Hub refusal", other.Code)
		}
		if isCrossHubDeclineClassified(output.NWCErrorForCashToken(&cobra.Command{}, other)) {
			t.Errorf("classified %s must not be read as a cross-Hub refusal", other.Code)
		}
	}
}

// consolidateGroups must REPLACE a cross-Hub refusal with the explanation, and
// leave every other failure's own classification untouched.
func TestConsolidateGroups_ExplainsACrossHubRefusal(t *testing.T) {
	orig := consolidateItemsFn
	defer func() { consolidateItemsFn = orig }()

	consolidateItemsFn = func(cmd *cobra.Command, l *ledger.Ledger, ids []string, toFlag string, jsonMode, yesFlag bool) (*consolidateResult, error) {
		return nil, output.NWCErrorForCashToken(cmd, &relayclient.WalletError{
			Method:  "cash_consolidate",
			Code:    "BAD_REQUEST",
			Message: "all sources must belong to the same Cash Hub",
		})
	}

	outcomes := consolidateGroups(&cobra.Command{}, &ledger.Ledger{}, twoGroups(), "", true, true)
	if len(outcomes) != 2 {
		t.Fatalf("want an outcome per group, got %d", len(outcomes))
	}
	for i, o := range outcomes {
		if o.Err == nil {
			t.Fatalf("group %d: want a failure", i)
		}
		msg := o.Err.Error()
		for _, want := range []string{"same Lightning node", "Nothing was changed", "--sources"} {
			if !strings.Contains(msg, want) {
				t.Errorf("group %d error = %q, want it to contain %q", i, msg, want)
			}
		}
		if ce := output.AsCLIError(o.Err); ce == nil || ce.Code != output.CodeInvalidInput {
			t.Errorf("group %d: want invalid_input classification, got %+v", i, ce)
		}
	}
}

// An unrelated failure must pass through with its own classification and text —
// the mapping must not swallow every error into the cross-Hub explanation.
func TestConsolidateGroups_LeavesOtherFailuresAlone(t *testing.T) {
	orig := consolidateItemsFn
	defer func() { consolidateItemsFn = orig }()

	consolidateItemsFn = func(cmd *cobra.Command, l *ledger.Ledger, ids []string, toFlag string, jsonMode, yesFlag bool) (*consolidateResult, error) {
		return nil, output.NWCErrorForCashToken(cmd, &relayclient.WalletError{
			Method:  "cash_consolidate",
			Code:    "NOT_FOUND",
			Message: "no slice registered for this identity",
		})
	}

	outcomes := consolidateGroups(&cobra.Command{}, &ledger.Ledger{}, twoGroups(), "", true, true)
	for i, o := range outcomes {
		if o.Err == nil {
			t.Fatalf("group %d: want a failure", i)
		}
		if strings.Contains(o.Err.Error(), "same Lightning node") {
			t.Errorf("group %d: an unrelated failure was rewritten as a cross-Hub refusal: %v", i, o.Err)
		}
		if ce := output.AsCLIError(o.Err); ce == nil || ce.NWCCode != "NOT_FOUND" {
			t.Errorf("group %d: the original NWC code must survive, got %+v", i, ce)
		}
	}
}
