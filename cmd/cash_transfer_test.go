package cmd

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/credential"
	"github.com/ohstr/cashctl/internal/identity"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// newTestTransferCmd builds a *cobra.Command carrying the same flags
// newCashTransferCmd registers on the real command tree — resolveTarget/
// transferWithAutoConsolidate/Confirm all read these directly off cmd,
// mirroring newTestDecodeCmd's own reasoning in decode_test.go.
func newTestTransferCmd() *cobra.Command {
	c := &cobra.Command{}
	c.Flags().Bool("json", false, "")
	c.Flags().Bool("yes", false, "")
	c.Flags().String("token", "", "")
	c.Flags().String("to", "", "")
	c.Flags().String("amount", "", "")
	c.Flags().String("as", "", "")
	c.Flags().String("ia", "", "")
	return c
}

func nconnectionForTest(t *testing.T) string {
	t.Helper()
	return "nconnection1qqs2u2jjqsyulm0ruq4xpx2xgxp3jyrxwsemlmsm5vudxv92sqz4tjgpzamhxue69uhhyetvv9ujuetcv9khqmr99e3k7mgzqajxjumrdaexg3g904n"
}

func TestResolveTarget_NConnectionJSONModeErrorsNamingIA(t *testing.T) {
	c := newTestTransferCmd()
	if err := c.Flags().Set("json", "true"); err != nil {
		t.Fatal(err)
	}
	// No stdin queued — if this fell through to the wizard prompt, reading
	// from an empty reader would hang/EOF instead of erroring cleanly.
	_, err := resolveTarget(c, nconnectionForTest(t))
	if err == nil {
		t.Fatal("resolveTarget(nconnection, --json, no --ia) = nil error, want a usage error naming --ia")
	}
	if !strings.Contains(err.Error(), "--ia") {
		t.Errorf("error = %q, want it to mention --ia", err.Error())
	}
}

func TestResolveTarget_NConnectionWithIAFlagResolves(t *testing.T) {
	c := newTestTransferCmd()
	if err := c.Flags().Set("json", "true"); err != nil {
		t.Fatal(err)
	}
	if err := c.Flags().Set("ia", "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"); err != nil {
		t.Fatal(err)
	}
	rt, err := resolveTarget(c, nconnectionForTest(t))
	if err != nil {
		t.Fatalf("resolveTarget(nconnection, --ia <hex>) error = %v", err)
	}
	if rt.Target == nil {
		t.Fatal("resolveTarget() returned a nil Target")
	}
	if rt.Resolved == "" {
		t.Error("resolveTarget() Resolved is empty, want the resolved IA surfaced before it's trusted")
	}
}

func TestResolveTarget_NConnectionInteractiveWizardResolves(t *testing.T) {
	c := newTestTransferCmd()
	withStdin(t, "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1\n")
	rt, err := resolveTarget(c, nconnectionForTest(t))
	if err != nil {
		t.Fatalf("resolveTarget(nconnection, interactive wizard) error = %v", err)
	}
	if rt.Target == nil {
		t.Fatal("resolveTarget() returned a nil Target")
	}
}

func TestResolveTarget_NConnectionInteractiveEmptyLineIsUsageError(t *testing.T) {
	c := newTestTransferCmd()
	withStdin(t, "\n") // bare Enter: declines to supply an IA
	_, err := resolveTarget(c, nconnectionForTest(t))
	if err == nil {
		t.Fatal("resolveTarget(nconnection, bare Enter) = nil error, want a usage error")
	}
	if !strings.Contains(err.Error(), "--ia") {
		t.Errorf("error = %q, want it to mention --ia", err.Error())
	}
}

func TestResolveTarget_PlainTargetUnaffected(t *testing.T) {
	c := newTestTransferCmd()
	rt, err := resolveTarget(c, "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1")
	if err != nil {
		t.Fatalf("resolveTarget(bare hex) error = %v", err)
	}
	if rt.Target == nil {
		t.Fatal("resolveTarget() returned a nil Target")
	}
	if rt.Resolved != "" {
		t.Errorf("resolveTarget(bare hex) Resolved = %q, want empty", rt.Resolved)
	}
}

// --- shouldPrintResolvedTarget: the pre-confirm "resolves to:" gate.
// A bearer target's Resolved carries the freshly generated secret (needed
// intact for --json's target_resolved field — pinned by
// TestParseTarget_BearerTarget_ResolvedSurfacesSecret in
// internal/credential), but printing it before anything is confirmed is
// pure noise since it's shown again anyway, combined with the resulting
// token, once the transfer completes. These two tests pin both halves at
// once: the data survives untouched, only the premature print is gated.

func TestShouldPrintResolvedTarget_BearerTargetIsFalse(t *testing.T) {
	c := newTestTransferCmd()
	rt, err := resolveTarget(c, "bearer-target")
	if err != nil {
		t.Fatalf("resolveTarget(bearer-target) error = %v", err)
	}
	if rt.Resolved == "" {
		t.Fatal("resolveTarget(bearer-target) Resolved is empty — the generated secret would be unrecoverable (see credential.go's own ParseTarget)")
	}
	if shouldPrintResolvedTarget(rt) {
		t.Error("shouldPrintResolvedTarget(bearer target) = true, want false — the secret shouldn't be printed before anything is confirmed")
	}
}

func TestShouldPrintResolvedTarget_NonBearerResolutionIsTrue(t *testing.T) {
	c := newTestTransferCmd()
	if err := c.Flags().Set("ia", "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"); err != nil {
		t.Fatal(err)
	}
	rt, err := resolveTarget(c, nconnectionForTest(t))
	if err != nil {
		t.Fatalf("resolveTarget(nconnection, --ia) error = %v", err)
	}
	if rt.Resolved == "" {
		t.Fatal("resolveTarget(nconnection, --ia) Resolved is empty (test fixture assumption)")
	}
	if !shouldPrintResolvedTarget(rt) {
		t.Error("shouldPrintResolvedTarget(nconnection resolution) = false, want true — this is a real identity worth reviewing before confirming")
	}
}

func TestShouldPrintResolvedTarget_EmptyResolvedIsFalse(t *testing.T) {
	rt := credential.ResolvedTarget{Target: nipcash.Pubkey(strings.Repeat("a1", 32))}
	if shouldPrintResolvedTarget(rt) {
		t.Error("shouldPrintResolvedTarget(empty Resolved) = true, want false — nothing to show")
	}
}

// TestTransferWithAutoConsolidate_Declined confirms declining the
// combined consolidate+transfer confirmation returns cleanly (nil error)
// — and, critically, never reaches either wire call: group's fake
// entries have no dialable token, so a real attempt to build sources
// from them would fail loudly (or hang), not return cleanly.
// localPubKeyHex is resolved before the confirm prompt (see
// cash_transfer.go's own comment on why), so a local identity must exist
// even for this decline path to reach the prompt at all.
func TestTransferWithAutoConsolidate_Declined(t *testing.T) {
	tmp := t.TempDir()
	appdir.SetOverride(tmp)
	t.Cleanup(func() { appdir.SetOverride("") })
	if _, err := identity.GenerateAndSaveLocal(); err != nil {
		t.Fatalf("GenerateAndSaveLocal() error = %v", err)
	}

	c := newTestTransferCmd()
	withStdin(t, "n\n")

	amount := uint64(1000)
	group := []ledger.Entry{
		{ID: "tok-a", Token: "not-a-real-token", AmountMillis: &amount},
		{ID: "tok-b", Token: "not-a-real-token-either", AmountMillis: &amount},
	}
	l := &ledger.Ledger{}
	target := credential.ResolvedTarget{Target: nipcash.Pubkey("a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1")}

	if err := transferWithAutoConsolidate(c, l, group, 1500, "alice@example.com", target); err != nil {
		t.Fatalf("transferWithAutoConsolidate() error = %v, want nil (a decline is not an error)", err)
	}
}

// TestTransferWithAutoConsolidate_EmptyGroupErrors pins the defensive guard
// added alongside doCashConsolidate's own — see transferWithAutoConsolidate's
// doc comment on why an empty group isn't reachable via runCashTransfer
// today, but must not silently "succeed" (a zero-source
// TransferFromSourcesParams call) if some future caller ever passes one.
// Never reaches localPubKeyHex (checked before it), so no local identity
// needs to exist for this test.
func TestTransferWithAutoConsolidate_EmptyGroupErrors(t *testing.T) {
	c := newTestTransferCmd()
	l := &ledger.Ledger{}
	target := credential.ResolvedTarget{Target: nipcash.Pubkey(strings.Repeat("a1", 32))}

	if err := transferWithAutoConsolidate(c, l, nil, 1000, "alice@example.com", target); err == nil {
		t.Fatal("transferWithAutoConsolidate(empty group) = nil error, want an error")
	}
}

// --- expiryWarningSuffix / earliestExpiry / isExpiredWalletErr / fetchExpiresAt ---
// Round 2 (docs/private/audit-round2-expiration-matrix.md) added these and
// verified them only live, against a real Hub. These pin the same
// contracts at the unit level, no network required.

func TestExpiryWarningSuffix_NilExpiryIsEmpty(t *testing.T) {
	if got := expiryWarningSuffix(nil, "transfer"); got != "" {
		t.Errorf("expiryWarningSuffix(nil) = %q, want empty", got)
	}
}

func TestExpiryWarningSuffix_AlreadyPassed(t *testing.T) {
	past := time.Now().Add(-time.Hour).Unix()
	got := expiryWarningSuffix(&past, "transfer")
	if !strings.Contains(got, "passed") {
		t.Errorf("expiryWarningSuffix(past) = %q, want it to say the deadline already passed", got)
	}
	// The already-passed case is deliberately verb-agnostic ("Deadline
	// passed — may fail.") — unlike the "expires soon" case below, naming
	// the specific action doesn't add anything once it's already too late.
}

func TestExpiryWarningSuffix_SoonWarns(t *testing.T) {
	soon := time.Now().Add(2 * time.Hour).Unix()
	got := expiryWarningSuffix(&soon, "consolidate")
	if !strings.Contains(got, "Expires in") {
		t.Errorf("expiryWarningSuffix(2h out) = %q, want an 'Expires in ...' warning", got)
	}
	if !strings.Contains(got, "consolidate") {
		t.Errorf("expiryWarningSuffix(2h out, %q) = %q, want the verb named in the warning", "consolidate", got)
	}
}

func TestExpiryWarningSuffix_FarAwayIsEmpty(t *testing.T) {
	farAway := time.Now().Add(48 * time.Hour).Unix()
	if got := expiryWarningSuffix(&farAway, "transfer"); got != "" {
		t.Errorf("expiryWarningSuffix(48h out) = %q, want empty (only warns inside the 24h threshold)", got)
	}
}

func TestEarliestExpiry_BothNilIsNil(t *testing.T) {
	if got := earliestExpiry(nil, nil); got != nil {
		t.Errorf("earliestExpiry(nil, nil) = %v, want nil", *got)
	}
}

func TestEarliestExpiry_OneNilReturnsTheOther(t *testing.T) {
	v := int64(500)
	if got := earliestExpiry(nil, &v); got == nil || *got != v {
		t.Errorf("earliestExpiry(nil, %d) = %v, want %d", v, got, v)
	}
	if got := earliestExpiry(&v, nil); got == nil || *got != v {
		t.Errorf("earliestExpiry(%d, nil) = %v, want %d", v, got, v)
	}
}

func TestEarliestExpiry_PicksSmaller(t *testing.T) {
	earlier := int64(100)
	later := int64(200)
	if got := earliestExpiry(&earlier, &later); got == nil || *got != earlier {
		t.Errorf("earliestExpiry(100, 200) = %v, want 100", got)
	}
	if got := earliestExpiry(&later, &earlier); got == nil || *got != earlier {
		t.Errorf("earliestExpiry(200, 100) = %v, want 100 (order shouldn't matter)", got)
	}
}

func TestIsExpiredWalletErr(t *testing.T) {
	if !isExpiredWalletErr(&relayclient.WalletError{Code: "EXPIRED"}) {
		t.Error("isExpiredWalletErr(EXPIRED) = false, want true")
	}
	if isExpiredWalletErr(&relayclient.WalletError{Code: "RESTRICTED"}) {
		t.Error("isExpiredWalletErr(RESTRICTED) = true, want false — only EXPIRED is about which connection placed the call")
	}
	if isExpiredWalletErr(errors.New("plain error, not a wallet decline at all")) {
		t.Error("isExpiredWalletErr(plain error) = true, want false")
	}
	if isExpiredWalletErr(nil) {
		t.Error("isExpiredWalletErr(nil) = true, want false")
	}
}

// TestFetchExpiresAt_MalformedTokenReturnsNil confirms the "nil on any
// failure" contract for the cheapest failure: nipcash.Decode itself fails,
// before any dial is even attempted.
func TestFetchExpiresAt_MalformedTokenReturnsNil(t *testing.T) {
	c := newTestTransferCmd()
	if got := fetchExpiresAt(c, "not-a-real-token"); got != nil {
		t.Errorf("fetchExpiresAt(malformed) = %v, want nil", *got)
	}
}

// TestFetchExpiresAt_NoRelayURLsReturnsNil confirms the same contract one
// layer deeper: a structurally valid, decodable token that just has no
// RelayURLs at all — nipcashclient.Connect's own NewNWCClient rejects that
// immediately ("pairing info has no relay urls") before opening any real
// network connection, so this exercises fetchExpiresAt's Connect-failure
// branch without a live Hub or a timeout.
func TestFetchExpiresAt_NoRelayURLsReturnsNil(t *testing.T) {
	walletPubkey := strings.Repeat("aa", 32)
	secret := strings.Repeat("bb", 32)
	encoded, err := nipcash.Encode(nipcash.Token{
		HRP:              "lokicash",
		WalletPubkey:     walletPubkey,
		Secret:           secret,
		IdentityRequired: ptrTo(false),
	})
	if err != nil {
		t.Fatalf("nipcash.Encode: %v", err)
	}
	c := newTestTransferCmd()
	if got := fetchExpiresAt(c, encoded); got != nil {
		t.Errorf("fetchExpiresAt(no relay urls) = %v, want nil", *got)
	}
}

// --- attemptTransferFromSourcesWithRetry: the dial-candidate retry policy,
// exercised with no network via attemptTransferFromSourcesFn (a
// package-var seam, the same pattern prompt.go's own stdin uses) instead
// of a real dial. Mirrors cash_consolidate_test.go's doCashConsolidate
// coverage — see doCashConsolidate's own doc comment for the shared
// reasoning both retry loops implement.

func withFakeAttemptTransferFromSources(t *testing.T, fn func(dialToken string, params nipcashclient.TransferFromSourcesParams) (*nipcashclient.TransferFromSourcesResult, error)) {
	t.Helper()
	orig := attemptTransferFromSourcesFn
	attemptTransferFromSourcesFn = fn
	t.Cleanup(func() { attemptTransferFromSourcesFn = orig })
}

func TestAttemptTransferFromSourcesWithRetry_SucceedsOnFirstCandidate(t *testing.T) {
	calls := 0
	withFakeAttemptTransferFromSources(t, func(dialToken string, params nipcashclient.TransferFromSourcesParams) (*nipcashclient.TransferFromSourcesResult, error) {
		calls++
		return &nipcashclient.TransferFromSourcesResult{
			ConsolidatedFirst: &nipcash.CashConsolidateResult{AmountMillis: 1000},
			Transfer:          &nipcash.CashTransferResult{AmountMillis: 1000},
		}, nil
	})

	result, err := attemptTransferFromSourcesWithRetry([]string{"tok-a", "tok-b"}, nipcashclient.TransferFromSourcesParams{})
	if err != nil {
		t.Fatalf("error = %v, want nil", err)
	}
	if result == nil || result.Transfer == nil || result.Transfer.AmountMillis != 1000 {
		t.Fatalf("result = %+v, want the fake success result", result)
	}
	if calls != 1 {
		t.Errorf("attemptTransferFromSourcesFn called %d times, want 1 (first candidate already succeeded)", calls)
	}
}

func TestAttemptTransferFromSourcesWithRetry_RetriesOnExpiredThenSucceeds(t *testing.T) {
	var dialed []string
	withFakeAttemptTransferFromSources(t, func(dialToken string, params nipcashclient.TransferFromSourcesParams) (*nipcashclient.TransferFromSourcesResult, error) {
		dialed = append(dialed, dialToken)
		if dialToken == "expired-a" {
			return nil, &relayclient.WalletError{Method: "cash_consolidate", Code: "EXPIRED", Message: "expired"}
		}
		return &nipcashclient.TransferFromSourcesResult{Transfer: &nipcash.CashTransferResult{AmountMillis: 500}}, nil
	})

	result, err := attemptTransferFromSourcesWithRetry([]string{"expired-a", "healthy-b"}, nipcashclient.TransferFromSourcesParams{})
	if err != nil {
		t.Fatalf("error = %v, want nil (should succeed via the healthy sibling)", err)
	}
	if result == nil || result.Transfer == nil || result.Transfer.AmountMillis != 500 {
		t.Fatalf("result = %+v, want the healthy sibling's result", result)
	}
	if want := []string{"expired-a", "healthy-b"}; !reflect.DeepEqual(dialed, want) {
		t.Errorf("dialed %v, want %v (retry only after an EXPIRED decline, in order)", dialed, want)
	}
}

// TestAttemptTransferFromSourcesWithRetry_AllExpiredFails mirrors
// TestCashConsolidate_AllSourcesExpired_ClassifiedAsAuth's live coverage
// (docs/private/audit-round2-expiration-matrix.md) for this composite's
// own interim-consolidate leg: every candidate exhausted must still fail
// cleanly as the real classified EXPIRED/auth error, not hang, loop, or
// misreport as something else.
func TestAttemptTransferFromSourcesWithRetry_AllExpiredFails(t *testing.T) {
	calls := 0
	withFakeAttemptTransferFromSources(t, func(dialToken string, params nipcashclient.TransferFromSourcesParams) (*nipcashclient.TransferFromSourcesResult, error) {
		calls++
		return nil, &relayclient.WalletError{Method: "cash_consolidate", Code: "EXPIRED", Message: "expired"}
	})

	_, err := attemptTransferFromSourcesWithRetry([]string{"a", "b", "c"}, nipcashclient.TransferFromSourcesParams{})
	if err == nil {
		t.Fatal("error = nil, want the real EXPIRED error once every candidate is exhausted")
	}
	if calls != 3 {
		t.Errorf("attemptTransferFromSourcesFn called %d times, want 3 (every candidate tried once)", calls)
	}
	ce := output.AsCLIError(classifyNWCErr(&cobra.Command{}, err))
	if ce.Code != output.CodeAuth || ce.NWCCode != "EXPIRED" {
		t.Errorf("classified = {code: %s, nwc_code: %s}, want {auth, EXPIRED} — AGENTS.md's error table for an expired-wallet decline", ce.Code, ce.NWCCode)
	}
}

// TestAttemptTransferFromSourcesWithRetry_PartialProgressStopsImmediately
// confirms a *nipcashclient.PartialProgressError (the interim consolidate
// landed for real; the sources are already consumed) is never retried
// through a further candidate, even with siblings left and even though its
// own Cause here is EXPIRED-coded — the sources being spent already is
// what matters, not the failure's own code (see
// attemptTransferFromSourcesWithRetry's own doc comment).
func TestAttemptTransferFromSourcesWithRetry_PartialProgressStopsImmediately(t *testing.T) {
	calls := 0
	partialErr := &nipcashclient.PartialProgressError{
		Consolidated: &nipcash.CashConsolidateResult{AmountMillis: 700, NewWalletToken: "interim-token", NewWalletPubkey: "interim-pub"},
		Cause:        &relayclient.WalletError{Code: "EXPIRED"},
	}
	withFakeAttemptTransferFromSources(t, func(dialToken string, params nipcashclient.TransferFromSourcesParams) (*nipcashclient.TransferFromSourcesResult, error) {
		calls++
		return nil, partialErr
	})

	_, err := attemptTransferFromSourcesWithRetry([]string{"a", "b", "c"}, nipcashclient.TransferFromSourcesParams{})
	var partial *nipcashclient.PartialProgressError
	if !errors.As(err, &partial) || partial.Consolidated == nil || partial.Consolidated.NewWalletToken != "interim-token" {
		t.Fatalf("error = %v, want the PartialProgressError surfaced unchanged", err)
	}
	if calls != 1 {
		t.Errorf("attemptTransferFromSourcesFn called %d times, want 1 (never retry once the interim consolidate has actually landed)", calls)
	}
}

// TestAttemptTransferFromSourcesWithRetry_NonExpiredFailureStopsImmediately
// confirms a decline unrelated to which connection placed the call (e.g.
// insufficient balance) is never retried through a sibling, even with
// candidates left — only an EXPIRED decline is about the dial choice
// itself.
func TestAttemptTransferFromSourcesWithRetry_NonExpiredFailureStopsImmediately(t *testing.T) {
	calls := 0
	withFakeAttemptTransferFromSources(t, func(dialToken string, params nipcashclient.TransferFromSourcesParams) (*nipcashclient.TransferFromSourcesResult, error) {
		calls++
		return nil, &relayclient.WalletError{Code: "INSUFFICIENT_BALANCE"}
	})

	_, err := attemptTransferFromSourcesWithRetry([]string{"a", "b"}, nipcashclient.TransferFromSourcesParams{})
	if err == nil {
		t.Fatal("error = nil, want the INSUFFICIENT_BALANCE failure")
	}
	if calls != 1 {
		t.Errorf("attemptTransferFromSourcesFn called %d times, want 1 (a non-EXPIRED decline is about the request itself, never worth retrying via a sibling connection)", calls)
	}
}

// TestAttemptTransferFromSourcesWithRetry_ZeroCandidates guards the
// defensive check added alongside doCashConsolidate's own equivalent: an
// empty dial-candidate list must return a real error, not a silent
// (nil, nil) "success".
func TestAttemptTransferFromSourcesWithRetry_ZeroCandidates(t *testing.T) {
	calls := 0
	withFakeAttemptTransferFromSources(t, func(dialToken string, params nipcashclient.TransferFromSourcesParams) (*nipcashclient.TransferFromSourcesResult, error) {
		calls++
		return nil, nil
	})

	result, err := attemptTransferFromSourcesWithRetry(nil, nipcashclient.TransferFromSourcesParams{})
	if err == nil {
		t.Fatal("error = nil, want an error instead of a silent (nil, nil) 'success'")
	}
	if result != nil {
		t.Errorf("result = %+v, want nil", result)
	}
	if calls != 0 {
		t.Errorf("attemptTransferFromSourcesFn called %d times, want 0", calls)
	}
}

// TestResolveLiveEntry_CashSelectedEntryWritesThroughOnMutation is the
// regression test for resolveLiveEntry's own reason to exist — see its
// doc comment. ledger.SelectForAmount's SelectionPlan.Entry points into
// l.Held()'s copy slice, not l.Entries; this proves resolveLiveEntry's
// re-resolved pointer is the live one instead, the same way
// TestResolveHeldToken_SingleHeldAutoPickWritesThroughOnMutation
// (cmd/cash_redeem_test.go) proves it for the identical shape there.
func TestResolveLiveEntry_CashSelectedEntryWritesThroughOnMutation(t *testing.T) {
	amount := uint64(5_000)
	l := &ledger.Ledger{Entries: []ledger.Entry{{ID: "tok-selected", Status: ledger.StatusHeld, AmountMillis: &amount}}}

	plan, err := ledger.SelectForAmount(l.Held(), amount)
	if err != nil {
		t.Fatalf("SelectForAmount() error = %v", err)
	}
	if plan.Entry == nil {
		t.Fatalf("plan.Entry = nil, want the exact-match entry (test setup)")
	}

	entry, err := resolveLiveEntry(l, plan.Entry.ID)
	if err != nil {
		t.Fatalf("resolveLiveEntry() error = %v", err)
	}

	// A direct field mutation on entry (what resolveAmount does when it
	// discovers an uncached amount live) must reach l.Entries — not
	// plan.Entry's own copy, which this doesn't even touch.
	discovered := uint64(9_999)
	entry.AmountMillis = &discovered

	got, ok := l.Find("tok-selected")
	if !ok {
		t.Fatalf("l.Find(%q) not found after mutation", "tok-selected")
	}
	if got.AmountMillis == nil || *got.AmountMillis != discovered {
		t.Errorf("l.Entries' own copy has AmountMillis = %v, want %d — resolveLiveEntry must return a pointer into l.Entries, not plan.Entry's copy", got.AmountMillis, discovered)
	}
	if plan.Entry.AmountMillis == nil || *plan.Entry.AmountMillis != amount {
		t.Errorf("plan.Entry.AmountMillis = %v, want unchanged at %d — confirms it really is a separate copy, not aliased with l.Entries", plan.Entry.AmountMillis, amount)
	}
}

// --- disambiguateTransferArgs: the positional-args logic that lets
// `cashctl transfer 100` (bare amount, no target) work — this is the
// regression test for that exact bearer-transfer UX request: a purely
// numeric single argument must be read as an amount, never as a target,
// so it doesn't get rejected as an invalid target string, and a real
// target string must never be misread as an amount. With two args, this
// also pins that both the documented `<amount> <target>` order and the
// old `<target> <amount>` order resolve the same way.

func TestDisambiguateTransferArgs_NoArgs(t *testing.T) {
	to, amount := disambiguateTransferArgs(nil)
	if to != "" || amount != "" {
		t.Errorf("disambiguateTransferArgs(nil) = (%q, %q), want (\"\", \"\")", to, amount)
	}
}

func TestDisambiguateTransferArgs_SingleNumericArgIsAmount(t *testing.T) {
	to, amount := disambiguateTransferArgs([]string{"100"})
	if to != "" {
		t.Errorf("disambiguateTransferArgs([\"100\"]) to = %q, want empty (no target given)", to)
	}
	if amount != "100" {
		t.Errorf("disambiguateTransferArgs([\"100\"]) amount = %q, want \"100\"", amount)
	}
}

func TestDisambiguateTransferArgs_SingleFractionalArgIsAmount(t *testing.T) {
	// A fractional loki amount ("0.5") must still be recognized as an
	// amount, not a target — disambiguateTransferArgs now sniffs shape via
	// output.ParseAmount (which accepts loki's up-to-3-decimal precision),
	// not a plain-integer check.
	to, amount := disambiguateTransferArgs([]string{"0.5"})
	if to != "" {
		t.Errorf("disambiguateTransferArgs([\"0.5\"]) to = %q, want empty (no target given)", to)
	}
	if amount != "0.5" {
		t.Errorf("disambiguateTransferArgs([\"0.5\"]) amount = %q, want \"0.5\"", amount)
	}
}

func TestDisambiguateTransferArgs_SingleNonNumericArgIsTarget(t *testing.T) {
	for _, arg := range []string{
		"alice@example.com",
		"a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1",
		"bearer-target",
		"nconnection1qqs2u2jj",
	} {
		to, amount := disambiguateTransferArgs([]string{arg})
		if to != arg {
			t.Errorf("disambiguateTransferArgs([%q]) to = %q, want %q", arg, to, arg)
		}
		if amount != "" {
			t.Errorf("disambiguateTransferArgs([%q]) amount = %q, want empty", arg, amount)
		}
	}
}

func TestDisambiguateTransferArgs_TwoArgsAreTargetThenAmount(t *testing.T) {
	to, amount := disambiguateTransferArgs([]string{"alice@example.com", "500"})
	if to != "alice@example.com" || amount != "500" {
		t.Errorf("disambiguateTransferArgs(2 args) = (%q, %q), want (\"alice@example.com\", \"500\")", to, amount)
	}
}

func TestDisambiguateTransferArgs_TwoArgsAmountThenTargetAlsoWorks(t *testing.T) {
	to, amount := disambiguateTransferArgs([]string{"500", "alice@example.com"})
	if to != "alice@example.com" || amount != "500" {
		t.Errorf("disambiguateTransferArgs(2 args, amount-first) = (%q, %q), want (\"alice@example.com\", \"500\")", to, amount)
	}
}

// TestDisambiguateTransferArgs_MalformedAmountBlamesTheAmountNotTheTarget
// is the regression guard for the bug found live: `transfer 1.5x <npub>`
// used to report `"<npub>" is not a valid amount in loki` — the malformed
// amount landed in the target slot (it isn't a target either, but nothing
// checked that), and the perfectly valid npub got blamed for not parsing
// as a number. Neither "1.5x" nor a target string parses as an amount, so
// the old position-only fallback (args[0] fails ParseAmount -> assume
// args[0] is the target) always picked wrong here regardless of which
// side the malformed amount was on.
func TestDisambiguateTransferArgs_MalformedAmountBlamesTheAmountNotTheTarget(t *testing.T) {
	validTarget := "alice@example.com"
	for _, badAmount := range []string{"1.5x", "5 loki", "abc123"} {
		t.Run("amount_first="+badAmount, func(t *testing.T) {
			to, amount := disambiguateTransferArgs([]string{badAmount, validTarget})
			if to != validTarget {
				t.Errorf("to = %q, want %q (the valid target, not the malformed amount)", to, validTarget)
			}
			if amount != badAmount {
				t.Errorf("amount = %q, want %q (the actually-malformed one)", amount, badAmount)
			}
		})
		t.Run("target_first="+badAmount, func(t *testing.T) {
			to, amount := disambiguateTransferArgs([]string{validTarget, badAmount})
			if to != validTarget {
				t.Errorf("to = %q, want %q (the valid target, not the malformed amount)", to, validTarget)
			}
			if amount != badAmount {
				t.Errorf("amount = %q, want %q (the actually-malformed one)", amount, badAmount)
			}
		})
	}
}

// TestDisambiguateTransferArgs_BothInvalidFallsBackToAmountFirstOrder
// covers the genuinely ambiguous case (neither side parses as an amount,
// neither looks like a real target shape either) — there's no way to know
// which was meant, so this pins the fallback to the documented
// amount-first order rather than asserting a "correct" answer that
// doesn't exist.
func TestDisambiguateTransferArgs_BothInvalidFallsBackToAmountFirstOrder(t *testing.T) {
	to, amount := disambiguateTransferArgs([]string{"garbage1", "garbage2"})
	if amount != "garbage1" || to != "garbage2" {
		t.Errorf("disambiguateTransferArgs(both invalid) = (to=%q, amount=%q), want (to=\"garbage2\", amount=\"garbage1\") — documented amount-first fallback", to, amount)
	}
}

// --- printAndSaveTransferResult: the cash_to_send assembly for a bearer
// transfer — the regression test for "reveal only the sent piece, never
// require an unknown flag to signal a bearer send." A bearer target's
// combined <token>#<secret> string must appear ONLY when this transfer
// actually is a bearer transfer, and only the newly-minted sent token,
// never a remainder.

func withTempConfigDirForTransferResult(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	appdir.SetOverride(tmp)
	t.Cleanup(func() { appdir.SetOverride("") })
}

func TestPrintAndSaveTransferResult_BearerTargetAssemblesCashToSend(t *testing.T) {
	withTempConfigDirForTransferResult(t)
	c := testCmdWithFlags(true, false)
	l := &ledger.Ledger{}
	bt := nipcash.NewBearerTarget()
	target := credential.ResolvedTarget{Target: bt}
	result := &nipcash.CashTransferResult{AmountMillis: 500, NewWalletToken: "new-sent-token"}

	captured := withCapturedStdout(func() {
		if err := printAndSaveTransferResult(c, l, result, 500, "bearer-target", target, nil, ledger.Entry{}, ""); err != nil {
			t.Fatalf("printAndSaveTransferResult() error = %v", err)
		}
	})

	var out map[string]any
	if err := json.Unmarshal([]byte(captured), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", captured, err)
	}
	want := "new-sent-token#" + bt.Secret()
	got, _ := out["cash_to_send"].(string)
	if got != want {
		t.Errorf("cash_to_send = %q, want %q", got, want)
	}
}

func TestPrintAndSaveTransferResult_BearerTargetHumanModeShowsCashString(t *testing.T) {
	withTempConfigDirForTransferResult(t)
	c := testCmdWithFlags(false, false)
	l := &ledger.Ledger{}
	bt := nipcash.NewBearerTarget()
	target := credential.ResolvedTarget{Target: bt}
	result := &nipcash.CashTransferResult{AmountMillis: 500, NewWalletToken: "new-sent-token"}

	captured := withCapturedStdout(func() {
		if err := printAndSaveTransferResult(c, l, result, 500, "bearer-target", target, nil, ledger.Entry{}, ""); err != nil {
			t.Fatalf("printAndSaveTransferResult() error = %v", err)
		}
	})

	want := "new-sent-token#" + bt.Secret()
	if !strings.Contains(captured, want) {
		t.Errorf("printed output = %q, want it to contain the combined cash string %q", captured, want)
	}
	if strings.Contains(captured, "bearer-target") {
		t.Errorf("printed output = %q, want the literal \"bearer-target\" placeholder never shown to the human", captured)
	}
}

func TestPrintAndSaveTransferResult_NonBearerTargetOmitsCashToSend(t *testing.T) {
	withTempConfigDirForTransferResult(t)
	c := testCmdWithFlags(true, false)
	l := &ledger.Ledger{}
	target := credential.ResolvedTarget{Target: nipcash.Pubkey(strings.Repeat("a1", 32))}
	result := &nipcash.CashTransferResult{AmountMillis: 500, NewWalletToken: "new-sent-token"}

	captured := withCapturedStdout(func() {
		if err := printAndSaveTransferResult(c, l, result, 500, "alice@example.com", target, nil, ledger.Entry{}, ""); err != nil {
			t.Fatalf("printAndSaveTransferResult() error = %v", err)
		}
	})

	var out map[string]any
	if err := json.Unmarshal([]byte(captured), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", captured, err)
	}
	if _, present := out["cash_to_send"]; present {
		t.Errorf("cash_to_send present = %v, want the key entirely absent for a non-bearer target", out["cash_to_send"])
	}
	// The fix for a related bug: a pubkey/npub/connection recipient still
	// needs an exact token string to receive with, split or not.
	if got, _ := out["recipient_token"].(string); got != "new-sent-token" {
		t.Errorf("recipient_token = %q, want %q", got, "new-sent-token")
	}
}

// TestPrintAndSaveTransferResult_BearerTargetNoNewTokenFallsBackToOriginal is
// the regression test for the real bug (not a defensive corner case): an
// EXACT full-amount transfer — SplitAmount == CurrentAmount, or amount
// omitted entirely — reassigns the wallet IN PLACE, so NewWalletToken comes
// back "" even on a genuinely completed, money-moving transfer. That used to
// mean cash_to_send was never assembled at all: the whole gift was lost.
// originalToken (the entry actually transferred) must be used as the
// fallback half of the combined <token>#<secret> string.
func TestPrintAndSaveTransferResult_BearerTargetNoNewTokenFallsBackToOriginal(t *testing.T) {
	withTempConfigDirForTransferResult(t)
	c := testCmdWithFlags(true, false)
	l := &ledger.Ledger{}
	bt := nipcash.NewBearerTarget()
	target := credential.ResolvedTarget{Target: bt}
	result := &nipcash.CashTransferResult{AmountMillis: 500}

	captured := withCapturedStdout(func() {
		if err := printAndSaveTransferResult(c, l, result, 500, "bearer-target", target, nil, ledger.Entry{}, "original-token"); err != nil {
			t.Fatalf("printAndSaveTransferResult() error = %v", err)
		}
	})

	var out map[string]any
	if err := json.Unmarshal([]byte(captured), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", captured, err)
	}
	want := "original-token#" + bt.Secret()
	if got, _ := out["cash_to_send"].(string); got != want {
		t.Errorf("cash_to_send = %q, want %q (the fund-loss bug: an in-place full transfer must still hand back a spendable gift)", got, want)
	}
}

// TestPrintAndSaveTransferResult_BearerTargetNothingToFallBackTo is the true
// defensive case: neither a new wallet token nor an original token exists
// (should be unreachable from any real call site) — must not assemble a
// bogus "#secret" string with no token half.
func TestPrintAndSaveTransferResult_BearerTargetNothingToFallBackTo(t *testing.T) {
	withTempConfigDirForTransferResult(t)
	c := testCmdWithFlags(true, false)
	l := &ledger.Ledger{}
	bt := nipcash.NewBearerTarget()
	target := credential.ResolvedTarget{Target: bt}
	result := &nipcash.CashTransferResult{AmountMillis: 500}

	captured := withCapturedStdout(func() {
		if err := printAndSaveTransferResult(c, l, result, 500, "bearer-target", target, nil, ledger.Entry{}, ""); err != nil {
			t.Fatalf("printAndSaveTransferResult() error = %v", err)
		}
	})

	var out map[string]any
	if err := json.Unmarshal([]byte(captured), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", captured, err)
	}
	if _, present := out["cash_to_send"]; present {
		t.Errorf("cash_to_send present = %v, want absent when there's truly nothing to combine with the secret", out["cash_to_send"])
	}
}

// --- split remainders keep the source's credential mode: the regression
// test for "a bearer wallet's second transfer asked for `cashctl init`."
// lokihub leaves a split's remainder under the source's SAME identity, so
// a remainder saved with no credential info fell through resolveCredential
// to the (never-configured) local identity.

func TestPrintAndSaveTransferResult_BearerRemainderStaysSpendableWithoutIdentity(t *testing.T) {
	withTempConfigDirForTransferResult(t) // fresh config dir: no identity configured
	c := testCmdWithFlags(true, false)
	l := &ledger.Ledger{}
	target := credential.ResolvedTarget{Target: nipcash.NewBearerTarget()}
	remaining := uint64(9999)
	result := &nipcash.CashTransferResult{
		AmountMillis:          1,
		NewWalletToken:        "new-sent-token",
		RemainingAmountMillis: &remaining,
		RemainderWalletToken:  "remainder-token",
		RemainderWalletPubkey: strings.Repeat("b2", 32),
	}
	source := ledger.Entry{IdentityRequired: ptrTo(false), BearerSecret: strings.Repeat("c3", 32)}

	withCapturedStdout(func() {
		if err := printAndSaveTransferResult(c, l, result, 1, "bearer-target", target, nil, credentialModeOf(source), ""); err != nil {
			t.Fatalf("printAndSaveTransferResult() error = %v", err)
		}
	})

	rem, ok := l.FindByToken("remainder-token")
	if !ok {
		t.Fatal("remainder entry not saved")
	}
	if rem.IdentityRequired == nil || *rem.IdentityRequired {
		t.Errorf("remainder IdentityRequired = %v, want false (bearer)", rem.IdentityRequired)
	}
	if rem.BearerSecret != source.BearerSecret {
		t.Errorf("remainder BearerSecret = %q, want the source's own secret", rem.BearerSecret)
	}
	if rem.AmountMillis == nil || *rem.AmountMillis != remaining {
		t.Errorf("remainder AmountMillis = %v, want %d", rem.AmountMillis, remaining)
	}
	// The user-visible symptom: spending the remainder must not need a
	// local identity when the source never did.
	if _, err := resolveCredential(testCmdWithFlags(true, false), rem); err != nil {
		t.Errorf("resolveCredential(remainder) error = %v, want nil — a bearer remainder must not need `cashctl init`", err)
	}
}

func TestPrintAndSaveTransferResult_PubkeyRemainderStaysPubkeyMode(t *testing.T) {
	withTempConfigDirForTransferResult(t)
	c := testCmdWithFlags(true, false)
	l := &ledger.Ledger{}
	target := credential.ResolvedTarget{Target: nipcash.Pubkey(strings.Repeat("a1", 32))}
	result := &nipcash.CashTransferResult{
		AmountMillis:          1,
		NewWalletToken:        "new-sent-token",
		RemainderWalletToken:  "remainder-token",
		RemainderWalletPubkey: strings.Repeat("b2", 32),
	}

	withCapturedStdout(func() {
		// The auto-consolidate path's mode: the interim wallet is the
		// caller's own pubkey identity.
		if err := printAndSaveTransferResult(c, l, result, 1, "alice@example.com", target, nil, ledger.Entry{IdentityRequired: ptrTo(true)}, ""); err != nil {
			t.Fatalf("printAndSaveTransferResult() error = %v", err)
		}
	})

	rem, ok := l.FindByToken("remainder-token")
	if !ok {
		t.Fatal("remainder entry not saved")
	}
	if rem.IdentityRequired == nil || !*rem.IdentityRequired {
		t.Errorf("remainder IdentityRequired = %v, want true (pubkey)", rem.IdentityRequired)
	}
	if rem.BearerSecret != "" {
		t.Errorf("remainder BearerSecret = %q, want empty for a pubkey-mode remainder", rem.BearerSecret)
	}
}

// TestPrintAndSaveTransferResult_RecipientTokenNeverSavedToLedger locks in
// the split half of the invariant cash_consolidate.go's own doCashConsolidate
// enforces explicitly for a gift target ("must not be saved as one of the
// caller's own held tokens") — printAndSaveTransferResult enforces the same
// thing structurally instead, by simply never passing NewWalletToken to
// l.Add anywhere in this file (only RemainderWalletToken, which NIP-CASH
// guarantees server-side always stays bound to the caller's OWN unchanged
// identity — see cash_transfer_controller.go's RemainderIdentityType/Value).
// This test exists so a future change that starts also persisting
// NewWalletToken (e.g. "for convenience", mirroring consolidate's own
// newLedgerEntry shape) fails loudly instead of silently starting to save
// the recipient's own split-off cash as if it were the caller's.
func TestPrintAndSaveTransferResult_RecipientTokenNeverSavedToLedger(t *testing.T) {
	withTempConfigDirForTransferResult(t)
	c := testCmdWithFlags(true, false)
	l := &ledger.Ledger{}
	target := credential.ResolvedTarget{Target: nipcash.Pubkey(strings.Repeat("a1", 32))}
	result := &nipcash.CashTransferResult{
		AmountMillis:          1,
		NewWalletToken:        "recipients-own-token",
		NewWalletPubkey:       strings.Repeat("c3", 32),
		RemainingAmountMillis: ptrTo(uint64(9999)),
		RemainderWalletToken:  "callers-own-remainder",
		RemainderWalletPubkey: strings.Repeat("b2", 32),
	}

	withCapturedStdout(func() {
		if err := printAndSaveTransferResult(c, l, result, 1, "alice@example.com", target, nil, ledger.Entry{IdentityRequired: ptrTo(true)}, ""); err != nil {
			t.Fatalf("printAndSaveTransferResult() error = %v", err)
		}
	})

	if _, ok := l.FindByToken("recipients-own-token"); ok {
		t.Error("the recipient's own split-off token was found in the caller's ledger — it belongs to the recipient, not the caller")
	}
	if len(l.Entries) != 1 {
		t.Fatalf("l.Entries = %+v, want exactly 1 (only the caller's own remainder)", l.Entries)
	}
	if _, ok := l.FindByToken("callers-own-remainder"); !ok {
		t.Error("the caller's own remainder was not saved")
	}
}

func TestCredentialModeOf_CopiesOnlyCredentialFields(t *testing.T) {
	amt := uint64(5000)
	minter := "minter"
	src := ledger.Entry{
		ID: "tok-1", Token: "src-token", WalletPubkey: "src-pub", AmountMillis: &amt, MinterPubkey: &minter,
		IdentityRequired: ptrTo(false), BearerSecret: "sec",
		ConnectionKeyPlatform: "github", ConnectionKeyExternalID: "42", AttestationEventID: "ev", IAPubkey: "ia",
	}
	want := ledger.Entry{
		IdentityRequired: ptrTo(false), BearerSecret: "sec",
		ConnectionKeyPlatform: "github", ConnectionKeyExternalID: "42", AttestationEventID: "ev", IAPubkey: "ia",
	}
	if got := credentialModeOf(src); !reflect.DeepEqual(got, want) {
		t.Errorf("credentialModeOf() = %+v, want %+v (identity/amount/token fields of the source must not carry over)", got, want)
	}
}

// --- expiresAtFromCheck: an already-expired wallet rejects CheckClaim's own
// list_recipients call with EXPIRED, so that rejection IS the expiry signal.
// It used to collapse to nil, and the "already expired" warnings never fired.

func TestExpiresAtFromCheck_ExpiredDeclineReportsAPassedDeadline(t *testing.T) {
	err := &relayclient.WalletError{Method: "list_recipients", Code: "EXPIRED", Message: "app expired"}
	got := expiresAtFromCheck(nil, err)
	if got == nil {
		t.Fatal("expiresAtFromCheck(EXPIRED) = nil, want a deadline that has already passed")
	}
	if time.Until(time.Unix(*got, 0)) > 0 {
		t.Errorf("deadline %v is still in the future, want it already passed", time.Unix(*got, 0))
	}
	// It must be recognised as expired by the callers' own test.
	if w := expiryWarningSuffix(got, "transfer"); w != "Deadline passed — may fail." {
		t.Errorf("expiryWarningSuffix() = %q, want the deadline-passed warning", w)
	}
}

func TestExpiresAtFromCheck_OtherFailuresStillMeanUnknown(t *testing.T) {
	// Only EXPIRED is an answer: an unrelated decline or a network failure
	// says nothing about the deadline, and must not manufacture a warning.
	for _, err := range []error{
		&relayclient.WalletError{Method: "list_recipients", Code: "RESTRICTED", Message: "no"},
		errors.New("dial tcp: connection refused"),
	} {
		if got := expiresAtFromCheck(nil, err); got != nil {
			t.Errorf("expiresAtFromCheck(%v) = %v, want nil", err, time.Unix(*got, 0))
		}
	}
}

func TestExpiresAtFromCheck_SuccessPassesTheRealDeadlineThrough(t *testing.T) {
	when := time.Now().Add(3 * time.Hour).Unix()
	if got := expiresAtFromCheck(&nipcash.CheckClaimResult{ExpiresAt: &when}, nil); got == nil || *got != when {
		t.Errorf("expiresAtFromCheck() = %v, want %d", got, when)
	}
	if got := expiresAtFromCheck(&nipcash.CheckClaimResult{}, nil); got != nil {
		t.Errorf("no deadline on the result: got %v, want nil", *got)
	}
}
