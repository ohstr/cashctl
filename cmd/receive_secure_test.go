package cmd

import (
	"testing"

	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
)

// newTestReceiveCmd mirrors cash_transfer_test.go's own newTestTransferCmd
// — protectBearerReceipt/checkClaimWithCashHub read these flags directly
// off cmd.
func newTestReceiveCmd() *cobra.Command {
	c := &cobra.Command{}
	c.Flags().Bool("json", false, "")
	c.Flags().Bool("yes", false, "")
	return c
}

// TestProtectBearerReceipt_NonBearer_NotApplicable confirms the pure,
// no-network guard: a non-bearer entry never reaches the confirm prompt,
// the ledger, or a dial — it's the one branch of protectBearerReceipt this
// package can unit-test directly (the rest are network-touching, covered
// by the integration suite instead, matching this codebase's existing
// split between unit and integration test responsibilities).
func TestProtectBearerReceipt_NonBearer_NotApplicable(t *testing.T) {
	c := newTestReceiveCmd()
	l := &ledger.Ledger{}
	entry := &ledger.Entry{ID: "tok-abcd"}

	status, finalEntry := protectBearerReceipt(c, l, entry, false)

	if status.Status != "not_applicable" {
		t.Fatalf("Status = %q, want \"not_applicable\"", status.Status)
	}
	if finalEntry != entry {
		t.Fatal("finalEntry must be the same entry unchanged for a non-bearer receive")
	}
}

// TestProtectBearerReceipt_Declined confirms declining the confirm prompt
// returns cleanly without ever reaching the ledger or a dial — entry has
// no real token, so a real attempt to build a RekeyBearerSlice call from
// it would fail loudly (or hang), not return cleanly.
func TestProtectBearerReceipt_Declined(t *testing.T) {
	c := newTestReceiveCmd()
	withStdin(t, "n\n")
	l := &ledger.Ledger{}
	entry := &ledger.Entry{ID: "tok-abcd", Token: "not-a-real-token"}

	status, finalEntry := protectBearerReceipt(c, l, entry, true)

	if status.Status != "declined" {
		t.Fatalf("Status = %q, want \"declined\"", status.Status)
	}
	if finalEntry != entry {
		t.Fatal("finalEntry must be the same entry unchanged when declined")
	}
}
