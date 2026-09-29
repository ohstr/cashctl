package cmd

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
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

// withCapturedOutput is withCapturedStdout for text that's now narration —
// pick-lists, prompts, previews — and so goes to stderr (AGENTS.md): both
// streams, merged, since these tests assert on WHAT is printed (never a raw
// ID), not which stream carries it. Stream routing is asserted separately
// (prompt_test.go's own captureStreams).
func withCapturedOutput(fn func()) string {
	realOut, realErr := os.Stdout, os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		fn()
		return ""
	}
	os.Stdout, os.Stderr = w, w
	out := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		out <- string(b)
	}()
	fn()
	os.Stdout, os.Stderr = realOut, realErr
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

	printed := withCapturedOutput(func() {
		_, _ = pickHeldToken(cmd, entries)
	})

	if strings.Contains(printed, "tok-") {
		t.Fatalf("pickHeldToken printed a raw ledger ID:\n%s", printed)
	}
	if !strings.Contains(printed, "1 loki") {
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
	if !strings.Contains(got, "0.9 loki") || !strings.Contains(got, "Fee") {
		t.Errorf("previewSuffix() with a nonzero fee = %q, want it to mention the fee and the net (900 mloki = 0.9 loki) amount", got)
	}
}

// --- safeDestinationLabel: the fix for a real secret leak. resolveDestWallet
// used to return the raw connection string itself as the display name for
// an unregistered `--into`/positional destination — reaching the confirm
// prompt, --json's to_wallet, and persisted wallet history.

func TestSafeDestinationLabel_NWCURIShowsPubkeyNotSecret(t *testing.T) {
	uri := "nostr+walletconnect://" + strings.Repeat("a1", 32) + "?relay=wss%3A%2F%2Fx.invalid&secret=" + strings.Repeat("b2", 32)
	got := safeDestinationLabel(uri)
	if strings.Contains(got, strings.Repeat("b2", 32)) {
		t.Errorf("safeDestinationLabel(%q) = %q, leaks the secret", uri, got)
	}
	if !strings.Contains(got, strings.Repeat("a1", 4)) {
		t.Errorf("safeDestinationLabel(%q) = %q, want it to identify the wallet by its (non-secret) pubkey prefix", uri, got)
	}
}

func TestSafeDestinationLabel_HubStringsNameOnlyTheKind(t *testing.T) {
	// A bech32 hub string has no substring that's safe to reveal (it's one
	// TLV-encoded blob including a spending secret) — unlike an NWC URI,
	// nothing here should ever echo any piece of the raw value.
	for _, raw := range []string{
		"cashhub1qqsxyzsomefakepayloadthatlookslikebech32qqq",
		"circlehub1qqsxyzsomefakepayloadthatlookslikebech32qqq",
	} {
		got := safeDestinationLabel(raw)
		if strings.Contains(got, "qqsxyz") {
			t.Errorf("safeDestinationLabel(%q) = %q, echoes part of the raw connection string", raw, got)
		}
	}
}

func TestSafeDestinationLabel_UnparseableNWCURIFallsBackSafely(t *testing.T) {
	got := safeDestinationLabel("nostr+walletconnect://not-a-valid-pubkey")
	if strings.Contains(got, "not-a-valid-pubkey") {
		t.Errorf("safeDestinationLabel() = %q, echoes the unparseable raw value", got)
	}
}

// TestTruncateInvoiceForDisplay guards against the bug found auditing
// `redeem --invoice`: destName used to be the literal string "the invoice
// above", naming a line the command never actually printed in either
// mode. Unlike safeDestinationLabel's own wallet-connection case, an
// invoice carries no secret, so this is purely a readability trim, not a
// redaction — a short invoice is returned whole.
func TestTruncateInvoiceForDisplay(t *testing.T) {
	long := "lnbc1" + strings.Repeat("a", 200)
	got := truncateInvoiceForDisplay(long)
	if got != long[:12] {
		t.Errorf("truncateInvoiceForDisplay(long) = %q, want the first 12 chars %q", got, long[:12])
	}
	short := "lnbc1x"
	if got := truncateInvoiceForDisplay(short); got != short {
		t.Errorf("truncateInvoiceForDisplay(short) = %q, want it returned whole (%q)", got, short)
	}
}

// TestResolveHeldToken_SpentTokenIsRefusedWithoutDialling covers the case
// --token can reach but the auto-pick path cannot: a bill this ledger already
// recorded as spent.
//
// It matters because of what the Hub does now. A Hub deletes a bill once
// nothing is left on it and then answers nothing at all about it, so dialling
// one does not fail fast -- it waits out the whole timeout and then reports a
// network problem. Our own ledger already knows the answer the Hub is
// refusing to give.
func TestResolveHeldToken_SpentTokenIsRefusedWithoutDialling(t *testing.T) {
	for _, status := range []string{
		ledger.StatusRedeemed,
		ledger.StatusTransferred,
		ledger.StatusConsolidated,
	} {
		t.Run(status, func(t *testing.T) {
			l := heldLedger(2)
			l.Entries[1].Status = status

			cmd := testCmdWithFlags(false, false)
			cmd.Flags().String("token", l.Entries[1].ID, "")

			entry, err := resolveHeldToken(cmd, l)
			if err == nil {
				t.Fatalf("resolveHeldToken() on a %s token returned entry %v, want an error", status, entry)
			}
			if !strings.Contains(err.Error(), "no longer exists on the Hub") {
				t.Fatalf("error = %q, want it to say the bill is gone", err)
			}
		})
	}
}

// TestResolveHeldToken_StillHeldTokenIsAccepted is the other half: the gate
// above must not reject a bill that is still spendable.
func TestResolveHeldToken_StillHeldTokenIsAccepted(t *testing.T) {
	l := heldLedger(2)
	cmd := testCmdWithFlags(false, false)
	cmd.Flags().String("token", l.Entries[1].ID, "")

	entry, err := resolveHeldToken(cmd, l)
	if err != nil {
		t.Fatalf("resolveHeldToken() on a held token error = %v", err)
	}
	if entry.ID != l.Entries[1].ID {
		t.Fatalf("got %s, want %s", entry.ID, l.Entries[1].ID)
	}
}

// --- resolveHeldTokensForRedeem: multi-bill selection. Redeeming more than one
// token in a single command was previously impossible — pickHeldToken hard-errored
// under --json/--yes with "specify which with --token" and the whole flow was
// single-entry — so all of this is new capability rather than changed behaviour.

// redeemCmdWithTokens builds redeem's real flag set: --token is a StringSlice
// here, unlike transfer/inspect/protect, which keep a single-value --token and
// still share resolveHeldToken.
func redeemCmdWithTokens(jsonMode, yes bool, tokens []string, all bool) *cobra.Command {
	cmd := testCmdWithFlags(jsonMode, yes)
	cmd.Flags().StringSlice("token", tokens, "")
	cmd.Flags().Bool("all", all, "")
	return cmd
}

func heldIDs(entries []*ledger.Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}

func TestResolveHeldTokensForRedeem_SeveralExplicitTokens(t *testing.T) {
	l := heldLedger(4)
	cmd := redeemCmdWithTokens(true, true, []string{"tok-0", "tok-2"}, false)

	got, err := resolveHeldTokensForRedeem(cmd, l)
	if err != nil {
		t.Fatalf("resolveHeldTokensForRedeem() error = %v", err)
	}
	if want := []string{"tok-0", "tok-2"}; !reflect.DeepEqual(heldIDs(got), want) {
		t.Errorf("selected %v, want %v", heldIDs(got), want)
	}
}

// TestResolveHeldTokensForRedeem_ReturnsLivePointers is the same copy-vs-pointer
// contract resolveHeldToken has its own long doc comment about: l.Held() returns
// copies, so a pointer into that slice looks live and silently does not write
// through. resolveRedeemQuote writes the discovered amount straight onto the
// entry it is handed, so getting this wrong loses it permanently.
func TestResolveHeldTokensForRedeem_ReturnsLivePointers(t *testing.T) {
	l := heldLedger(3)
	cmd := redeemCmdWithTokens(true, true, []string{"tok-0", "tok-1"}, false)

	got, err := resolveHeldTokensForRedeem(cmd, l)
	if err != nil {
		t.Fatalf("resolveHeldTokensForRedeem() error = %v", err)
	}
	for _, e := range got {
		amt := uint64(424242)
		e.AmountMillis = &amt
	}
	for i := 0; i < 2; i++ {
		if l.Entries[i].AmountMillis == nil || *l.Entries[i].AmountMillis != 424242 {
			t.Errorf("l.Entries[%d].AmountMillis = %v, want the write to land on the live entry", i, l.Entries[i].AmountMillis)
		}
	}
}

func TestResolveHeldTokensForRedeem_AllSelectsEveryHeldToken(t *testing.T) {
	l := heldLedger(3)
	cmd := redeemCmdWithTokens(true, true, nil, true)

	got, err := resolveHeldTokensForRedeem(cmd, l)
	if err != nil {
		t.Fatalf("resolveHeldTokensForRedeem(--all) error = %v", err)
	}
	if len(got) != 3 {
		t.Errorf("--all selected %d tokens, want all 3", len(got))
	}
}

// TestResolveHeldTokensForRedeem_DuplicateTokenRefused matters because the second
// attempt would not fail cleanly: a Hub deletes a spent slice and then stays
// silent about it, so redeeming the same bill twice in one run would wait out the
// whole timeout and report a network failure for what is really a caller mistake.
func TestResolveHeldTokensForRedeem_DuplicateTokenRefused(t *testing.T) {
	l := heldLedger(2)
	cmd := redeemCmdWithTokens(true, true, []string{"tok-0", "tok-0"}, false)

	if _, err := resolveHeldTokensForRedeem(cmd, l); err == nil {
		t.Fatal("expected an error when the same token is named twice")
	}
}

func TestResolveHeldTokensForRedeem_TokenAndAllTogetherRefused(t *testing.T) {
	l := heldLedger(2)
	cmd := redeemCmdWithTokens(true, true, []string{"tok-0"}, true)

	if _, err := resolveHeldTokensForRedeem(cmd, l); err == nil {
		t.Fatal("expected an error when both --token and --all are given")
	}
}

// TestResolveHeldTokensForRedeem_SpentTokenRefused keeps the rule the
// single-token path already enforced, now applied to every id in the list.
func TestResolveHeldTokensForRedeem_SpentTokenRefused(t *testing.T) {
	l := heldLedger(2)
	l.Entries[1].Status = ledger.StatusRedeemed
	cmd := redeemCmdWithTokens(true, true, []string{"tok-0", "tok-1"}, false)

	if _, err := resolveHeldTokensForRedeem(cmd, l); err == nil {
		t.Fatal("expected an error for a token the ledger already records as redeemed")
	}
}

func TestResolveHeldTokensForRedeem_SingleHeldStillAutoPicks(t *testing.T) {
	l := heldLedger(1)
	cmd := redeemCmdWithTokens(false, false, nil, false)

	got, err := resolveHeldTokensForRedeem(cmd, l)
	if err != nil {
		t.Fatalf("resolveHeldTokensForRedeem() error = %v", err)
	}
	if len(got) != 1 || got[0].ID != "tok-0" {
		t.Errorf("selected %v, want just tok-0 — the single-held auto-pick must be unchanged", heldIDs(got))
	}
}

// TestResolveHeldTokensForRedeem_JSONModeStillRefusesToGuess pins a deliberate
// asymmetry with consolidate, which DOES process every group under --json.
// Redeeming pays out irreversibly, so a script that did not say which bills it
// meant must not have that decided for it — and the error has to name --all,
// which is how a script says it means all of them.
func TestResolveHeldTokensForRedeem_JSONModeStillRefusesToGuess(t *testing.T) {
	l := heldLedger(3)
	cmd := redeemCmdWithTokens(true, false, nil, false)

	_, err := resolveHeldTokensForRedeem(cmd, l)
	if err == nil {
		t.Fatal("expected a usage error under --json with several held tokens")
	}
	if !strings.Contains(err.Error(), "--all") {
		t.Errorf("error does not mention --all, so a script cannot discover how to proceed: %v", err)
	}
}

// --- printRedeemOutcomes / firstRedeemError: reporting a run in which some
// payouts succeeded and others did not.

func okOutcome(id string, amount uint64, fee uint64, preimage string) redeemOutcome {
	return redeemOutcome{
		EntryID:      id,
		AmountMillis: &amount,
		Result:       &nipcash.CashRedeemResult{FeesPaid: fee, Preimage: preimage},
	}
}

// TestPrintRedeemOutcomes_SingleSuccessKeepsTheLegacyJSONShape guards the --json
// contract: the one-bill case is what every existing consumer parses, so it must
// keep emitting a top-level redeemed_token/preimage object rather than an array.
func TestPrintRedeemOutcomes_SingleSuccessKeepsTheLegacyJSONShape(t *testing.T) {
	outcomes := []redeemOutcome{okOutcome("tok-0", 1000, 7, "deadbeef")}
	out := withCapturedStdout(func() { printRedeemOutcomes(true, outcomes, "savings", "") })

	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not one JSON object: %v\n%s", err, out)
	}
	if got["redeemed_token"] != "tok-0" {
		t.Errorf("lost the top-level redeemed_token — this breaks every --json consumer:\n%s", out)
	}
	if got["preimage"] != "deadbeef" {
		t.Errorf("lost the top-level preimage:\n%s", out)
	}
	if _, ok := got["redeemed"]; ok {
		t.Errorf("single success used the array shape, changing the established contract:\n%s", out)
	}
}

// TestPrintRedeemOutcomes_PartialRunReportsEveryBill is the core of why this work
// exists. Each payout is separate and irreversible, so a run where bill 1 paid
// and bill 2 failed must report BOTH — the preimage of the one that paid is the
// only proof that it did.
func TestPrintRedeemOutcomes_PartialRunReportsEveryBill(t *testing.T) {
	outcomes := []redeemOutcome{
		okOutcome("tok-0", 1000, 0, "beef01"),
		{EntryID: "tok-1", Err: output.NetworkError(&cobra.Command{}, errors.New("relay went away"))},
	}
	out := withCapturedStdout(func() { printRedeemOutcomes(true, outcomes, "savings", "") })

	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not one JSON object: %v\n%s", err, out)
	}
	rows, _ := got["redeemed"].([]any)
	if len(rows) != 2 {
		t.Fatalf("reported %d bills, want 2 — a failure must not hide the one that paid:\n%s", len(rows), out)
	}
	first, _ := rows[0].(map[string]any)
	second, _ := rows[1].(map[string]any)
	if first["status"] != "ok" || first["preimage"] != "beef01" {
		t.Errorf("bill 1 lost its proof of payment: %v", first)
	}
	if second["status"] != "failed" || second["code"] != "network" {
		t.Errorf("bill 2 not reported as a classified failure: %v", second)
	}
}

// TestPrintRedeemOutcomes_NotAttemptedIsNotAFailure covers the third state. A
// bill in a run the person declined had no wire call made for it, so reporting it
// as failed would invite a retry of something that never happened.
func TestPrintRedeemOutcomes_NotAttemptedIsNotAFailure(t *testing.T) {
	outcomes := []redeemOutcome{okOutcome("tok-0", 1000, 0, "beef01"), {EntryID: "tok-1"}}
	out := withCapturedStdout(func() { printRedeemOutcomes(true, outcomes, "savings", "") })

	if !strings.Contains(out, `"status": "not_attempted"`) {
		t.Errorf("a bill that was never attempted is not reported as such:\n%s", out)
	}
	if err := firstRedeemError(outcomes); err != nil {
		t.Errorf("firstRedeemError = %v, want nil — nothing failed, so the exit code must stay 0", err)
	}
}

func TestPrintRedeemOutcomes_InvoiceDestinationUsesItsOwnField(t *testing.T) {
	outcomes := []redeemOutcome{okOutcome("tok-0", 1000, 0, "beef01"), {EntryID: "tok-1"}}
	out := withCapturedStdout(func() { printRedeemOutcomes(true, outcomes, "invoice lnbc1…", "lnbc1fullinvoice") })

	if !strings.Contains(out, `"to_invoice": "lnbc1fullinvoice"`) {
		t.Errorf("an invoice destination must report the whole invoice under to_invoice, not a truncated to_wallet:\n%s", out)
	}
	if strings.Contains(out, `"to_wallet"`) {
		t.Errorf("to_wallet must not be set for an invoice destination:\n%s", out)
	}
}

func TestFirstRedeemError_PreservesClassification(t *testing.T) {
	classified := output.NetworkError(&cobra.Command{}, errors.New("relay unreachable"))
	outcomes := []redeemOutcome{okOutcome("tok-0", 1000, 0, "beef01"), {EntryID: "tok-1", Err: classified}}

	got := firstRedeemError(outcomes)
	if got == nil {
		t.Fatal("firstRedeemError = nil, want the failing bill's error")
	}
	if output.ExitCode(got) != output.ExitCode(classified) {
		t.Errorf("exit code = %d, want %d — classification was lost", output.ExitCode(got), output.ExitCode(classified))
	}
}

// TestRedeemRecoveryHint_NamesEveryPreimage covers the worst case in this file:
// the payouts happened and recording them locally did not. A preimage is the only
// proof a specific payment occurred, so every one has to be in the message.
func TestRedeemRecoveryHint_NamesEveryPreimage(t *testing.T) {
	outcomes := []redeemOutcome{
		okOutcome("tok-0", 1000, 0, "beef01"),
		{EntryID: "tok-1", Err: errors.New("nope")},
		okOutcome("tok-2", 3000, 0, "beef02"),
	}
	got := redeemRecoveryHint(outcomes)
	for _, want := range []string{"tok-0=beef01", "tok-2=beef02"} {
		if !strings.Contains(got, want) {
			t.Errorf("recovery hint is missing %q — that payment becomes unreconcilable: %s", want, got)
		}
	}
	if strings.Contains(got, "tok-1") {
		t.Errorf("recovery hint names a bill that never paid out: %s", got)
	}
}

func TestRedeemConfirmMessage_SingleBillUnchangedMultiShowsTheTotal(t *testing.T) {
	one := []redeemPlan{{Quote: redeemQuote{AmountMillis: 1000}}}
	if got := redeemConfirmMessage(one, "savings"); !strings.Contains(got, "into savings?") || strings.Contains(got, "tokens") {
		t.Errorf("single-bill prompt changed shape: %q", got)
	}
	two := []redeemPlan{{Quote: redeemQuote{AmountMillis: 1000}}, {Quote: redeemQuote{AmountMillis: 2000}}}
	got := redeemConfirmMessage(two, "savings")
	if !strings.Contains(got, "2 tokens") {
		t.Errorf("multi-bill prompt does not say how many: %q", got)
	}
	// The total is the number a person actually has to check before agreeing.
	if !strings.Contains(got, output.FormatAmount(3000)) {
		t.Errorf("multi-bill prompt does not show the total: %q", got)
	}
}

// TestRedeemTimeout_ScalesWithBillCount guards against the failure mode a fixed
// budget would reintroduce: aborting a large batch part-way through, which for a
// money path is the worst available outcome, since some bills would have paid out
// and the rest would be unknown.
func TestRedeemTimeout_ScalesWithBillCount(t *testing.T) {
	if one, four := redeemTimeout(1), redeemTimeout(4); four <= one {
		t.Errorf("redeemTimeout(4) = %v, want more than redeemTimeout(1) = %v", four, one)
	}
	if got := redeemTimeout(0); got <= 0 {
		t.Errorf("redeemTimeout(0) = %v, want a positive budget", got)
	}
	// Bounded, so a caller passing an absurd count cannot hang indefinitely.
	if got := redeemTimeout(10000); got > 5*time.Minute {
		t.Errorf("redeemTimeout(10000) = %v, want it capped", got)
	}
}

// TestWorthReportingRedeemRun_SingleFailureStaysQuietOnStdout pins the one place
// this work must NOT change behaviour. A single bill that failed has always been
// described by its error on stderr alone, with nothing on stdout; a script that
// checked for empty stdout on failure must keep working. The per-bill report only
// earns its place once the error cannot describe the run by itself.
func TestWorthReportingRedeemRun_SingleFailureStaysQuietOnStdout(t *testing.T) {
	justFailed := []redeemOutcome{{EntryID: "tok-0", Err: errors.New("nope")}}
	if worthReportingRedeemRun(justFailed) {
		t.Error("a lone failed bill should print no result document — its error already says everything")
	}
	// As soon as anything paid out, silence would hide a real payment.
	if !worthReportingRedeemRun([]redeemOutcome{okOutcome("tok-0", 1000, 0, "beef01")}) {
		t.Error("a bill that paid out must be reported")
	}
	// Several bills: which one failed is information the single error cannot carry.
	twoBills := []redeemOutcome{{EntryID: "tok-0", Err: errors.New("nope")}, {EntryID: "tok-1", Err: errors.New("nope")}}
	if !worthReportingRedeemRun(twoBills) {
		t.Error("with several bills the per-bill report is the only way to see which failed")
	}
}
