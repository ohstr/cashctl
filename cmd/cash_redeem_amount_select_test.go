package cmd

// Unit coverage for --amount's own pure logic: the exact-sum search and the
// --invoice/--amount interaction (docs/private/amount-first-decisions.md,
// questions 1 and 3). The selection function itself
// (selectHeldTokensForRedeemAmount) needs a live quote per candidate and is
// exercised in integration/, not here — these are the parts with no network
// dependency at all.

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestExactSubsetSumNet_SingleExactValue(t *testing.T) {
	idxs, ok := exactSubsetSumNet([]uint64{200_000, 150_000, 250_000}, 150_000)
	if !ok {
		t.Fatal("exactSubsetSumNet() = not found, want the single 150_000 entry")
	}
	if len(idxs) != 1 || idxs[0] != 1 {
		t.Errorf("idxs = %v, want [1]", idxs)
	}
}

func TestExactSubsetSumNet_PairSums(t *testing.T) {
	// 200 + 150 = 350, matching neither alone — the exact-sum case this
	// whole search exists for.
	idxs, ok := exactSubsetSumNet([]uint64{200_000, 150_000, 250_000}, 350_000)
	if !ok {
		t.Fatal("exactSubsetSumNet() = not found, want 200_000+150_000")
	}
	sum := uint64(0)
	for _, i := range idxs {
		sum += []uint64{200_000, 150_000, 250_000}[i]
	}
	if sum != 350_000 {
		t.Errorf("selected indices %v sum to %d, want 350000", idxs, sum)
	}
}

func TestExactSubsetSumNet_NoCombination(t *testing.T) {
	// No subset of {200,150,250} sums to 300 — confirmed by hand: every
	// subset total is one of 0,150,200,250,350,400,450,600.
	if _, ok := exactSubsetSumNet([]uint64{200_000, 150_000, 250_000}, 300_000); ok {
		t.Error("exactSubsetSumNet() = found, want none — no subset of {200,150,250} sums to 300")
	}
}

func TestExactSubsetSumNet_EmptyGroupNeverMatchesZero(t *testing.T) {
	// mask starts at 1 (never 0), so an empty selection is never returned —
	// a target of 0 would otherwise match trivially and redeem nothing.
	if _, ok := exactSubsetSumNet([]uint64{100, 200}, 0); ok {
		t.Error("exactSubsetSumNet(_, 0) = found, want none — an empty subset must never count as a match")
	}
}

// TestExactSubsetSumNet_OverLimitRefusesRatherThanSearching confirms the
// bound is real: a holder with more same-Hub tokens than
// exactSubsetSumNetLimit gets "no exact sum found" (the same outcome as
// fragmentation) instead of a 2^N search no one asked to wait for.
func TestExactSubsetSumNet_OverLimitRefusesRatherThanSearching(t *testing.T) {
	nets := make([]uint64, exactSubsetSumNetLimit+1)
	for i := range nets {
		nets[i] = uint64(i + 1)
	}
	if _, ok := exactSubsetSumNet(nets, 1); ok {
		t.Error("exactSubsetSumNet() searched past its own limit — want a bounded refusal")
	}
}

func newTestRedeemAmountCmd(jsonMode bool) *cobra.Command {
	cmd := testCmdWithFlags(jsonMode, false)
	cmd.Flags().String("amount", "", "")
	return cmd
}

// fixedAmountInvoice and openInvoice are real, validly-encoded bech32
// strings (built with the same chainutil/bech32 encoder bolt11AmountMloki's
// own decoder reads) — not made up. A hand-typed fixture that merely LOOKS
// bech32-shaped fails bech32's checksum and these tests would all fail on
// "not a valid Lightning invoice" rather than on the behavior they exist to
// check; verified by running the encoder before writing these in.
const (
	fixedAmountInvoice = "lnbc1m1pzry9ncv45g" // hrp "lnbc1m" — the "m" multiplier means an encoded amount
	openInvoice        = "lnbc1pzry97n9xsu"   // hrp "lnbc" — no multiplier at all, bolt11AmountMloki's own "amountless invoice — legal" case
)

// TestInvoiceAmountOverride_FixedInvoiceRejectsAmount is the LN-wallet
// convention this decision is modelled on: a pasted invoice that already
// carries an amount locks the amount field.
func TestInvoiceAmountOverride_FixedInvoiceRejectsAmount(t *testing.T) {
	cmd := newTestRedeemAmountCmd(false)
	if err := cmd.Flags().Set("amount", "500"); err != nil {
		t.Fatal(err)
	}
	_, err := invoiceAmountOverride(cmd, fixedAmountInvoice)
	if err == nil {
		t.Fatal("invoiceAmountOverride(fixed-amount invoice, --amount also given) = nil, want rejected")
	}
	if !strings.Contains(err.Error(), "already asks for") {
		t.Errorf("error = %q, want it to name the invoice's own fixed amount", err)
	}
}

// TestInvoiceAmountOverride_OpenInvoiceRequiresAmount is the other half of
// the same convention: an amount-less invoice has nowhere else for the
// figure to come from.
func TestInvoiceAmountOverride_OpenInvoiceRequiresAmount(t *testing.T) {
	cmd := newTestRedeemAmountCmd(false)
	// No "m"/"u"/"n"/"p" multiplier in the human-readable part at all —
	// bolt11AmountMloki's own "amountless invoice — legal" case.
	_, err := invoiceAmountOverride(cmd, openInvoice)
	if err == nil {
		t.Fatal("invoiceAmountOverride(open invoice, no --amount) = nil, want required")
	}
	if !strings.Contains(err.Error(), "doesn't encode an amount") {
		t.Errorf("error = %q, want it to say the invoice has no amount of its own", err)
	}
}

// TestInvoiceAmountOverride_OpenInvoiceWithAmountWorks confirms the
// --amount value is parsed and returned (in mloki) when it's actually
// needed and actually given.
func TestInvoiceAmountOverride_OpenInvoiceWithAmountWorks(t *testing.T) {
	cmd := newTestRedeemAmountCmd(false)
	if err := cmd.Flags().Set("amount", "5"); err != nil {
		t.Fatal(err)
	}
	got, err := invoiceAmountOverride(cmd, openInvoice)
	if err != nil {
		t.Fatalf("invoiceAmountOverride() error = %v", err)
	}
	if got == nil || *got != 5000 {
		t.Errorf("got = %v, want a pointer to 5000 (5 loki in mloki)", got)
	}
}

func TestInvoiceAmountOverride_MalformedInvoiceRejected(t *testing.T) {
	cmd := newTestRedeemAmountCmd(false)
	_, err := invoiceAmountOverride(cmd, "not-an-invoice")
	if err == nil {
		t.Fatal("invoiceAmountOverride(garbage) = nil, want rejected")
	}
}

// TestResolveHeldTokensForRedeem_AmountConflictsWithTokenOrAll pins the
// mutual-exclusivity check: --amount selects which tokens to use, so naming
// them ALSO via --token/--all is a contradiction, not a combination.
func TestResolveHeldTokensForRedeem_AmountConflictsWithTokenAndAll(t *testing.T) {
	t.Run("with --token", func(t *testing.T) {
		cmd := testCmdWithFlags(false, false)
		cmd.Flags().StringSlice("token", nil, "")
		cmd.Flags().Bool("all", false, "")
		cmd.Flags().String("amount", "", "")
		cmd.Flags().String("invoice", "", "")
		if err := cmd.Flags().Set("amount", "5"); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Flags().Set("token", "tok-a"); err != nil {
			t.Fatal(err)
		}
		_, err := resolveHeldTokensForRedeem(cmd, nil)
		if err == nil {
			t.Fatal("resolveHeldTokensForRedeem(--amount + --token) = nil, want rejected")
		}
	})
	t.Run("with --all", func(t *testing.T) {
		cmd := testCmdWithFlags(false, false)
		cmd.Flags().StringSlice("token", nil, "")
		cmd.Flags().Bool("all", false, "")
		cmd.Flags().String("amount", "", "")
		cmd.Flags().String("invoice", "", "")
		if err := cmd.Flags().Set("amount", "5"); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Flags().Set("all", "true"); err != nil {
			t.Fatal(err)
		}
		_, err := resolveHeldTokensForRedeem(cmd, nil)
		if err == nil {
			t.Fatal("resolveHeldTokensForRedeem(--amount + --all) = nil, want rejected")
		}
	})
}

// TestResolveHeldTokensForRedeem_AmountInvalid confirms a malformed --amount
// is caught before any selection logic runs, not left to surface as a
// confusing downstream failure.
func TestResolveHeldTokensForRedeem_AmountInvalid(t *testing.T) {
	cmd := testCmdWithFlags(false, false)
	cmd.Flags().StringSlice("token", nil, "")
	cmd.Flags().Bool("all", false, "")
	cmd.Flags().String("amount", "", "")
	cmd.Flags().String("invoice", "", "")
	if err := cmd.Flags().Set("amount", "not-a-number"); err != nil {
		t.Fatal(err)
	}
	_, err := resolveHeldTokensForRedeem(cmd, nil)
	if err == nil {
		t.Fatal("resolveHeldTokensForRedeem(--amount garbage) = nil, want rejected")
	}
}
