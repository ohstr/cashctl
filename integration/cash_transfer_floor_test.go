//go:build integration

// cash_transfer_floor_test.go exercises CashMinTransferMloki — a Hub-side
// per-wallet minimum-transfer floor wired into cashHubOpts.MinTransferMloki
// since round 2 (docs/private/audit-round2-expiration-matrix.md's sibling
// knobs) but never exercised by any test until now (round 3).
//
// Ground truth, confirmed by reading lokihub's own source before writing
// any of this (api/models.go's CashMinTransferMloki doc comment,
// nip47/controllers/cash_transfer_controller.go's handleCashTransferSplit,
// db/models.go's CashWalletClaim.MinTransferMloki): the floor is a snapshot
// copied onto a wallet's own claim row at mint time (or inherited from the
// source slice on a later split) — never a live lookup of the hub's
// current config — and is enforced ONLY on cash_transfer's split path,
// checking BOTH the carved-off split amount and the remainder it would
// leave behind (a remainder of exactly 0 — a full, nothing-left-behind
// transfer — is never checked against it). Either violation rejects with
// ERROR_BAD_REQUEST: "amount_millis %d is below this slice's
// min_transfer_millis floor of %d" for the split side, "the %d remainder
// this split would leave behind is below this slice's min_transfer_millis
// floor of %d" for the remainder side. cash_consolidate never compares an
// amount against it at all — it only requires every source being merged to
// AGREE on the same floor value (rejecting a mismatch, also
// ERROR_BAD_REQUEST). cash_redeem never references it. cashctl's own cash
// selection (internal/ledger/cashselect.go) has no concept of this floor
// whatsoever — confirmed by reading it — so cashctl never avoids a
// below-floor split proactively; it always lets the Hub reject it and
// relies on the existing generic BAD_REQUEST->invalid_input mapping
// (internal/output/nwc_errors.go) to surface that cleanly. These tests
// confirm that reliance is actually justified: cashctl neither crashes,
// hangs, nor misclassifies (e.g. as "internal") on a Hub-side floor
// rejection, for either violation shape, and gets the exact-floor boundary
// right on both sides.
package integration

import (
	"fmt"
	"testing"
)

// lokiArg formats an exact mloki amount as the loki string cashctl's
// --amount/--max-amount now expect on the CLI (internal/output.ParseAmount
// is the inverse) — up to 3 fractional digits round-trips mloki-level
// precision exactly, which this file's own floor/floor-1 boundary checks
// depend on.
func lokiArg(mloki int64) string {
	neg := mloki < 0
	if neg {
		mloki = -mloki
	}
	s := fmt.Sprintf("%d.%03d", mloki/1000, mloki%1000)
	if neg {
		s = "-" + s
	}
	return s
}

// TestCashTransferMinFloor_SplitAtFloor_Succeeds confirms a split whose
// carved-off amount equals the floor EXACTLY succeeds (lokihub's own check
// is amount < floor, not <=) and its remainder (also comfortably above the
// floor here) is unaffected.
func TestCashTransferMinFloor_SplitAtFloor_Succeeds(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	f := newFixture(t)
	initResp := f.mustJSON("wallet", "init")
	myPubHex, err := npubToHex(initResp["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	const floor = int64(50_000)
	const total = uint64(200_000)
	hub := setUpCashHubOpts(t, admin, cashHubOpts{MinTransferMloki: floor})
	token := mintPubkeyTokenFromHub(t, hub, myPubHex, total)
	if res := f.run("receive", token); res.ExitCode != 0 {
		t.Fatalf("receive: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	transferResp := f.mustJSON("transfer", fakeHex32(t), "--amount", lokiArg(floor), "--yes")
	remaining, _ := transferResp["remaining_amount_millis"].(float64)
	if wantRemainder := total - uint64(floor); uint64(remaining) != wantRemainder {
		t.Errorf("remaining_amount_millis = %v, want %d", transferResp["remaining_amount_millis"], wantRemainder)
	}
}

// TestCashTransferMinFloor_SplitBelowFloor_Rejected confirms a split
// exactly one unit below the floor is rejected — Hub-side, not silently
// cash-selected around — and that cashctl surfaces it as invalid_input
// (exit 3), matching AGENTS.md's error table for a bad-request-shaped
// wallet decline, not "internal" (a wire decline cashctl doesn't recognize
// would otherwise fall through to CodeInternal — see nwc_errors.go's own
// comment on why that fallback exists). The token must stay held: a
// rejected split is a true no-op on lokihub's own side (confirmed by its
// TestCashTransferSplit_AmountAndFloorBoundaries, which asserts the source
// claim is left completely untouched), and cashctl must not mark anything
// transferred when the wire call itself never landed.
func TestCashTransferMinFloor_SplitBelowFloor_Rejected(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	f := newFixture(t)
	initResp := f.mustJSON("wallet", "init")
	myPubHex, err := npubToHex(initResp["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	const floor = int64(50_000)
	const total = uint64(200_000)
	hub := setUpCashHubOpts(t, admin, cashHubOpts{MinTransferMloki: floor})
	token := mintPubkeyTokenFromHub(t, hub, myPubHex, total)
	if res := f.run("receive", token); res.ExitCode != 0 {
		t.Fatalf("receive: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	res := f.run("transfer", fakeHex32(t), "--amount", lokiArg(floor-1), "--yes")
	if res.ExitCode != 3 {
		t.Fatalf("split one unit below the floor: exit = %d, want 3 (invalid_input)\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	// NOT asserting the message names "floor"/the actual numbers, even
	// though lokihub's own wire message does (handleCashTransferSplit:
	// "amount_millis %d is below this slice's min_transfer_millis floor of
	// %d") — confirmed live that it never reaches here. See
	// docs/private/audit-round3-transfer-consolidate-floor.md Finding 2:
	// internal/output/nwc_errors.go's NWCError unconditionally replaces
	// EVERY BAD_REQUEST decline's message with one static "That request
	// wasn't valid.", in --json mode too, contradicting that file's own
	// doc comment ("--json mode never uses this — it preserves the raw
	// {code, message}"). A real bug, but internal/output/ is outside this
	// file's ownership this round (a concurrent agent's own files, per
	// git status) — not fixed here, only pinned as-is so this test keeps
	// passing and the gap stays visible instead of silently regressing
	// further.
	if !jsonErrorContains(t, res.Stderr, "invalid_input", "") {
		t.Errorf("unexpected error body: %s", res.Stderr)
	}
	if got := nwcCodeFromError(t, res.Stderr); got != "BAD_REQUEST" {
		t.Errorf("nwc_code = %q, want BAD_REQUEST", got)
	}
	if n := heldCount(t, f); n != 1 {
		t.Errorf("a rejected split must leave the token held, got %d held", n)
	}
}

// TestCashTransferMinFloor_RemainderBelowFloor_Rejected confirms the OTHER
// side of the floor check: a split amount that is itself comfortably above
// the floor still gets rejected if what it would LEAVE BEHIND (the
// remainder) isn't. Worth its own test, not just inferred from the
// split-side case above: lokihub enforces these as two independent
// conditions (see handleCashTransferSplit), and it would be easy for a
// naive client-side assumption ("floor only bounds what you send") to miss
// this half entirely.
func TestCashTransferMinFloor_RemainderBelowFloor_Rejected(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	f := newFixture(t)
	initResp := f.mustJSON("wallet", "init")
	myPubHex, err := npubToHex(initResp["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	const floor = int64(50_000)
	const total = uint64(200_000)
	// Carve off comfortably more than the floor, but leave a remainder of
	// exactly floor-1 behind (total - split = floor-1 => split = total -
	// floor + 1).
	splitAmount := total - uint64(floor) + 1
	hub := setUpCashHubOpts(t, admin, cashHubOpts{MinTransferMloki: floor})
	token := mintPubkeyTokenFromHub(t, hub, myPubHex, total)
	if res := f.run("receive", token); res.ExitCode != 0 {
		t.Fatalf("receive: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	res := f.run("transfer", fakeHex32(t), "--amount", lokiArg(int64(splitAmount)), "--yes")
	if res.ExitCode != 3 {
		t.Fatalf("split leaving a below-floor remainder: exit = %d, want 3 (invalid_input)\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	// Same generic-message gap as TestCashTransferMinFloor_SplitBelowFloor_
	// Rejected above (see its own comment, and docs/private/
	// audit-round3-transfer-consolidate-floor.md Finding 2): lokihub's own
	// wire message here specifically names "remainder" ("the %d remainder
	// this split would leave behind is below this slice's
	// min_transfer_millis floor of %d") — exactly the detail that would
	// let a user tell this failure apart from the split-amount-side one
	// above — but NWCError's canned BAD_REQUEST text erases that
	// distinction before it ever reaches here. Confirmed live: both this
	// test and the split-side one currently produce byte-identical error
	// bodies except for nothing at all distinguishing them. Not fixed
	// here (internal/output/ ownership, see above).
	if !jsonErrorContains(t, res.Stderr, "invalid_input", "") {
		t.Errorf("unexpected error body: %s", res.Stderr)
	}
	if got := nwcCodeFromError(t, res.Stderr); got != "BAD_REQUEST" {
		t.Errorf("nwc_code = %q, want BAD_REQUEST", got)
	}
	if n := heldCount(t, f); n != 1 {
		t.Errorf("a rejected split must leave the token held, got %d held", n)
	}
}
