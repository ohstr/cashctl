//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nip47"
)

// This file exercises real interactive (text-mode, no --json)
// confirmation prompts via fixture.runInteractive — every other test in
// this suite passes --json, which skips them.

// TestCashRedeem_BareEnterDeclinesByDefault: redeem's confirmation must
// default to NO on a bare Enter — never accept a send/spend passively.
func TestCashRedeem_BareEnterDeclinesByDefault(t *testing.T) {
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

	hub := setUpCashHub(t, admin)
	token := mintPubkeyTokenFromHub(t, hub, myPubHex, 20_000)
	if res := f.run("receive", token); res.ExitCode != 0 {
		t.Fatalf("receive: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if res := f.run("connect", "add", "hub", hub.PairingUri); res.ExitCode != 0 {
		t.Fatalf("connect add: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	// A bare Enter: empty stdin line, no "y"/"yes" typed.
	res := f.runInteractive("\n", "redeem", "hub")
	if res.ExitCode != 0 {
		t.Fatalf("redeem (bare Enter): unexpected exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "Cancelled") {
		t.Fatalf("redeem (bare Enter): expected \"Cancelled\", got stdout: %q", res.Stdout)
	}
	if !strings.Contains(res.Combined(), "[y/N]") {
		t.Errorf("redeem confirmation prompt = %q, want it to show [y/N] (default no)", res.Combined())
	}

	if n := heldCount(t, f); n != 1 {
		t.Fatalf("a declined redeem must leave the token held, got %d held", n)
	}
}

// TestCashRedeem_ExplicitYAcceptsDespiteDefaultNo confirms the flip to
// default-no didn't just make redeem impossible interactively — typing
// "y" explicitly must still redeem for real.
func TestCashRedeem_ExplicitYAcceptsDespiteDefaultNo(t *testing.T) {
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

	hub := setUpCashHub(t, admin)
	token := mintPubkeyTokenFromHub(t, hub, myPubHex, 15_000)
	if res := f.run("receive", token); res.ExitCode != 0 {
		t.Fatalf("receive: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if res := f.run("connect", "add", "hub", hub.PairingUri); res.ExitCode != 0 {
		t.Fatalf("connect add: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	res := f.runInteractive("y\n", "redeem", "hub")
	if res.ExitCode != 0 {
		t.Fatalf("redeem (explicit y): exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "Redeemed") {
		t.Errorf("redeem (explicit y): expected a success message, got stdout: %q", res.Stdout)
	}
	if n := heldCount(t, f); n != 0 {
		t.Errorf("an accepted redeem must consume the held token, got %d still held", n)
	}
}

// TestCashRedeem_PreviewShowsExpiryWarning confirms redeem's confirmation
// shows an expiry warning up front, not only after redeeming.
// setUpCashHub's CashMaxExpSecs (1h) is within the "soon" threshold.
func TestCashRedeem_PreviewShowsExpiryWarning(t *testing.T) {
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

	hub := setUpCashHub(t, admin)
	const amountMillis = uint64(30_000)
	token := mintPubkeyTokenFromHub(t, hub, myPubHex, amountMillis)
	if res := f.run("receive", token); res.ExitCode != 0 {
		t.Fatalf("receive: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if res := f.run("connect", "add", "hub", hub.PairingUri); res.ExitCode != 0 {
		t.Fatalf("connect add: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	// Decline (bare Enter) — this test only cares about what the prompt
	// itself said, not about actually redeeming.
	res := f.runInteractive("\n", "redeem", "hub")
	if res.ExitCode != 0 {
		t.Fatalf("redeem (preview check): unexpected exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Combined(), "Expires in") && !strings.Contains(res.Combined(), "Deadline passed") {
		t.Errorf("redeem confirmation: expected an expiry warning (this hub's own CashMaxExpSecs is 1h), got stdout: %q", res.Combined())
	}
	if !strings.Contains(res.Combined(), "30 loki") {
		t.Errorf("redeem confirmation: expected the token's own amount (30000 mloki = 30 loki) shown before confirming, got stdout: %q", res.Combined())
	}

	if n := heldCount(t, f); n != 1 {
		t.Fatalf("a declined redeem must leave the token held, got %d held", n)
	}
}

// setUpFundedCircleWallet joins t's fixture into a fresh circle_hub with a
// generous cap, then actually funds the resulting member wallet's own
// sub-balance — funding the parent hub alone isn't enough (circle members
// have independent sub-balances; see
// TestCircleWallet_FeeBudgetMatchesHubGroundTruth's own doc comment) — by
// having a separately funded ephemeral wallet pay an invoice minted on it,
// mirroring circle_test.go's own funding pattern. Returns the funded
// amount so callers can assert real, live balance deltas rather than just
// trusting cashctl's own exit code.
func setUpFundedCircleWallet(t *testing.T, admin *adminClient, f *fixture, myPubHex string) int64 {
	t.Helper()
	circleHub := setUpCircleHub(t, admin, myPubHex)
	if circleHub.CircleHubToken == nil || *circleHub.CircleHubToken == "" {
		t.Fatalf("create ephemeral circle_hub: no circleHubToken in response: %+v", circleHub)
	}
	// A generous cap: these tests are about the confirmation gate, not the
	// budget one — a low cap declining a payment too would muddy which
	// gate actually blocked it.
	if res := f.run("join", "--hub", *circleHub.CircleHubToken, "--max-amount", "50", "--yes"); res.ExitCode != 0 {
		t.Fatalf("join: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	const fundAmountMloki = 20_000
	invoiceResp := f.mustJSON("wallet", "invoice", lokiArg(fundAmountMloki), "--desc", "pay confirmation test funding")
	invoiceStr, _ := invoiceResp["invoice"].(string)
	if invoiceStr == "" {
		t.Fatalf("wallet invoice: no invoice in response: %v", invoiceResp)
	}
	payer := setUpPlainWalletWithPayScope(t, admin)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	payerClient := dialNWC(t, ctx, payer.PairingUri)
	if _, err := payerClient.PayInvoice(ctx, nip47.PayInvoiceParams{Invoice: invoiceStr}); err != nil {
		t.Fatalf("payer failed to fund the joined circle wallet: %v", err)
	}
	return fundAmountMloki
}

// walletTotalMloki reads `wallet balance`'s total via --json — used by the
// pay-confirmation tests below to prove money did or didn't actually move,
// not just that cashctl printed a particular line.
func walletTotalMloki(t *testing.T, f *fixture) int64 {
	t.Helper()
	resp := f.mustJSON("wallet", "balance")
	total, ok := resp["total_mloki"].(float64)
	if !ok {
		t.Fatalf("wallet balance --json: no total_mloki in response: %v", resp)
	}
	return int64(total)
}

// TestWalletPay_BareEnterDeclinesByDefault_NoPaymentSent is the live
// evidence for the fix to `pay` moving real money with zero confirmation:
// it used to go straight from parsing the invoice's shape to actually
// paying it, so `--yes`/`--json` had nothing to skip. A bare Enter must
// now decline, and the check goes past "cashctl printed Cancelled" — the
// funded wallet's own live balance, read back independently after, proves
// the money genuinely never moved (Confirm() runs before PayInvoice is
// ever dialed, not just before printing success).
func TestWalletPay_BareEnterDeclinesByDefault_NoPaymentSent(t *testing.T) {
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
	fundedMloki := setUpFundedCircleWallet(t, admin, f, myPubHex)
	before := walletTotalMloki(t, f)
	if before < fundedMloki {
		t.Fatalf("wallet balance after funding = %d, want >= %d", before, fundedMloki)
	}

	payHub := setUpCashHub(t, admin)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	payHubNWC := dialNWC(t, ctx, payHub.PairingUri)
	invoice, err := payHubNWC.MakeInvoice(ctx, nip47.MakeInvoiceParams{Amount: 5_000})
	if err != nil {
		t.Fatalf("make_invoice: %v", err)
	}

	res := f.runInteractive("\n", "pay", invoice.Invoice)
	if res.ExitCode != 0 {
		t.Fatalf("pay (bare Enter): unexpected exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "Cancelled") {
		t.Fatalf("pay (bare Enter): expected \"Cancelled\", got stdout: %q", res.Stdout)
	}
	if !strings.Contains(res.Combined(), "5 loki") {
		t.Errorf("pay confirmation: expected the invoice's own amount (5000 mloki = 5 loki) shown before confirming, got: %q", res.Combined())
	}
	if !strings.Contains(res.Combined(), "[y/N]") {
		t.Errorf("pay confirmation prompt = %q, want it to show [y/N] (default no)", res.Combined())
	}

	if after := walletTotalMloki(t, f); after != before {
		t.Errorf("declined pay still moved money: balance before=%d after=%d", before, after)
	}
}

// TestWalletPay_ExplicitYAcceptsDespiteDefaultNo confirms the new
// confirmation gate didn't just make `pay` impossible interactively —
// typing "y" explicitly must still pay for real, verified against the
// wallet's own live balance drop, not just a zero exit code.
func TestWalletPay_ExplicitYAcceptsDespiteDefaultNo(t *testing.T) {
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
	setUpFundedCircleWallet(t, admin, f, myPubHex)
	before := walletTotalMloki(t, f)

	payHub := setUpCashHub(t, admin)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	payHubNWC := dialNWC(t, ctx, payHub.PairingUri)
	const payAmountMloki = 5_000
	invoice, err := payHubNWC.MakeInvoice(ctx, nip47.MakeInvoiceParams{Amount: payAmountMloki})
	if err != nil {
		t.Fatalf("make_invoice: %v", err)
	}

	res := f.runInteractive("y\n", "pay", invoice.Invoice)
	if res.ExitCode != 0 {
		t.Fatalf("pay (explicit y): unexpected exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}

	after := walletTotalMloki(t, f)
	if after > before-payAmountMloki {
		t.Errorf("pay (explicit y): balance before=%d after=%d, want a drop of at least %d (the invoice amount, plus any fee)", before, after, payAmountMloki)
	}
}

// TestDecode_TextModeDefaultsToNoNetworkCheck is the live evidence for the
// fix to decode silently going online despite its own documented "no
// network call by default" contract: a real, live-checkable cash token,
// decoded interactively with NEITHER --check nor an explicit answer (bare
// Enter and true EOF both), must show no "check:" line at all — the
// network round trip must never have been attempted. Typing "y" explicitly
// still runs it for real, proving the prompt itself still works, only its
// unattended default changed.
func TestDecode_TextModeDefaultsToNoNetworkCheck(t *testing.T) {
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
	token := mintPubkeyToken(t, admin, myPubHex, 3_000)

	bareEnter := f.runInteractive("\n", "decode", token)
	if strings.Contains(bareEnter.Combined(), "check:") {
		t.Errorf("decode (bare Enter, no --check): unexpectedly ran the network check: %s", bareEnter.Combined())
	}

	eof := f.runInteractive("", "decode", token)
	if strings.Contains(eof.Combined(), "check:") {
		t.Errorf("decode (EOF, no --check): unexpectedly ran the network check: %s", eof.Combined())
	}

	explicit := f.runInteractive("y\n", "decode", token)
	if !strings.Contains(explicit.Combined(), "check:") {
		t.Errorf("decode (explicit y): expected the network check to actually run: %s", explicit.Combined())
	}
}
