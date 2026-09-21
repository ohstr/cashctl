package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// TestSummarizeHeldTokens_ExpiredEntryExcludedFromTotal guards against the
// bug found auditing `balance`: a held cash token past its own Hub-side
// redemption deadline used to keep counting toward the total forever (the
// ledger had nowhere to cache expires_at, so balance had no local signal
// to exclude it, and re-checking every held token live on every `balance`
// call was never acceptable). ptrTo(int64(...)) mirrors this package's
// existing helper for building *int64/*uint64 test fixtures.
func TestSummarizeHeldTokens_ExpiredEntryExcludedFromTotal(t *testing.T) {
	now := int64(1_700_000_000)
	held := []ledger.Entry{
		{ID: "fresh", AmountMillis: ptrTo(uint64(1000)), ExpiresAt: ptrTo(now + 3600)},
		{ID: "expired", AmountMillis: ptrTo(uint64(2000)), ExpiresAt: ptrTo(now - 1)},
		{ID: "expires-this-second", AmountMillis: ptrTo(uint64(500)), ExpiresAt: ptrTo(now)},
		{ID: "no-known-expiry", AmountMillis: ptrTo(uint64(300)), ExpiresAt: nil},
	}

	lines, total, expiredHeld := summarizeHeldTokens(held, now)

	if total != 1300 {
		t.Errorf("total = %d, want 1300 (fresh 1000 + no-known-expiry 300)", total)
	}
	if expiredHeld != 2500 {
		t.Errorf("expiredHeld = %d, want 2500 (expired 2000 + expires-this-second 500)", expiredHeld)
	}
	if len(lines) != 4 {
		t.Fatalf("len(lines) = %d, want 4 — an expired entry must still be listed, not silently dropped", len(lines))
	}
	for _, l := range lines {
		wantExpired := l.Name == "expired" || l.Name == "expires-this-second"
		if l.Expired != wantExpired {
			t.Errorf("line %q: Expired = %v, want %v", l.Name, l.Expired, wantExpired)
		}
	}
}

// TestSummarizeHeldTokens_NilAmountSkipped matches the pre-existing
// behavior for an entry whose amount was never learned (nothing to sum or
// display yet) — unrelated to expiry, just guarding the refactor didn't
// change it.
func TestSummarizeHeldTokens_NilAmountSkipped(t *testing.T) {
	held := []ledger.Entry{{ID: "unknown-amount", AmountMillis: nil}}
	lines, total, expiredHeld := summarizeHeldTokens(held, 0)
	if len(lines) != 0 || total != 0 || expiredHeld != 0 {
		t.Errorf("summarizeHeldTokens(nil-amount) = %v, %d, %d, want empty/zero", lines, total, expiredHeld)
	}
}

// TestRunWalletBalanceFrom_SpentTokenIsNotFound guards against the bug
// found auditing `balance --from <id>`: l.Find matches an entry by ID
// regardless of status, so a token already redeemed/transferred/
// consolidated away used to have its old cached amount reported as if it
// were still a real, spendable balance. Status must gate it, the same way
// `receive` already gates a re-received token on Status == held (not mere
// existence).
func TestRunWalletBalanceFrom_SpentTokenIsNotFound(t *testing.T) {
	tmp := t.TempDir()
	appdir.SetOverride(tmp)
	t.Cleanup(func() { appdir.SetOverride("") })

	l, err := ledger.Load()
	if err != nil {
		t.Fatalf("ledger.Load() error = %v", err)
	}
	amount := uint64(5000)
	entry, err := l.Add(ledger.Entry{Token: "lokicash1spentbalance", AmountMillis: &amount})
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := l.SetStatus(entry.ID, ledger.StatusRedeemed); err != nil {
		t.Fatalf("SetStatus() error = %v", err)
	}
	if err := l.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	cmd := &cobra.Command{}
	cmd.Flags().Bool("json", false, "")

	err = runWalletBalanceFrom(cmd, entry.ID, false)
	if err == nil {
		t.Fatal("runWalletBalanceFrom(spent token id) = nil error, want rejected")
	}
	ce := output.AsCLIError(err)
	if ce.Code != output.CodeNotFound {
		t.Errorf("Code = %q, want %q", ce.Code, output.CodeNotFound)
	}
	if output.ExitCode(err) != 4 {
		t.Errorf("ExitCode = %d, want 4", output.ExitCode(err))
	}
}

// TestRunWalletBalanceFrom_HeldTokenStillReportsAmount is the held-status
// counterpart, guarding that the new status check didn't just reject
// everything.
func TestRunWalletBalanceFrom_HeldTokenStillReportsAmount(t *testing.T) {
	tmp := t.TempDir()
	appdir.SetOverride(tmp)
	t.Cleanup(func() { appdir.SetOverride("") })

	l, err := ledger.Load()
	if err != nil {
		t.Fatalf("ledger.Load() error = %v", err)
	}
	amount := uint64(7000)
	entry, err := l.Add(ledger.Entry{Token: "lokicash1heldbalance", AmountMillis: &amount})
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := l.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	cmd := &cobra.Command{}
	cmd.Flags().Bool("json", false, "")

	printed := withCapturedStdout(func() {
		if err := runWalletBalanceFrom(cmd, entry.ID, false); err != nil {
			t.Fatalf("runWalletBalanceFrom(held token id) error = %v", err)
		}
	})
	if !strings.Contains(printed, "7 loki") {
		t.Errorf("output = %q, want the held token's own amount (7000 mloki = 7 loki)", printed)
	}
}
