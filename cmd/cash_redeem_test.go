package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
)

// withCapturedStdout redirects os.Stdout to a pipe for the duration of fn,
// returning whatever was written — same shape as prompt_test.go's own
// withStdoutSuppressed, but for tests that need to assert on the actual
// printed text rather than just silence it.
func withCapturedStdout(fn func()) string {
	real := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		fn()
		return ""
	}
	os.Stdout = w
	out := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		out <- string(b)
	}()
	fn()
	os.Stdout = real
	_ = w.Close()
	return <-out
}

func testCmdWithFlags(jsonMode, yes bool) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().Bool("json", jsonMode, "")
	cmd.Flags().Bool("yes", yes, "")
	return cmd
}

func heldEntries(n int) []ledger.Entry {
	entries := make([]ledger.Entry, n)
	for i := range entries {
		amt := uint64((i + 1) * 1000)
		entries[i] = ledger.Entry{ID: fmt.Sprintf("tok-%d", i), AmountMillis: &amt}
	}
	return entries
}

// stubStdin redirects prompt.go's package-level stdin reader to input for
// the duration of a test — os.Stdin itself can't be swapped after package
// init, since bufio.NewReader already captured the original file
// descriptor by then; reassigning the package var directly is the only
// way to feed PromptLine/Confirm canned input from a test in this package.
func stubStdin(t *testing.T, input string) {
	t.Helper()
	orig := stdin
	stdin = bufio.NewReader(strings.NewReader(input))
	t.Cleanup(func() { stdin = orig })
}

func TestPickHeldToken_JSONModeErrorsWithoutPrompting(t *testing.T) {
	cmd := testCmdWithFlags(true, false)
	if _, err := pickHeldToken(cmd, heldEntries(2)); err == nil {
		t.Fatal("expected an error under --json with multiple held tokens")
	}
}

func TestPickHeldToken_YesFlagErrorsWithoutPrompting(t *testing.T) {
	cmd := testCmdWithFlags(false, true)
	if _, err := pickHeldToken(cmd, heldEntries(2)); err == nil {
		t.Fatal("expected an error under --yes with multiple held tokens")
	}
}

func TestPickHeldToken_InteractivePicksByNumber(t *testing.T) {
	stubStdin(t, "2\n")
	cmd := testCmdWithFlags(false, false)
	entries := heldEntries(3)

	var picked *ledger.Entry
	var err error
	withStdoutSuppressed(func() {
		picked, err = pickHeldToken(cmd, entries)
	})
	if err != nil {
		t.Fatalf("pickHeldToken() error = %v", err)
	}
	if picked.ID != entries[1].ID {
		t.Fatalf("picked %s, want %s", picked.ID, entries[1].ID)
	}
}

// TestPickHeldToken_NeverPrintsRawID is a regression guard for
// docs/private/wallet-abstraction-plan.md: a human picking among held
// tokens sees index + amount + received-date, never the raw ledger.Entry.ID
// (ledger.go's own newID() is the actual "tok-" generator being guarded
// against here — not a placeholder prefix).
func TestPickHeldToken_NeverPrintsRawID(t *testing.T) {
	stubStdin(t, "1\n")
	cmd := testCmdWithFlags(false, false)
	entries := heldEntries(2)

	printed := withCapturedStdout(func() {
		_, _ = pickHeldToken(cmd, entries)
	})

	if strings.Contains(printed, "tok-") {
		t.Fatalf("pickHeldToken printed a raw ledger ID:\n%s", printed)
	}
	if !strings.Contains(printed, "1000 loki") {
		t.Fatalf("pickHeldToken didn't print the expected amount:\n%s", printed)
	}
}

func TestPickHeldToken_InvalidChoiceErrors(t *testing.T) {
	stubStdin(t, "9\n")
	cmd := testCmdWithFlags(false, false)

	var err error
	withStdoutSuppressed(func() {
		_, err = pickHeldToken(cmd, heldEntries(2))
	})
	if err == nil {
		t.Fatal("expected an error for an out-of-range choice")
	}
}

func heldLedger(n int) *ledger.Ledger {
	l := &ledger.Ledger{Entries: heldEntries(n)}
	for i := range l.Entries {
		l.Entries[i].Status = ledger.StatusHeld
	}
	return l
}

func TestResolveHeldToken_SingleHeldAutoPicksWithoutPrompting(t *testing.T) {
	l := heldLedger(1)
	cmd := testCmdWithFlags(false, false)
	cmd.Flags().String("token", "", "")

	entry, err := resolveHeldToken(cmd, l)
	if err != nil {
		t.Fatalf("resolveHeldToken() error = %v", err)
	}
	if entry.ID != l.Entries[0].ID {
		t.Fatalf("got %s, want %s", entry.ID, l.Entries[0].ID)
	}
}

func TestResolveHeldToken_ExplicitTokenFlag(t *testing.T) {
	l := heldLedger(3)
	cmd := testCmdWithFlags(false, false)
	cmd.Flags().String("token", l.Entries[1].ID, "")

	entry, err := resolveHeldToken(cmd, l)
	if err != nil {
		t.Fatalf("resolveHeldToken() error = %v", err)
	}
	if entry.ID != l.Entries[1].ID {
		t.Fatalf("got %s, want %s", entry.ID, l.Entries[1].ID)
	}
}

func TestResolveHeldToken_ExplicitTokenFlagNotFound(t *testing.T) {
	l := heldLedger(1)
	cmd := testCmdWithFlags(false, false)
	cmd.Flags().String("token", "tok-does-not-exist", "")

	if _, err := resolveHeldToken(cmd, l); err == nil {
		t.Fatal("expected an error for a --token ID that isn't held")
	}
}

func TestResolveHeldToken_NoneHeld(t *testing.T) {
	l := &ledger.Ledger{}
	cmd := testCmdWithFlags(false, false)
	cmd.Flags().String("token", "", "")

	if _, err := resolveHeldToken(cmd, l); err == nil {
		t.Fatal("expected an error when no tokens are held")
	}
}

// TestResolveHeldToken_SingleHeldAutoPickWritesThroughOnMutation is the
// regression test for the bug found auditing this file (documented as
// "found but out of scope" in docs/private/audit-round2-race-adversarial.md
// and fixed here, see docs/private/audit-round3-redeem-fee-boundaries.md):
// resolveHeldToken's single-held-token auto-pick used to hand back a
// pointer into l.Held()'s own copy slice (Ledger.Held's own doc comment:
// "returns copies") rather than the live entry in l.Entries. resolveAmount
// mutates entry.AmountMillis directly when it discovers an uncached amount
// live (a cash_transfer split remainder is the one real case this arises
// for — see resolveAmount's own doc comment) — before this fix, that write
// landed on the throwaway copy and never reached l.Entries at all, so the
// newly-discovered amount was silently lost (persisted as NULL forever)
// even though the very next call, l.SetVerified(entry.ID, true), DID write
// through correctly (it goes through Ledger.SetVerified by ID, not a
// direct struct mutation) — the mismatch that made this bug easy to miss.
// This test rigs resolveAmount's exact mutation sequence without any
// network call, then confirms — via l.Find, i.e. against the real
// l.Entries slice, not the pointer resolveHeldToken returned — that the
// write actually stuck.
func TestResolveHeldToken_SingleHeldAutoPickWritesThroughOnMutation(t *testing.T) {
	l := &ledger.Ledger{Entries: []ledger.Entry{{ID: "tok-remainder", Status: ledger.StatusHeld}}}
	cmd := testCmdWithFlags(false, false)
	cmd.Flags().String("token", "", "")

	entry, err := resolveHeldToken(cmd, l)
	if err != nil {
		t.Fatalf("resolveHeldToken() error = %v", err)
	}
	if entry.AmountMillis != nil {
		t.Fatalf("test setup: entry.AmountMillis = %v, want nil (uncached)", entry.AmountMillis)
	}

	// resolveAmount's exact mutation sequence on a live CheckClaim result
	// (cmd/cash_redeem.go): a direct field write, then a setter call.
	discovered := uint64(4200)
	entry.AmountMillis = &discovered
	_ = l.SetVerified(entry.ID, true)

	got, ok := l.Find("tok-remainder")
	if !ok {
		t.Fatalf("l.Find(%q) not found after mutation", "tok-remainder")
	}
	if got.AmountMillis == nil || *got.AmountMillis != discovered {
		t.Errorf("l.Entries' own copy of the entry has AmountMillis = %v, want %d — the discovered amount was lost (pre-fix: resolveHeldToken returned a pointer into l.Held()'s copy, not the live entry)", got.AmountMillis, discovered)
	}
	if !got.Verified {
		t.Errorf("l.Entries' own copy has Verified = false, want true — SetVerified always wrote through correctly even before this fix; this pins that it still does")
	}
}

// TestResolveHeldToken_InteractivePickWritesThroughOnMutation is the same
// regression as above, for the multi-held interactive-pick path
// (pickHeldToken) rather than the single-held auto-pick — pickHeldToken's
// held []ledger.Entry parameter is itself a copy (from l.Held()), so a
// pointer into it has the identical copy-vs-live-entry problem.
func TestResolveHeldToken_InteractivePickWritesThroughOnMutation(t *testing.T) {
	l := &ledger.Ledger{Entries: []ledger.Entry{
		{ID: "tok-a", Status: ledger.StatusHeld, AmountMillis: uint64Ptr(1000)},
		{ID: "tok-remainder", Status: ledger.StatusHeld},
	}}
	cmd := testCmdWithFlags(false, false)
	cmd.Flags().String("token", "", "")
	stubStdin(t, "2\n")

	var entry *ledger.Entry
	var err error
	withStdoutSuppressed(func() {
		entry, err = resolveHeldToken(cmd, l)
	})
	if err != nil {
		t.Fatalf("resolveHeldToken() error = %v", err)
	}
	if entry.ID != "tok-remainder" {
		t.Fatalf("picked %s, want tok-remainder", entry.ID)
	}

	discovered := uint64(777)
	entry.AmountMillis = &discovered
	_ = l.SetVerified(entry.ID, true)

	got, ok := l.Find("tok-remainder")
	if !ok {
		t.Fatalf("l.Find(%q) not found after mutation", "tok-remainder")
	}
	if got.AmountMillis == nil || *got.AmountMillis != discovered {
		t.Errorf("l.Entries' own copy has AmountMillis = %v, want %d — lost through pickHeldToken's own copy", got.AmountMillis, discovered)
	}
}

func uint64Ptr(v uint64) *uint64 { return &v }

// TestRedeemInvoiceAmount_ExternalRedemptionBug is the pure-function
// regression test for the bug found auditing this file: before the fix,
// the destination invoice always asked for the token's full face amount,
// which a real lokihub instance's cash_redeem_controller.go step 9 rejects
// outright (ERROR_BAD_REQUEST) the moment a nonzero RedeemFeePpm applies
// and the redemption resolves to a genuinely external payment — confirmed
// live in integration/cash_redeem_fee_test.go
// (TestCashRedeemFee_ExternalRedemption_FullAmountInvoiceRejected). This
// pins the fixed contract at the unit level, no network required: always
// the net (fee-reduced) amount.
func TestRedeemInvoiceAmount_ExternalRedemptionBug(t *testing.T) {
	q := redeemQuote{AmountMillis: 1000, RedeemFeeMillis: 100, NetRedeemableMillis: 900}
	if got := redeemInvoiceAmount(q); got != 900 {
		t.Errorf("redeemInvoiceAmount() = %d, want 900 (the net amount) — the pre-fix bug requested the full 1000, which lokihub rejects for a genuinely external redemption once RedeemFeePpm > 0", got)
	}
}

// TestRedeemInvoiceAmount_ZeroFeeUnaffected confirms the fix is a no-op for
// every existing test fixture and (per NIP-CASH's own "same-node redeems
// are routinely free" framing) presumably the common real deployment too:
// with no fee configured, NetRedeemableMillis == AmountMillis by
// construction (lokihub's list_recipients_controller.go computes it as a
// plain subtraction), so there's exactly one correct amount either way.
func TestRedeemInvoiceAmount_ZeroFeeUnaffected(t *testing.T) {
	q := redeemQuote{AmountMillis: 1000, RedeemFeeMillis: 0, NetRedeemableMillis: 1000}
	if got := redeemInvoiceAmount(q); got != 1000 {
		t.Errorf("redeemInvoiceAmount() = %d, want 1000 (zero-fee case must be unchanged)", got)
	}
}

// TestPreviewSuffix_FeeOnlyMentionedWhenNonZero guards previewSuffix's own
// "same-node redeems are routinely free" framing: no fee line at all when
// RedeemFeeMillis is zero, so the vastly-more-common zero-fee case doesn't
// pick up needless preamble.
func TestPreviewSuffix_FeeOnlyMentionedWhenNonZero(t *testing.T) {
	if got := previewSuffix(redeemQuote{AmountMillis: 1000, NetRedeemableMillis: 1000}); got != "" {
		t.Errorf("previewSuffix() with no fee/expiry = %q, want empty", got)
	}
	got := previewSuffix(redeemQuote{AmountMillis: 1000, RedeemFeeMillis: 100, NetRedeemableMillis: 900})
	if !strings.Contains(got, "900") || !strings.Contains(got, "fee") {
		t.Errorf("previewSuffix() with a nonzero fee = %q, want it to mention the fee and the net (900) amount", got)
	}
}
