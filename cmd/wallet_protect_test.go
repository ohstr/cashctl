package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

func newTestProtectCmd(jsonMode, yes bool) *cobra.Command {
	cmd := testCmdWithFlags(jsonMode, yes)
	cmd.Flags().String("token", "", "")
	return cmd
}

// TestResolveUnprotectedCashHolding_NoneEligible guards the "nothing to
// protect" case — a fully-protected or pubkey-only wallet must get a
// specific, actionable answer, not a bare "not found."
func TestResolveUnprotectedCashHolding_NoneEligible(t *testing.T) {
	amt := uint64(1000)
	l := &ledger.Ledger{Entries: []ledger.Entry{
		{ID: "already-protected", Status: ledger.StatusHeld, AmountMillis: &amt, CashProtection: ledger.CashProtected},
		{ID: "pubkey-mode", Status: ledger.StatusHeld, AmountMillis: &amt},
	}}
	cmd := newTestProtectCmd(false, false)
	_, err := resolveUnprotectedCashHolding(cmd, l, nil)
	if err == nil {
		t.Fatal("resolveUnprotectedCashHolding() = nil error, want rejected (nothing eligible)")
	}
	if output.ExitCode(err) != 4 {
		t.Errorf("ExitCode = %d, want 4 (not_found)", output.ExitCode(err))
	}
}

// TestResolveUnprotectedCashHolding_AutoPicksTheOnlyOne mirrors
// resolveHeldToken's own single-match auto-pick, scoped to eligible
// (CashShared) entries only — a protected sibling must never be offered.
func TestResolveUnprotectedCashHolding_AutoPicksTheOnlyOne(t *testing.T) {
	amt := uint64(1000)
	l := &ledger.Ledger{Entries: []ledger.Entry{
		{ID: "shared-one", Status: ledger.StatusHeld, AmountMillis: &amt, CashProtection: ledger.CashShared},
		{ID: "already-protected", Status: ledger.StatusHeld, AmountMillis: &amt, CashProtection: ledger.CashProtected},
	}}
	cmd := newTestProtectCmd(false, false)
	e, err := resolveUnprotectedCashHolding(cmd, l, nil)
	if err != nil {
		t.Fatalf("resolveUnprotectedCashHolding() error = %v", err)
	}
	if e.ID != "shared-one" {
		t.Errorf("resolved ID = %q, want %q", e.ID, "shared-one")
	}
}

// TestResolveUnprotectedCashHolding_ExplicitTokenAlreadyProtected
// confirms naming an already-protected token by --token gets a specific
// conflict, not a silent re-protect attempt.
func TestResolveUnprotectedCashHolding_ExplicitTokenAlreadyProtected(t *testing.T) {
	amt := uint64(1000)
	l := &ledger.Ledger{Entries: []ledger.Entry{
		{ID: "already-protected", Status: ledger.StatusHeld, AmountMillis: &amt, CashProtection: ledger.CashProtected},
	}}
	cmd := newTestProtectCmd(false, false)
	if err := cmd.Flags().Set("token", "already-protected"); err != nil {
		t.Fatal(err)
	}
	_, err := resolveUnprotectedCashHolding(cmd, l, nil)
	if err == nil {
		t.Fatal("resolveUnprotectedCashHolding(already protected) = nil error, want rejected")
	}
	if output.ExitCode(err) != 5 {
		t.Errorf("ExitCode = %d, want 5 (conflict)", output.ExitCode(err))
	}
	if !strings.Contains(err.Error(), "already protected") {
		t.Errorf("error = %q, want it to say already protected", err)
	}
}

// TestResolveUnprotectedCashHolding_ExplicitTokenPubkeyMode confirms
// naming a pubkey-mode entry (CashProtection == "", n/a) is rejected
// with a reason, not silently accepted.
func TestResolveUnprotectedCashHolding_ExplicitTokenPubkeyMode(t *testing.T) {
	amt := uint64(1000)
	l := &ledger.Ledger{Entries: []ledger.Entry{
		{ID: "pubkey-mode", Status: ledger.StatusHeld, AmountMillis: &amt},
	}}
	cmd := newTestProtectCmd(false, false)
	if err := cmd.Flags().Set("token", "pubkey-mode"); err != nil {
		t.Fatal(err)
	}
	_, err := resolveUnprotectedCashHolding(cmd, l, nil)
	if err == nil {
		t.Fatal("resolveUnprotectedCashHolding(pubkey-mode) = nil error, want rejected")
	}
	if !strings.Contains(err.Error(), "cash-mode") {
		t.Errorf("error = %q, want it to say this isn't a cash-mode holding", err)
	}
}

// TestResolveUnprotectedCashHolding_ExplicitTokenNotFound confirms a
// nonexistent id is a plain not_found, same as every other --token lookup
// in this codebase.
func TestResolveUnprotectedCashHolding_ExplicitTokenNotFound(t *testing.T) {
	l := &ledger.Ledger{}
	cmd := newTestProtectCmd(false, false)
	if err := cmd.Flags().Set("token", "nope"); err != nil {
		t.Fatal(err)
	}
	_, err := resolveUnprotectedCashHolding(cmd, l, nil)
	if err == nil {
		t.Fatal("resolveUnprotectedCashHolding(nonexistent) = nil error, want rejected")
	}
	if output.ExitCode(err) != 4 {
		t.Errorf("ExitCode = %d, want 4 (not_found)", output.ExitCode(err))
	}
}

// TestResolveUnprotectedCashHolding_MultipleEligibleJSONModeErrors
// confirms the multi-match case defers to pickHeldToken (already
// extensively tested in cash_redeem_test.go) rather than guessing —
// --json has no terminal to prompt from.
func TestResolveUnprotectedCashHolding_MultipleEligibleJSONModeErrors(t *testing.T) {
	amt := uint64(1000)
	l := &ledger.Ledger{Entries: []ledger.Entry{
		{ID: "shared-a", Status: ledger.StatusHeld, AmountMillis: &amt, CashProtection: ledger.CashShared},
		{ID: "shared-b", Status: ledger.StatusHeld, AmountMillis: &amt, CashProtection: ledger.CashShared},
	}}
	cmd := newTestProtectCmd(true, false)
	_, err := resolveUnprotectedCashHolding(cmd, l, nil)
	if err == nil {
		t.Fatal("resolveUnprotectedCashHolding(2 eligible, --json) = nil error, want rejected (ambiguous)")
	}
	if output.ExitCode(err) != 2 {
		t.Errorf("ExitCode = %d, want 2 (usage — ambiguous choice)", output.ExitCode(err))
	}
}

// TestResolveUnprotectedCashHolding_PositionalArg confirms the id can be
// given positionally, not just via --token.
func TestResolveUnprotectedCashHolding_PositionalArg(t *testing.T) {
	amt := uint64(1000)
	l := &ledger.Ledger{Entries: []ledger.Entry{
		{ID: "shared-one", Status: ledger.StatusHeld, AmountMillis: &amt, CashProtection: ledger.CashShared},
	}}
	cmd := newTestProtectCmd(false, false)
	e, err := resolveUnprotectedCashHolding(cmd, l, []string{"shared-one"})
	if err != nil {
		t.Fatalf("resolveUnprotectedCashHolding() error = %v", err)
	}
	if e.ID != "shared-one" {
		t.Errorf("resolved ID = %q, want %q", e.ID, "shared-one")
	}
}
