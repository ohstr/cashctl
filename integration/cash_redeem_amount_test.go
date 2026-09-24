//go:build integration

package integration

import (
	"strings"
	"testing"
)

// Characterisation tests for redeem's CURRENT bill-level model, written
// ahead of the amount-first work (docs/private/redeem-ux-review.md, F3).
//
// They exist to make the 0.5.0 change visible rather than silent: each one
// pins a behaviour the review proposes to change, so the day `redeem` learns
// an amount, these fail loudly and are rewritten into the new contract
// instead of quietly continuing to pass against a command that no longer
// works that way.
//
// Where a test asserts a REFUSAL, the refusal is the thing being pinned —
// not an assertion that refusing is correct.

// TestRedeem_NumericArgIsAWalletNameNotAnAmount pins the sharpest edge of
// having no amount concept: `redeem 500` today does not mean "redeem 500",
// it means "redeem into the wallet named 500". A user reaching for the
// transfer-shaped invocation gets a destination lookup, not a partial
// cash-out. Once redeem takes an amount this must change meaning.
func TestRedeem_NumericArgIsAWalletNameNotAnAmount(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 40_000))

	res := f.run("redeem", "500", "--yes")
	if res.ExitCode == 0 {
		t.Fatalf("`redeem 500` succeeded — if it now means an amount, rewrite this test to the new contract:\n%s", res.Stdout)
	}
	// The giveaway: it complains about a destination, never about an amount.
	if !strings.Contains(res.Stderr, "500") {
		t.Errorf("expected the error to quote %q as the thing it could not resolve, got: %s", "500", res.Stderr)
	}
}

// TestRedeem_ConsumesTheWholeBill pins that redeem is all-or-nothing: there
// is no partial cash-out, so a redeemed bill leaves no change behind. Under
// the amount-first model a partial redeem must leave a remainder entry, the
// way a split transfer already does.
func TestRedeem_ConsumesTheWholeBill(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	entry := f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 40_000))["entry"].(map[string]any)
	id := entryID(t, entry)

	if res := f.run("redeem", "--invoice", makeHubInvoice(t, hub, 40_000), "--yes"); res.ExitCode != 0 {
		t.Fatalf("redeem: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	// Nothing left: the whole bill went, no change entry was created.
	held, _ := f.mustJSON("wallet", "show")["held_tokens"].([]any)
	for _, h := range held {
		if e, _ := h.(map[string]any); e["id"] == id {
			t.Errorf("bill %s still held after a full redeem", id)
		}
	}
	if len(held) != 0 {
		t.Errorf("redeem left %d held bill(s) — today it is all-or-nothing with no change: %v", len(held), held)
	}
}

// TestRedeem_CannotCombineBills pins the limitation the review's worked
// example turns on: holding several bills that together cover an amount,
// redeem still cannot produce it, because it acts on exactly one bill and
// never consolidates. Amount-first redeem must be able to reach 500 from
// 100 + 400 + 50 by merging first, the way transfer already does.
func TestRedeem_CannotCombineBills(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	for _, amount := range []uint64{10_000, 40_000, 5_000} {
		f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, amount))
	}

	// Three bills held, none named: today this is a refusal, not a selection.
	res := f.run("redeem", "--yes")
	if res.ExitCode == 0 {
		t.Fatalf("redeem with 3 bills held succeeded — if it now selects, rewrite this test:\n%s", res.Stdout)
	}
	assertTerminalFailure(t, res, "redeem with several bills and none named", "usage")
	if !strings.Contains(res.Stderr, "--token") {
		t.Errorf("expected the refusal to name --token as the escape hatch, got: %s", res.Stderr)
	}

	// And the sum is unreachable: no invocation redeems 55_000 across them.
	// The largest single bill caps what one redeem can move.
	if res := f.run("redeem", "--invoice", makeHubInvoice(t, hub, 55_000), "--yes"); res.ExitCode == 0 {
		t.Errorf("redeem paid an invoice larger than any single held bill — redeem can now combine bills, rewrite this test")
	}
}

// TestRedeem_FeeComesOffTheSameBill pins today's fee sourcing, which the
// review proposes to change. With one bill and a fee-charging Hub, the fee
// is taken from that bill and the payout is the net — there is no way to
// source the fee from a sibling bill. The net model makes the fee payable
// out of whatever is held, so this becomes one case of many rather than the
// only possible behaviour.
func TestRedeem_FeeComesOffTheSameBill(t *testing.T) {
	admin := adminOrSkip(t)
	// 1% redeem fee, so net and gross are observably different.
	hub := setUpCashHubOpts(t, admin, cashHubOpts{RedeemFeePpm: 10_000})
	f := newFixture(t)
	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 100_000))

	// The auto-invoice path requests the NET amount (redeemInvoiceAmount),
	// so a full-amount invoice is refused — the fee has nowhere else to come
	// from. This is exactly the shortfall the net model solves by sourcing
	// the fee from other bills.
	res := f.run("redeem", "--invoice", makeHubInvoice(t, hub, 100_000), "--yes")

	// Both outcomes are legitimate — the quote is a worst case, and a
	// same-node redeem may waive the fee entirely (nipcash/list_recipients.go
	// :12-15) — so assert on both rather than skipping the inconvenient one.
	held, _ := f.mustJSON("wallet", "show")["held_tokens"].([]any)
	if res.ExitCode == 0 {
		t.Logf("this Hub waived the fee (same-node redeem); asserting the payout instead of the rejection")
		if len(held) != 0 {
			t.Errorf("redeem reported success but %d bill(s) are still held: %v", len(held), held)
		}
		return
	}
	// Refused: the fee had nowhere to come from, which is precisely the
	// shortfall the net model solves by sourcing it from other bills.
	assertTerminalFailure(t, res, "full-amount invoice against a fee-charging Hub", "invalid_input", "internal")
	if len(held) != 1 {
		t.Errorf("a refused redeem must leave the bill untouched, got %d held", len(held))
	}
}
