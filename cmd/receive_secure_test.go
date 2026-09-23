package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
)

// newTestReceiveCmd mirrors cash_transfer_test.go's own newTestTransferCmd
// — protectCashReceipt/checkClaimWithCashHub read these flags directly
// off cmd.
func newTestReceiveCmd() *cobra.Command {
	c := &cobra.Command{}
	c.Flags().Bool("json", false, "")
	c.Flags().Bool("yes", false, "")
	return c
}

// TestProtectCashReceipt_NonCash_NotApplicable confirms the pure,
// no-network guard: a identity-bound entry never reaches the confirm prompt,
// the ledger, or a dial — it's the one branch of protectCashReceipt this
// package can unit-test directly (the rest are network-touching, covered
// by the integration suite instead, matching this codebase's existing
// split between unit and integration test responsibilities).
func TestProtectCashReceipt_NonCash_NotApplicable(t *testing.T) {
	c := newTestReceiveCmd()
	l := &ledger.Ledger{}
	entry := &ledger.Entry{ID: "tok-abcd"}

	status, finalEntry := protectCashReceipt(c, l, entry, false)

	if status.Status != "not_applicable" {
		t.Fatalf("Status = %q, want \"not_applicable\"", status.Status)
	}
	if finalEntry != entry {
		t.Fatal("finalEntry must be the same entry unchanged for a identity-bound receive")
	}
}

// TestProtectCashReceipt_Declined confirms declining the confirm prompt
// returns cleanly without ever reaching the ledger or a dial — entry has
// no real token, so a real attempt to build a RekeyCashSlice call from
// it would fail loudly (or hang), not return cleanly.
func TestProtectCashReceipt_Declined(t *testing.T) {
	c := newTestReceiveCmd()
	withStdin(t, "n\n")
	l := &ledger.Ledger{}
	entry := &ledger.Entry{ID: "tok-abcd", Token: "not-a-real-token"}

	status, finalEntry := protectCashReceipt(c, l, entry, true)

	if status.Status != "declined" {
		t.Fatalf("Status = %q, want \"declined\"", status.Status)
	}
	if finalEntry != entry {
		t.Fatal("finalEntry must be the same entry unchanged when declined")
	}
}

// TestPrintProtectFailure_PointsAtWalletProtect pins the remediation these
// two text-mode hints name. Both used to say `consolidate --to cash`, which
// cannot reach the holding either way: with no sources it auto-groups, and
// GroupableForConsolidation skips cash-mode entries entirely; with one it
// refuses ("needs at least 2 sources, got 1"). `wallet protect` exists for
// exactly this state. The string was last changed by a mechanical
// bearer->cash rename sweep, so a plain regression test is what keeps the
// next sweep from reintroducing advice that can't work.
func TestPrintProtectFailure_PointsAtWalletProtect(t *testing.T) {
	out := withCapturedStdout(func() { printProtectFailure(false, errors.New("hub declined")) })

	if !strings.Contains(out, "cashctl wallet protect") {
		t.Errorf("printProtectFailure must name `cashctl wallet protect`, got: %q", out)
	}
	if strings.Contains(out, "--to cash") {
		t.Errorf("printProtectFailure must not send the user to consolidate --to cash, got: %q", out)
	}
}

// TestPrintProtectPartialFailure_DoesNotSendToCashConsolidate covers the
// partial case, whose state is different: the interim step already
// reassigned the holding to the local pubkey (CashSecret cleared,
// IdentityRequired set), so nothing is still shared and only the merge
// failed. Naming a cash-mode target here was doubly wrong.
func TestPrintProtectPartialFailure_DoesNotSendToCashConsolidate(t *testing.T) {
	out := withCapturedStdout(func() { printProtectPartialFailure(false, errors.New("merge failed")) })

	if strings.Contains(out, "--to cash") {
		t.Errorf("printProtectPartialFailure must not name a cash-mode consolidate target, got: %q", out)
	}
}

// TestPrintProtectHints_SilentInJSONMode keeps these on the right side of
// AGENTS.md's output contract: they are text-mode narration, so --json
// callers must see nothing on stdout from them.
func TestPrintProtectHints_SilentInJSONMode(t *testing.T) {
	for name, fn := range map[string]func(){
		"printProtectFailure":        func() { printProtectFailure(true, errors.New("hub declined")) },
		"printProtectPartialFailure": func() { printProtectPartialFailure(true, errors.New("merge failed")) },
	} {
		if out := withCapturedStdout(fn); out != "" {
			t.Errorf("%s in jsonMode printed %q, want nothing", name, out)
		}
	}
}
