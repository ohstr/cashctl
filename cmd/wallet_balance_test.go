package cmd

import (
	"errors"
	"strings"
	"testing"

	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// TestStrandedBalanceFallback_CodeAuthDeclinesUseCache guards against the
// bug found auditing an intermittent live-Hub flake: a get_balance decline
// used to fall back to the cached LastKnownBalanceMloki only on the exact
// literal code "EXPIRED", even though RESTRICTED/UNAUTHORIZED mean the same
// thing (the Hub has permanently cut the connection off — see
// internal/output's own nwcErrorCode table, which already groups all three
// under CodeAuth). A Hub free to decline an expired connection with any of
// the three used to have its two non-EXPIRED spellings silently drop the
// wallet's cached balance out of `wallet balance --breakdown` entirely.
func TestStrandedBalanceFallback_CodeAuthDeclinesUseCache(t *testing.T) {
	cached := int64(8_000)
	c := config.Connection{Name: "circle:test", LastKnownBalanceMloki: &cached}

	for _, code := range []string{"EXPIRED", "RESTRICTED", "UNAUTHORIZED"} {
		t.Run(code, func(t *testing.T) {
			err := &relayclient.WalletError{Method: "get_balance", Code: code, Message: "declined"}
			amount, ok := strandedBalanceFallback(err, c)
			if !ok {
				t.Fatalf("strandedBalanceFallback(%s) ok = false, want true — a CodeAuth decline must fall back to the cached balance", code)
			}
			if amount != cached {
				t.Errorf("strandedBalanceFallback(%s) amount = %d, want cached %d", code, amount, cached)
			}
		})
	}
}

// TestStrandedBalanceFallback_NonAuthFailuresStayUnreachable guards the
// other side of the same fix: a failure that isn't a permanent Hub decline
// must NOT use the cached figure, since the wallet might come back with a
// different live balance — the whole reason `unreachable` exists as a
// distinct, amount-less bucket (see runWalletBalance's own doc comment on
// it). Widening the CodeAuth match must not also start masking these.
func TestStrandedBalanceFallback_NonAuthFailuresStayUnreachable(t *testing.T) {
	cached := int64(8_000)
	c := config.Connection{Name: "circle:test", LastKnownBalanceMloki: &cached}

	cases := []struct {
		name string
		err  error
	}{
		{"non-auth wallet decline", &relayclient.WalletError{Method: "get_balance", Code: "RATE_LIMITED", Message: "slow down"}},
		{"plain network error", errors.New("dial tcp: connection refused")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if amount, ok := strandedBalanceFallback(tc.err, c); ok {
				t.Errorf("strandedBalanceFallback(%s) = (%d, true), want ok = false", tc.name, amount)
			}
		})
	}
}

// TestStrandedBalanceFallback_NoCachedBalance guards the pre-existing guard
// that a CodeAuth decline with nothing cached yet still can't produce a
// figure out of nowhere.
func TestStrandedBalanceFallback_NoCachedBalance(t *testing.T) {
	c := config.Connection{Name: "circle:test"}
	err := &relayclient.WalletError{Method: "get_balance", Code: "EXPIRED", Message: "expired"}
	if amount, ok := strandedBalanceFallback(err, c); ok {
		t.Errorf("strandedBalanceFallback(no cache) = (%d, true), want ok = false", amount)
	}
}

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
