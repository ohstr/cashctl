package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

func newTestProtectCmd(jsonMode, yes bool) *cobra.Command {
	cmd := testCmdWithFlags(jsonMode, yes)
	cmd.Flags().String("token", "", "")
	return cmd
}

func TestResolveUnprotectedCashHoldings_NoneEligible(t *testing.T) {
	amt := uint64(1000)
	l := &ledger.Ledger{Entries: []ledger.Entry{
		{ID: "already-protected", Status: ledger.StatusHeld, AmountMillis: &amt, CashProtection: ledger.CashProtected},
		{ID: "pubkey-mode", Status: ledger.StatusHeld, AmountMillis: &amt},
	}}
	cmd := newTestProtectCmd(false, false)
	_, err := resolveUnprotectedCashHoldings(cmd, l, nil)
	if err == nil {
		t.Fatal("resolveUnprotectedCashHoldings() = nil error, want rejected (nothing eligible)")
	}
	if output.ExitCode(err) != 4 {
		t.Errorf("ExitCode = %d, want 4 (not_found)", output.ExitCode(err))
	}
}

func TestResolveUnprotectedCashHoldings_AutoPicksTheOnlyOne(t *testing.T) {
	amt := uint64(1000)
	l := &ledger.Ledger{Entries: []ledger.Entry{
		{ID: "shared-one", Status: ledger.StatusHeld, AmountMillis: &amt, CashProtection: ledger.CashShared},
		{ID: "already-protected", Status: ledger.StatusHeld, AmountMillis: &amt, CashProtection: ledger.CashProtected},
	}}
	cmd := newTestProtectCmd(false, false)
	es, err := resolveUnprotectedCashHoldings(cmd, l, nil)
	if err != nil {
		t.Fatalf("resolveUnprotectedCashHoldings() error = %v", err)
	}
	if len(es) != 1 || es[0].ID != "shared-one" {
		t.Errorf("resolved = %v, want exactly [shared-one]", idsOf(es))
	}
}

func TestResolveUnprotectedCashHoldings_ExplicitTokenAlreadyProtected(t *testing.T) {
	amt := uint64(1000)
	l := &ledger.Ledger{Entries: []ledger.Entry{
		{ID: "already-protected", Status: ledger.StatusHeld, AmountMillis: &amt, CashProtection: ledger.CashProtected},
	}}
	cmd := newTestProtectCmd(false, false)
	if err := cmd.Flags().Set("token", "already-protected"); err != nil {
		t.Fatal(err)
	}
	_, err := resolveUnprotectedCashHoldings(cmd, l, nil)
	if err == nil {
		t.Fatal("resolveUnprotectedCashHoldings(already protected) = nil error, want rejected")
	}
	if output.ExitCode(err) != 5 {
		t.Errorf("ExitCode = %d, want 5 (conflict)", output.ExitCode(err))
	}
	if !strings.Contains(err.Error(), "already protected") {
		t.Errorf("error = %q, want it to say already protected", err)
	}
}

func TestResolveUnprotectedCashHoldings_ExplicitTokenPubkeyMode(t *testing.T) {
	amt := uint64(1000)
	l := &ledger.Ledger{Entries: []ledger.Entry{
		{ID: "pubkey-mode", Status: ledger.StatusHeld, AmountMillis: &amt},
	}}
	cmd := newTestProtectCmd(false, false)
	if err := cmd.Flags().Set("token", "pubkey-mode"); err != nil {
		t.Fatal(err)
	}
	_, err := resolveUnprotectedCashHoldings(cmd, l, nil)
	if err == nil {
		t.Fatal("resolveUnprotectedCashHoldings(pubkey-mode) = nil error, want rejected")
	}
	if !strings.Contains(err.Error(), "cash-mode") {
		t.Errorf("error = %q, want it to say this isn't a cash-mode holding", err)
	}
}

func TestResolveUnprotectedCashHoldings_ExplicitTokenNotFound(t *testing.T) {
	l := &ledger.Ledger{}
	cmd := newTestProtectCmd(false, false)
	if err := cmd.Flags().Set("token", "nope"); err != nil {
		t.Fatal(err)
	}
	_, err := resolveUnprotectedCashHoldings(cmd, l, nil)
	if err == nil {
		t.Fatal("resolveUnprotectedCashHoldings(nonexistent) = nil error, want rejected")
	}
	if output.ExitCode(err) != 4 {
		t.Errorf("ExitCode = %d, want 4 (not_found)", output.ExitCode(err))
	}
}

// TestResolveUnprotectedCashHoldings_MultipleEligibleJSONModeReturnsAll is
// the behavior this whole file changed for (D-CLI audit follow-up, #19 F4):
// protecting keeps the value fully yours either way, so — unlike redeem,
// which pays out irreversibly — an ambiguous selection here has nothing to
// lose. --json with several eligible and none named used to refuse
// (ExitCode 2, "ambiguous"); it now resolves every eligible holding, mirroring
// consolidate's own auto-group-processes-all pattern.
func TestResolveUnprotectedCashHoldings_MultipleEligibleJSONModeReturnsAll(t *testing.T) {
	amt := uint64(1000)
	l := &ledger.Ledger{Entries: []ledger.Entry{
		{ID: "shared-a", Status: ledger.StatusHeld, AmountMillis: &amt, CashProtection: ledger.CashShared},
		{ID: "shared-b", Status: ledger.StatusHeld, AmountMillis: &amt, CashProtection: ledger.CashShared},
	}}
	cmd := newTestProtectCmd(true, false)
	es, err := resolveUnprotectedCashHoldings(cmd, l, nil)
	if err != nil {
		t.Fatalf("resolveUnprotectedCashHoldings(2 eligible, --json) error = %v, want both resolved", err)
	}
	got := idsOf(es)
	if len(got) != 2 || !contains(got, "shared-a") || !contains(got, "shared-b") {
		t.Errorf("resolved = %v, want both shared-a and shared-b", got)
	}
}

// TestResolveUnprotectedCashHoldings_MultipleEligibleYesFlagReturnsAll is the
// same guarantee under --yes instead of --json, which pickHeldTokensOrAll
// treats identically (no terminal to meaningfully prompt from either way, by
// convention — see Confirm's own rule, cmd/prompt.go).
func TestResolveUnprotectedCashHoldings_MultipleEligibleYesFlagReturnsAll(t *testing.T) {
	amt := uint64(1000)
	l := &ledger.Ledger{Entries: []ledger.Entry{
		{ID: "shared-a", Status: ledger.StatusHeld, AmountMillis: &amt, CashProtection: ledger.CashShared},
		{ID: "shared-b", Status: ledger.StatusHeld, AmountMillis: &amt, CashProtection: ledger.CashShared},
	}}
	cmd := newTestProtectCmd(false, true)
	es, err := resolveUnprotectedCashHoldings(cmd, l, nil)
	if err != nil {
		t.Fatalf("resolveUnprotectedCashHoldings(2 eligible, --yes) error = %v, want both resolved", err)
	}
	if len(es) != 2 {
		t.Errorf("resolved %d entries, want 2", len(es))
	}
}

func TestResolveUnprotectedCashHoldings_PositionalArg(t *testing.T) {
	amt := uint64(1000)
	l := &ledger.Ledger{Entries: []ledger.Entry{
		{ID: "shared-one", Status: ledger.StatusHeld, AmountMillis: &amt, CashProtection: ledger.CashShared},
	}}
	cmd := newTestProtectCmd(false, false)
	es, err := resolveUnprotectedCashHoldings(cmd, l, []string{"shared-one"})
	if err != nil {
		t.Fatalf("resolveUnprotectedCashHoldings() error = %v", err)
	}
	if len(es) != 1 || es[0].ID != "shared-one" {
		t.Errorf("resolved = %v, want exactly [shared-one]", idsOf(es))
	}
}

// TestFirstProtectOutcomeError_Classification pins the three classified
// shapes runWalletProtect has always returned for a failure — unchanged by
// the move to multi-entry, just factored out so any outcome in a run of
// several can be classified, not only ever the sole one.
func TestFirstProtectOutcomeError_Classification(t *testing.T) {
	cmd := newTestProtectCmd(false, false)
	cases := []struct {
		name string
		o    protectOutcome
		want int
	}{
		{"generic failure", protectOutcome{ID: "a", Status: protectedStatus{Status: "failed", Error: "boom"}}, 1},
		{"likely wrong secret", protectOutcome{ID: "a", Status: protectedStatus{Status: "failed", Error: "boom", LikelyWrongSecret: true}}, 3},
		{"pending secret unresolved", protectOutcome{ID: "a", Status: protectedStatus{Status: "failed", Error: "boom", PendingSecretUnresolved: true}}, 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := firstProtectOutcomeError(cmd, []protectOutcome{tc.o})
			if err == nil {
				t.Fatal("firstProtectOutcomeError() = nil, want an error")
			}
			if got := output.ExitCode(err); got != tc.want {
				t.Errorf("ExitCode = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestFirstProtectOutcomeError_AllRekeyedIsNil confirms a fully successful
// run — of any size — returns no error at all, which is what drives exit 0
// whether one entry or several were protected.
func TestFirstProtectOutcomeError_AllRekeyedIsNil(t *testing.T) {
	cmd := newTestProtectCmd(false, false)
	outcomes := []protectOutcome{
		{ID: "a", Status: protectedStatus{Status: "rekeyed"}},
		{ID: "b", Status: protectedStatus{Status: "rekeyed"}},
	}
	if err := firstProtectOutcomeError(cmd, outcomes); err != nil {
		t.Errorf("firstProtectOutcomeError(all rekeyed) = %v, want nil", err)
	}
}

// TestFirstProtectOutcomeError_FirstFailureWins confirms the FIRST failure
// drives the exit code, not any later one — same contract
// firstOutcomeError (cash_consolidate.go) already has.
func TestFirstProtectOutcomeError_FirstFailureWins(t *testing.T) {
	cmd := newTestProtectCmd(false, false)
	outcomes := []protectOutcome{
		{ID: "a", Status: protectedStatus{Status: "failed", Error: "first", LikelyWrongSecret: true}},
		{ID: "b", Status: protectedStatus{Status: "failed", Error: "second", PendingSecretUnresolved: true}},
	}
	err := firstProtectOutcomeError(cmd, outcomes)
	if err == nil {
		t.Fatal("firstProtectOutcomeError() = nil, want the first failure")
	}
	if got := output.ExitCode(err); got != 3 { // invalid_input, from the first (LikelyWrongSecret), not 6
		t.Errorf("ExitCode = %d, want 3 (the FIRST outcome's classification)", got)
	}
}

// TestPrintProtectOutcomes_SingleSuccessKeepsTheOldShape is the backward-
// compatibility guard: an explicit --token (or an auto-picked single
// holding) that succeeds must still print exactly {"protected": {...}},
// with no "id" and no array — the shape every existing script already
// parses, unchanged by this feature.
func TestPrintProtectOutcomes_SingleSuccessKeepsTheOldShape(t *testing.T) {
	s, _ := captureStreams(t, func() {
		printProtectOutcomes(true, []protectOutcome{
			{ID: "shared-one", Status: protectedStatus{Status: "rekeyed"}},
		})
	})
	if strings.Contains(s, `"id"`) {
		t.Errorf("single-success shape carries an \"id\" field, want the old bare-object shape: %s", s)
	}
	if !strings.Contains(s, `"status": "rekeyed"`) {
		t.Errorf("missing status: %s", s)
	}
}

// TestPrintProtectOutcomes_MultipleCarriesID is the new shape's own
// requirement: with several outcomes, each row MUST say which entry it is,
// or the array is useless to a caller.
func TestPrintProtectOutcomes_MultipleCarriesID(t *testing.T) {
	s, _ := captureStreams(t, func() {
		printProtectOutcomes(true, []protectOutcome{
			{ID: "shared-a", Status: protectedStatus{Status: "rekeyed"}},
			{ID: "shared-b", Status: protectedStatus{Status: "rekeyed"}},
		})
	})
	for _, want := range []string{`"shared-a"`, `"shared-b"`} {
		if !strings.Contains(s, want) {
			t.Errorf("multi-outcome shape is missing %s: %s", want, s)
		}
	}
}

func TestRunWalletProtect_RejectsConnectionFlag(t *testing.T) {
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })

	cmd := newTestProtectCmd(false, false)
	cmd.Flags().String("connection", "savings", "")

	err := runWalletProtect(cmd, nil)
	if err == nil {
		t.Fatal("runWalletProtect(-c savings) = nil error, want rejected")
	}
	if got := output.ExitCode(err); got != 2 {
		t.Errorf("ExitCode = %d, want 2 (usage)", got)
	}
	if !strings.Contains(err.Error(), "-c/--connection doesn't apply") {
		t.Errorf("error = %q, want it to name the flag that doesn't apply", err)
	}
}

func idsOf(es []*ledger.Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.ID
	}
	return out
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
