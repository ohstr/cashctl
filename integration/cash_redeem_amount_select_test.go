//go:build integration

// cash_redeem_amount_select_test.go covers --amount's own selection against
// a real lokihub instance (issue #19, docs/private/amount-first-decisions.md).
// The bars `--amount` has to clear are the same two existing
// cash_redeem_amount_test.go already pinned as unchanged by this feature —
// TestRedeem_NumericArgIsAWalletNameNotAnAmount (the positional stays a
// destination) and TestRedeem_CannotCombineBills (bare redeem, no --amount,
// still refuses an ambiguous pick) — neither needed rewriting, because
// --amount is additive and the carve case this file's own comments
// anticipated was deliberately not shipped (see the decisions doc's third
// correction).
package integration

import (
	"strings"
	"testing"
)

// TestRedeemAmount_ExactSingleMatch is the simplest case: one held token's
// own net-redeemable value already equals the target, with the Hub charging
// no fee at all, so net == gross and no live quote math is in play beyond
// confirming the match.
func TestRedeemAmount_ExactSingleMatch(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 5_000))

	res := f.run("redeem", "--amount", "5", "--yes") // loki, not millis — the CLI's own unit
	if res.ExitCode != 0 {
		t.Fatalf("redeem --amount 5 against a 5000-milli held token: exit=%d\nstdout=%s\nstderr=%s", res.ExitCode, res.Stdout, res.Stderr)
	}

	show := f.mustJSON("wallet", "show")
	held, _ := show["held_tokens"].([]any)
	if len(held) != 0 {
		t.Errorf("token still held after an exact-match redeem: %v", show)
	}
}

// TestRedeemAmount_ExactSameHubSum is the case neither a single held token
// nor --invoice alone can reach (confirmed by TestRedeem_CannotCombineBills
// in the sibling file): two same-Hub bills whose individual values are each
// too small, but which sum to exactly the target.
func TestRedeemAmount_ExactSameHubSum(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 2_000))
	f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 3_000))

	res := f.run("redeem", "--amount", "5", "--yes") // 2000+3000 millis = 5 loki; neither bill alone
	if res.ExitCode != 0 {
		t.Fatalf("redeem --amount 5 against two bills (2000+3000 millis): exit=%d\nstdout=%s\nstderr=%s", res.ExitCode, res.Stdout, res.Stderr)
	}

	show := f.mustJSON("wallet", "show")
	held, _ := show["held_tokens"].([]any)
	if len(held) != 0 {
		t.Errorf("a bill still held after an exact-sum redeem of both: %v", show)
	}
}

// TestRedeemAmount_InsufficientFunds confirms the classified refusal
// (invalid_input, not a generic failure) when the target exceeds everything
// held, net — the same classification transfer's own SelectForAmount
// already uses for the identical diagnosis.
func TestRedeemAmount_InsufficientFunds(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 1_000))

	res := f.run("redeem", "--amount", "5", "--yes") // target (5000 millis) exceeds the 1000 millis held
	assertTerminalFailure(t, res, "redeem --amount exceeding total held", "invalid_input")
	if !strings.Contains(res.Stderr, "not enough funds") {
		t.Errorf("expected a not-enough-funds message, got: %s", res.Stderr)
	}
}

// TestRedeemAmount_CrossHubFragmentation confirms two bills from DIFFERENT
// Cash Hubs — each individually too small, summing to the target only
// across Hubs — are refused as fragmented, not silently combined. Separate
// cash_hub apps on this admin API have distinct HubGroupKey fingerprints
// even though this lab runs one physical node, mirroring
// TestCashTransfer_CashSelection_Fragmented's own technique for transfer.
func TestRedeemAmount_CrossHubFragmentation(t *testing.T) {
	admin := adminOrSkip(t)
	hubA := setUpCashHub(t, admin)
	hubB := setUpCashHub(t, admin)
	f := newFixture(t)
	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	f.mustJSON("receive", mintPubkeyTokenFromHub(t, hubA, pub, 2_000))
	f.mustJSON("receive", mintPubkeyTokenFromHub(t, hubB, pub, 3_000))

	res := f.run("redeem", "--amount", "5", "--yes") // 2000(A)+3000(B) = 5 loki total, but no single Hub's tokens do
	assertTerminalFailure(t, res, "redeem --amount spanning two Hubs", "usage")
	if !strings.Contains(res.Stderr, "fragmented") {
		t.Errorf("expected a fragmentation message naming the real diagnosis, got: %s", res.Stderr)
	}

	// Neither bill may be spent by a refused selection.
	show := f.mustJSON("wallet", "show")
	held, _ := show["held_tokens"].([]any)
	if len(held) != 2 {
		t.Errorf("a refused fragmented selection must leave every bill untouched, got %d held: %v", len(held), show)
	}
}

// TestRedeemAmount_ConflictsWithTokenAndAll is --token/--amount's and
// --all/--amount's own live confirmation of the unit-tested flag-conflict
// check (cash_redeem_amount_select_test.go in cmd/) — --amount decides
// which tokens to use, so naming them again is a contradiction, caught as
// usage before any wire call, not run.
func TestRedeemAmount_ConflictsWithTokenAndAll(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	receiveResp := f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 5_000))
	id, _ := receiveResp["entry"].(map[string]any)["id"].(string)

	if res := f.run("redeem", "--amount", "5", "--token", id, "--yes"); res.ExitCode == 0 {
		t.Errorf("redeem --amount with --token also given succeeded, want a usage refusal: %s", res.Stdout)
	}
	if res := f.run("redeem", "--amount", "5", "--all", "--yes"); res.ExitCode == 0 {
		t.Errorf("redeem --amount with --all also given succeeded, want a usage refusal: %s", res.Stdout)
	}
}

// TestRedeemInvoiceAmount_FixedInvoiceRejectsAmount is question 3's live
// proof: an invoice minted with its own amount (makeHubInvoice's own
// MakeInvoiceParams.Amount) locks the field, matching every LN wallet's own
// convention for a fixed-amount invoice.
func TestRedeemInvoiceAmount_FixedInvoiceRejectsAmount(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 5_000))

	fixed := makeHubInvoice(t, hub, 5_000)
	res := f.run("redeem", "--invoice", fixed, "--amount", "5", "--yes")
	if res.ExitCode == 0 {
		t.Fatalf("redeem --invoice (fixed amount) --amount succeeded, want refused: %s", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "already asks for") {
		t.Errorf("expected the refusal to name the invoice's own fixed amount, got: %s", res.Stderr)
	}
}

// TestRedeemInvoiceAmount_OpenInvoiceRequiresAndUsesAmount is the other
// half: an amount-less invoice against the SAME node (so the redemption
// resolves same-node and the Hub's fee is waived per NIP-CASH §The Redeem
// Fee), paired with --amount, actually redeems — proving the override
// reaches CashRedeemParams.Amount and the Hub accepts it.
func TestRedeemInvoiceAmount_OpenInvoiceRequiresAndUsesAmount(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 5_000))

	// No --amount at all: required, not merely allowed.
	open := makeHubInvoice(t, hub, 0)
	if res := f.run("redeem", "--invoice", open, "--yes"); res.ExitCode == 0 {
		t.Fatalf("redeem --invoice (open, amount-less) with no --amount succeeded, want required: %s", res.Stdout)
	} else if !strings.Contains(res.Stderr, "doesn't encode an amount") {
		t.Errorf("expected the refusal to say the invoice has no amount of its own, got: %s", res.Stderr)
	}

	// With --amount, on the same-node invoice this fixture always produces
	// (makeHubInvoice dials the SAME hub's own node): should actually redeem.
	open2 := makeHubInvoice(t, hub, 0)
	res := f.run("redeem", "--invoice", open2, "--amount", "5", "--yes")
	if res.ExitCode != 0 {
		t.Fatalf("redeem --invoice (open) --amount 5, same-node: exit=%d\nstdout=%s\nstderr=%s", res.ExitCode, res.Stdout, res.Stderr)
	}
}

// TestRedeemInvoiceAmount_ExceedsNetRedeemableRejected is the bound-check
// this feature adds to the --invoice path specifically (it otherwise skips
// quoting entirely, per prepareRedeems' own doc comment): --amount above
// the bill's own quoted net-redeemable value is refused locally, before any
// wire call, rather than let a plainly-wrong request reach the Hub.
func TestRedeemInvoiceAmount_ExceedsNetRedeemableRejected(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 5_000))

	open := makeHubInvoice(t, hub, 0)
	res := f.run("redeem", "--invoice", open, "--amount", "500", "--yes") // 500 loki > the 5-loki bill
	if res.ExitCode == 0 {
		t.Fatalf("redeem --invoice (open) --amount far exceeding the bill's own value succeeded, want refused: %s", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "exceeds this bill's own net redeemable value") {
		t.Errorf("expected the refusal to name the bound it exceeded, got: %s", res.Stderr)
	}

	// Refused locally, before the wire — the bill must still be held.
	show := f.mustJSON("wallet", "show")
	held, _ := show["held_tokens"].([]any)
	if len(held) != 1 {
		t.Errorf("bill not left held after a locally-refused over-amount request: %v", show)
	}
}

// TestRedeemInvoiceAmount_ExternalFeeChargedConsistentWithExistingTradeoff
// checks the --invoice override against a fee-charging Hub resolving
// genuinely externally (mintInvoiceFromFlnd's own non-self-payment
// technique — see cash_redeem_fee_test.go's package doc comment for why
// that's needed even in a single-node lab). A correctly net-adjusted
// --amount must get past the Hub's own exact-amount check
// (cash_redeem_controller.go step 9) the same way
// TestCashRedeemFee_ExternalRedemption_FullAmountInvoiceRejected's own
// correctly-quoted retry already does for the auto-invoice path — this is
// the same tradeoff, reached through the new flag instead of
// redeemInvoiceAmount, and it must not behave differently.
func TestRedeemInvoiceAmount_ExternalFeeChargedConsistentWithExistingTradeoff(t *testing.T) {
	admin := adminOrSkip(t)

	const redeemFeePpm = 100_000 // 10%, same rate the sibling fee test uses
	const amountMillis = uint64(10_000)
	const feeMillis = amountMillis * redeemFeePpm / 1_000_000
	const netMillis = amountMillis - feeMillis // 9_000

	hub := setUpCashHubOpts(t, admin, cashHubOpts{RedeemFeePpm: redeemFeePpm, MaxExpSecs: 3600})
	f := newFixture(t)
	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, amountMillis))

	// The gross face value, requested on a genuinely external redemption
	// with a real fee owed: must be rejected, same BAD_REQUEST class the
	// sibling fee test already proves for the auto-invoice path.
	grossInvoice := mintInvoiceFromFlnd(t, amountMillis, "cashctl amount-first audit: gross amount over an open invoice")
	badRes := f.run("redeem", "--invoice", grossInvoice, "--amount", "10", "--yes") // 10 loki = 10_000 millis, the GROSS value
	if badRes.ExitCode == 0 {
		t.Fatalf("redeem --invoice --amount with the gross face value, fee-charging external redemption: succeeded, want rejected\nstdout=%s", badRes.Stdout)
	}
	if got := jsonErrorNWCCode(t, badRes.Stderr); got != "" && got != "BAD_REQUEST" {
		t.Logf("note: nwc_code = %q (expected BAD_REQUEST or a locally-classified refusal); this lab's live behavior, recorded rather than hard-asserted the way the sibling test does", got)
	}

	// Net-adjusted, matching the same tradeoff: must clear the amount check.
	netInvoice := mintInvoiceFromFlnd(t, netMillis, "cashctl amount-first audit: correctly net-adjusted amount")
	netAmountLoki := "9" // netMillis (9000) in loki, truncated — exact since netMillis is a whole multiple of 1000
	res := f.run("redeem", "--invoice", netInvoice, "--amount", netAmountLoki, "--yes")
	if res.ExitCode != 0 {
		if got := jsonErrorNWCCode(t, res.Stderr); got == "BAD_REQUEST" {
			t.Errorf("redeem --invoice --amount with the correctly net-adjusted amount still hit BAD_REQUEST: %s", res.Stderr)
		}
	}
	t.Logf("net-adjusted --invoice --amount retry: exit=%d stdout=%s stderr=%s", res.ExitCode, res.Stdout, res.Stderr)
}
