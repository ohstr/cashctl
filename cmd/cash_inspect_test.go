package cmd

// cash status had zero unit tests before this change — its only prior
// coverage was live, via integration/. This file covers the new multi-entry
// resolver and outcome printing added for #19's ambiguity-policy switch:
// unlike redeem, status is pure read access, so an ambiguous selection with
// none named processes every eligible candidate instead of refusing.

import (
	"errors"
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nipcash"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

func newTestStatusCmd(jsonMode, yes bool) *cobra.Command {
	cmd := testCmdWithFlags(jsonMode, yes)
	cmd.Flags().String("token", "", "")
	return cmd
}

func TestResolveHeldTokensOrAll_NoneHeld(t *testing.T) {
	l := &ledger.Ledger{}
	cmd := newTestStatusCmd(false, false)
	_, err := resolveHeldTokensOrAll(cmd, l)
	if err == nil {
		t.Fatal("resolveHeldTokensOrAll() = nil error, want rejected (nothing held)")
	}
	if output.ExitCode(err) != 4 {
		t.Errorf("ExitCode = %d, want 4 (not_found)", output.ExitCode(err))
	}
}

func TestResolveHeldTokensOrAll_AutoPicksTheOnlyOne(t *testing.T) {
	amt := uint64(1000)
	l := &ledger.Ledger{Entries: []ledger.Entry{
		{ID: "tok-a", Status: ledger.StatusHeld, AmountMillis: &amt},
	}}
	cmd := newTestStatusCmd(false, false)
	es, err := resolveHeldTokensOrAll(cmd, l)
	if err != nil {
		t.Fatalf("resolveHeldTokensOrAll() error = %v", err)
	}
	if len(es) != 1 || es[0].ID != "tok-a" {
		t.Errorf("resolved = %v, want exactly [tok-a]", idsOf(es))
	}
}

// TestResolveHeldTokensOrAll_MultipleJSONModeReturnsAll is the behavior this
// file exists for: cash status is read-only, so --json with several held
// and none named reports every one instead of refusing as ambiguous — the
// same switch wallet protect got, for the same reason.
func TestResolveHeldTokensOrAll_MultipleJSONModeReturnsAll(t *testing.T) {
	amt := uint64(1000)
	l := &ledger.Ledger{Entries: []ledger.Entry{
		{ID: "tok-a", Status: ledger.StatusHeld, AmountMillis: &amt},
		{ID: "tok-b", Status: ledger.StatusHeld, AmountMillis: &amt},
	}}
	cmd := newTestStatusCmd(true, false)
	es, err := resolveHeldTokensOrAll(cmd, l)
	if err != nil {
		t.Fatalf("resolveHeldTokensOrAll(2 held, --json) error = %v, want both resolved", err)
	}
	got := idsOf(es)
	if len(got) != 2 || !contains(got, "tok-a") || !contains(got, "tok-b") {
		t.Errorf("resolved = %v, want both tok-a and tok-b", got)
	}
}

func TestResolveHeldTokensOrAll_ExplicitTokenSpentRejected(t *testing.T) {
	amt := uint64(1000)
	l := &ledger.Ledger{Entries: []ledger.Entry{
		{ID: "tok-a", Status: ledger.StatusRedeemed, AmountMillis: &amt},
	}}
	cmd := newTestStatusCmd(false, false)
	if err := cmd.Flags().Set("token", "tok-a"); err != nil {
		t.Fatal(err)
	}
	_, err := resolveHeldTokensOrAll(cmd, l)
	if err == nil {
		t.Fatal("resolveHeldTokensOrAll(spent token) = nil error, want rejected")
	}
	if !strings.Contains(err.Error(), "redeemed") {
		t.Errorf("error = %q, want it to name how the token was spent", err)
	}
}

func TestResolveHeldTokensOrAll_ExplicitTokenNotFound(t *testing.T) {
	l := &ledger.Ledger{}
	cmd := newTestStatusCmd(false, false)
	if err := cmd.Flags().Set("token", "nope"); err != nil {
		t.Fatal(err)
	}
	_, err := resolveHeldTokensOrAll(cmd, l)
	if err == nil {
		t.Fatal("resolveHeldTokensOrAll(nonexistent) = nil error, want rejected")
	}
	if output.ExitCode(err) != 4 {
		t.Errorf("ExitCode = %d, want 4 (not_found)", output.ExitCode(err))
	}
}

func TestFirstCashStatusError_AllSucceededIsNil(t *testing.T) {
	outcomes := []cashStatusOutcome{
		{ID: "a", Result: &nipcash.CashStatusResult{}},
		{ID: "b", Result: &nipcash.CashStatusResult{}},
	}
	if err := firstCashStatusError(outcomes); err != nil {
		t.Errorf("firstCashStatusError(all ok) = %v, want nil", err)
	}
}

// TestFirstCashStatusError_FirstFailureWins mirrors firstOutcomeError /
// firstProtectOutcomeError's own contract: the FIRST failure drives the
// exit code, not any later one.
func TestFirstCashStatusError_FirstFailureWins(t *testing.T) {
	cmd := newTestStatusCmd(false, false)
	first := output.InvalidInputError(cmd, "a", errors.New("first"))
	second := output.NetworkError(cmd, errors.New("second"))
	outcomes := []cashStatusOutcome{
		{ID: "a", Err: first},
		{ID: "b", Err: second},
	}
	err := firstCashStatusError(outcomes)
	if err == nil {
		t.Fatal("firstCashStatusError() = nil, want the first failure")
	}
	if got := output.ExitCode(err); got != 3 { // invalid_input, from "a", not network's 6
		t.Errorf("ExitCode = %d, want 3 (the FIRST outcome's own code)", got)
	}
}

// TestPrintCashStatusOutcomes_SingleSuccessKeepsTheBareShape is the
// backward-compatibility guard: this command's --json shape has never had a
// wrapper key at all (output.PrintJSON(result) directly) — an explicit
// --token, or a single held token auto-picked, must keep printing the bare
// result object, not an array, not a "statuses" key.
func TestPrintCashStatusOutcomes_SingleSuccessKeepsTheBareShape(t *testing.T) {
	s, _ := captureStreams(t, func() {
		printCashStatusOutcomes(true, []cashStatusOutcome{
			{ID: "tok-a", Result: &nipcash.CashStatusResult{}},
		})
	})
	if strings.Contains(s, `"statuses"`) || strings.Contains(s, `"id"`) {
		t.Errorf("single-success output carries a wrapper/id, want the old bare shape: %s", s)
	}
}

// TestPrintCashStatusOutcomes_SingleFailurePrintsNothing matches
// printProtectOutcomes'/consolidate's own rule: a lone failure's classified
// error on stderr already says everything, so stdout must stay empty under
// --json — never both a success shape and a failure shape on the same
// stream (AGENTS.md).
func TestPrintCashStatusOutcomes_SingleFailurePrintsNothing(t *testing.T) {
	cmd := newTestStatusCmd(true, false)
	s, _ := captureStreams(t, func() {
		printCashStatusOutcomes(true, []cashStatusOutcome{
			{ID: "tok-a", Err: output.NetworkError(cmd, errors.New("boom"))},
		})
	})
	if strings.TrimSpace(s) != "" {
		t.Errorf("single-failure stdout = %q, want empty", s)
	}
}

// TestPrintCashStatusOutcomes_MultipleCarriesID is the new shape's own
// requirement, mirroring protect's: with several outcomes, each row MUST
// say which entry it is.
func TestPrintCashStatusOutcomes_MultipleCarriesID(t *testing.T) {
	cmd := newTestStatusCmd(true, false)
	s, _ := captureStreams(t, func() {
		printCashStatusOutcomes(true, []cashStatusOutcome{
			{ID: "tok-a", Result: &nipcash.CashStatusResult{}},
			{ID: "tok-b", Err: output.NetworkError(cmd, errors.New("boom"))},
		})
	})
	for _, want := range []string{`"tok-a"`, `"tok-b"`, `"statuses"`} {
		if !strings.Contains(s, want) {
			t.Errorf("multi-outcome shape is missing %s: %s", want, s)
		}
	}
}

func TestCashStatusHeader(t *testing.T) {
	if got := cashStatusHeader(1, 2); got != "Token 1 of 2:" {
		t.Errorf("cashStatusHeader(1, 2) = %q, want %q", got, "Token 1 of 2:")
	}
}
