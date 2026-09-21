//go:build integration

// cash_redeem_fee_test.go covers the redeem-fee-vs-invoice-amount bug found
// while auditing cmd/cash_redeem.go: runCashRedeem's auto-generated
// destination invoice always requested the token's full face amount,
// regardless of a nonzero RedeemFeePpm on the slice being redeemed. That's
// only correct for a redemption that resolves to a same-node payment
// (transactions.IsSelfPayment, lokihub's own
// nip47/controllers/cash_redeem_controller.go step 9) — a genuinely
// external one gets rejected outright (ERROR_BAD_REQUEST), because the
// invoice amount doesn't leave room for the Hub's own cut.
//
// Every existing cashctl integration test redeems into an invoice minted
// via the SAME lokihub instance's own make_invoice, which always leaves a
// tracked incoming db.Transaction row — always a same-node redemption, so
// this exact path was never exercised (see docs/private/
// audit-round2-fee-invoice-amount.md for the full writeup). The tests below
// arrange a genuine non-self-payment despite this being a single-node lab:
// mintInvoiceFromFlnd mints the destination invoice directly on the
// underlying flnd node, bypassing lokihub's own app-scoped make_invoice
// tracking entirely — the invoice's payee pubkey still matches lokihub's
// own LN node (same physical node), but no tracked incoming transaction row
// exists for its payment hash, so IsSelfPayment resolves false. lokihub's
// own nip47/controllers/cash_redeem_fee_test.go and
// integration/cash_redeem_fee_test.go document the identical
// self-payment-exemption gap in their own (multi-hub-but-still-one-node)
// harness — this technique is this suite's way past it.
package integration

import (
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nip47"
)

// flndContainerName/flndAdminMacaroonPath/flndTLSCertPath name this lab's
// own docker-compose topology (lokihub-dev-flnd, see docker-compose.dev.yml
// in the lokihub repo) — mintInvoiceFromFlnd's technique is inherently tied
// to it, not a portable requirement of this suite, hence the clean skip
// below rather than a hard failure when it's unreachable.
const (
	flndContainerName     = "lokihub-dev-flnd"
	flndAdminMacaroonPath = "/root/.flnd/data/chain/flokicoin/main/admin.macaroon"
	flndTLSCertPath       = "/root/.flnd/tls.cert"
)

// jsonErrorNWCCode parses stderr as cashctl's --json error shape and
// returns its nwc_code field — the raw NIP-47 error code a wallet returned
// (see AGENTS.md's own error table), which is all cashctl's own NWCError
// preserves verbatim; the human-readable "error" text is always one of a
// fixed set of canned, per-code messages (internal/output/nwc_errors.go's
// nwcErrorMessages), never the wallet's own raw message, so asserting on
// nwc_code is the only reliable way to distinguish "rejected because the
// amount didn't match" from any other BAD_REQUEST reason from outside
// lokihub itself.
func jsonErrorNWCCode(t *testing.T, stderr string) string {
	t.Helper()
	var body struct {
		NWCCode string `json:"nwc_code"`
	}
	if err := json.Unmarshal([]byte(stderr), &body); err != nil {
		t.Errorf("decode stderr JSON: %v: %s", err, stderr)
		return ""
	}
	return body.NWCCode
}

// mintInvoiceFromFlnd mints an invoice for amountMloki directly against the
// lab's underlying LN node, via flncli (docker exec) rather than through
// lokihub's own make_invoke NWC method — see this file's own package doc
// comment for why that specifically produces a genuine non-self-payment
// invoice despite the lab having only one physical LN node. Skips the
// calling test cleanly (not a failure) if the container/binary/macaroon
// aren't reachable exactly as this lab has them laid out.
func mintInvoiceFromFlnd(t *testing.T, amountMloki uint64, memo string) string {
	t.Helper()
	out, err := exec.Command("docker", "exec", flndContainerName,
		"flncli",
		"--macaroonpath="+flndAdminMacaroonPath,
		"--tlscertpath="+flndTLSCertPath,
		"addinvoice",
		"--amt_msat", strconv.FormatUint(amountMloki, 10),
		"--memo", memo,
	).CombinedOutput()
	if err != nil {
		t.Skipf("skipping: couldn't mint an invoice directly on the lab's flnd node (%v) — this test's non-self-payment technique is tied to this specific lab's docker-compose topology (see this file's own doc comment): %s", err, out)
	}
	var resp struct {
		PaymentRequest string `json:"payment_request"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("decode flncli addinvoice output: %v: %s", err, out)
	}
	if resp.PaymentRequest == "" {
		t.Fatalf("flncli addinvoice: empty payment_request: %s", out)
	}
	return resp.PaymentRequest
}

// TestCashRedeemFee_ExternalRedemption_FullAmountInvoiceRejected reproduces
// the bug live: an invoice for the token's full face amount (what
// runCashRedeem always requested before the fix, and what --invoice lets a
// caller still present manually) is rejected by a real lokihub instance the
// moment the redemption is a genuinely external one and the slice carries a
// nonzero redeem fee. A correctly-quoted retry, for the net (fee-reduced)
// amount, then gets past that same check.
func TestCashRedeemFee_ExternalRedemption_FullAmountInvoiceRejected(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	const redeemFeePpm = 100_000 // 10%
	const amountMillis = uint64(10_000)
	const feeMillis = amountMillis * redeemFeePpm / 1_000_000 // 1,000
	const netMillis = amountMillis - feeMillis                // 9,000

	hub := setUpCashHubOpts(t, admin, cashHubOpts{RedeemFeePpm: redeemFeePpm, MaxExpSecs: 3600})

	f := newFixture(t)
	initResp := f.mustJSON("wallet", "init")
	npub, _ := initResp["npub"].(string)
	pubHex, err := npubToHex(npub)
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	token := mintPubkeyTokenFromHub(t, hub, pubHex, amountMillis)
	f.mustJSON("receive", token)

	// Simulates the pre-fix behavior: an invoice for the slice's FULL face
	// amount, ignoring its nonzero RedeemFeePpm — exactly what
	// runCashRedeem's own destClient.MakeInvoice call asked for
	// unconditionally before this fix, whenever the destination invoice
	// turned out to resolve to a genuinely external payment.
	fullInvoice := mintInvoiceFromFlnd(t, amountMillis, "cashctl audit: buggy full-amount request")

	res := f.run("redeem", "--invoice", fullInvoice, "--yes")
	if res.ExitCode != 3 {
		t.Fatalf("redeem with a full-amount invoice against a fee-charging external redemption: exit = %d, want 3 (invalid_input)\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if got := jsonErrorNWCCode(t, res.Stderr); got != "BAD_REQUEST" {
		t.Errorf("nwc_code = %q, want BAD_REQUEST (cash_redeem_controller.go step 9's exact-amount rejection): %s", got, res.Stderr)
	}

	// A rejected attempt must not burn the slice — cash_redeem_
	// controller.go unclaims it server-side on a mismatch (step 9), and
	// runCashRedeem never marks the local entry redeemed before a
	// successful wire call. Confirm the token is still held locally
	// before retrying.
	showResp := f.mustJSON("wallet", "show")
	held, _ := showResp["held_tokens"].([]any)
	if len(held) != 1 {
		t.Fatalf("token must still be held locally after a rejected redeem attempt: %v", showResp)
	}

	// The correctly-quoted retry — what this fix now makes cashctl itself
	// request automatically (redeemInvoiceAmount) — must get past the
	// amount check: no longer BAD_REQUEST, regardless of what (if
	// anything) it fails with next. Whether the underlying Lightning
	// payment itself then succeeds is a separate question this
	// single-node lab genuinely can't always answer (a real non-self-
	// payment attempt has to actually route out and back over the node's
	// real channels) — see this test's own package doc comment and
	// docs/private/audit-round2-fee-invoice-amount.md, which records what
	// was actually observed live in this lab (an INTERNAL decline once
	// the real payment attempt is dispatched — this lab's flnd node has
	// no cycle back to itself, so SendPaymentSync can't complete a
	// genuine self-payment; a second LN node would be needed to observe
	// this succeed for real). t.Logf below records whatever this run
	// actually saw, rather than asserting a specific outcome past the
	// amount check.
	netInvoice := mintInvoiceFromFlnd(t, netMillis, "cashctl audit: correctly-quoted net amount")
	retryRes := f.run("redeem", "--invoice", netInvoice, "--yes")
	if retryRes.ExitCode != 0 {
		if got := jsonErrorNWCCode(t, retryRes.Stderr); got == "BAD_REQUEST" {
			t.Errorf("redeem with the correctly-quoted net amount still hit BAD_REQUEST — the fix did not take: %s", retryRes.Stderr)
		}
	}
	t.Logf("retry with the correctly-quoted net amount (%d millis): exit=%d stdout=%s stderr=%s", netMillis, retryRes.ExitCode, retryRes.Stdout, retryRes.Stderr)
}

// TestCashRedeemFee_SameNodeAutoInvoice_NowRejectedWithFeeConfigured proves
// the fix's own accepted tradeoff live, against a real lokihub instance:
// cashctl's auto-invoice redeem path now always asks for the net (fee-
// reduced) amount whenever the slice carries a redeem fee, so a redemption
// that happens to resolve same-node — the ONLY case cashctl's redeem
// command ever actually exercised for real before this fix (every prior
// test redeems into an invoice minted from the same hub) — now fails
// outright instead of quietly succeeding fee-free. See cmd/cash_redeem.go's
// redeemInvoiceAmount doc comment and docs/private/
// audit-round2-fee-invoice-amount.md for why this was judged the right
// tradeoff: a genuinely external redemption is far more representative of
// real usage (cashing a token out to a wallet the recipient actually
// controls elsewhere) than a same-Hub coincidence.
func TestCashRedeemFee_SameNodeAutoInvoice_NowRejectedWithFeeConfigured(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	const redeemFeePpm = 100_000 // 10%
	const amountMillis = uint64(10_000)

	hub := setUpCashHubOpts(t, admin, cashHubOpts{RedeemFeePpm: redeemFeePpm, MaxExpSecs: 3600})

	f := newFixture(t)
	initResp := f.mustJSON("wallet", "init")
	npub, _ := initResp["npub"].(string)
	pubHex, err := npubToHex(npub)
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	token := mintPubkeyTokenFromHub(t, hub, pubHex, amountMillis)
	f.mustJSON("receive", token)

	// --into <hub.PairingUri>: cashctl's own auto-invoice path mints the
	// destination invoice from THIS SAME hub's own wallet connection
	// (granted make_invoke/pay_invoice/get_balance by setUpCashHubOpts
	// precisely so it can double as a redeem destination) — a genuine
	// same-node redemption, since lokihub's own make_invoice always leaves
	// a tracked incoming transaction row satisfying IsSelfPayment's other
	// half.
	res := f.run("redeem", "--into", hub.PairingUri, "--yes")
	if res.ExitCode != 3 {
		t.Fatalf("redeem (auto-invoice, same-node, fee configured): exit = %d, want 3 (invalid_input)\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if got := jsonErrorNWCCode(t, res.Stderr); got != "BAD_REQUEST" {
		t.Errorf("nwc_code = %q, want BAD_REQUEST (lokihub's own same-node-specific amount-mismatch rejection): %s", got, res.Stderr)
	}
}

// --- Round 3: RedeemFeePpm boundary/extreme values ---------------------
//
// Round 2 only exercised moderate fees (2-10%). redeemInvoiceAmount itself
// (cmd/cash_redeem.go) does no ppm arithmetic at all — it just returns
// whatever NetRedeemableMillis the Hub already quoted — so cashctl's own
// correctness at any given ppm value never actually depends on the value
// itself. What's worth checking at the extremes is everything AROUND that:
// does a 0-vs-omitted distinction even reach the wire, does the smallest
// possible nonzero fee round the same way a large one does, does the
// 100%-fee edge (NetRedeemableMillis == 0) produce a sane invoice request
// and a sane confirmation message instead of something nonsensical, and is
// an out-of-range (>100%) ppm value even reachable given lokihub's own
// server-side validation.

// recipientRow returns the list-recipients row matching amountMillis (the
// single recipient every test below mints) — the live source of truth for
// a slice's own redeem_fee_millis/net_redeemable_millis quote, independent
// of anything cashctl's own redeem path computes.
func recipientRow(t *testing.T, f *fixture, amountMillis uint64) map[string]any {
	t.Helper()
	resp := f.mustJSON("cash", "list-recipients")
	recipients, _ := resp["recipients"].([]any)
	for _, r := range recipients {
		row, _ := r.(map[string]any)
		if amt, _ := row["amount_millis"].(float64); uint64(amt) == amountMillis {
			return row
		}
	}
	t.Fatalf("list-recipients: no row with amount_millis=%d: %v", amountMillis, resp)
	return nil
}

// TestCashRedeemFee_ZeroPpmExplicitMatchesOmitted confirms an explicit
// RedeemFeePpm: 0 behaves identically to leaving it unset (setUpCashHub's
// own default). This is actually guaranteed at the type level before any
// of cashctl's own code runs at all: admin_client.go's own
// CashRedeemFeePpm field carries `json:"cashRedeemFeePpm,omitempty"`, so a
// Go zero value and an explicit 0 serialize to the exact same wire
// request (the field is omitted either way) — there is no way for this
// suite's own admin API client to send "explicitly zero" as distinct from
// "not set." This test exercises the explicit-0 path end to end anyway
// (rather than relying on that as a paper argument) — a same-node
// auto-invoice redemption must succeed cleanly, the same as every
// pre-existing zero-fee test, with a zero fee actually reported back.
func TestCashRedeemFee_ZeroPpmExplicitMatchesOmitted(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	const amountMillis = uint64(10_000)
	hub := setUpCashHubOpts(t, admin, cashHubOpts{RedeemFeePpm: 0, MaxExpSecs: 3600})

	f := newFixture(t)
	initResp := f.mustJSON("wallet", "init")
	npub, _ := initResp["npub"].(string)
	pubHex, err := npubToHex(npub)
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	token := mintPubkeyTokenFromHub(t, hub, pubHex, amountMillis)
	f.mustJSON("receive", token)

	row := recipientRow(t, f, amountMillis)
	if fee, _ := row["redeem_fee_millis"].(float64); fee != 0 {
		t.Errorf("redeem_fee_millis = %v, want 0 for an explicit RedeemFeePpm: 0 hub", row["redeem_fee_millis"])
	}
	if net, _ := row["net_redeemable_millis"].(float64); uint64(net) != amountMillis {
		t.Errorf("net_redeemable_millis = %v, want %d (== amount_millis, no fee)", row["net_redeemable_millis"], amountMillis)
	}

	redeemResp := f.mustJSON("redeem", "--into", hub.PairingUri, "--yes")
	if preimage, _ := redeemResp["preimage"].(string); preimage != "" {
		// preimage isn't actually part of redeem's own --json shape (see
		// runCashRedeem) — kept as a loose sanity check, not a hard
		// assertion, in case that ever changes.
		_ = preimage
	}
	if feeMloki, _ := redeemResp["fee_mloki"].(float64); feeMloki != 0 {
		t.Errorf("redeem --json fee_mloki = %v, want 0 for an explicit-zero-fee hub", redeemResp["fee_mloki"])
	}
}

// TestCashRedeemFee_SmallestNonzeroPpm_RoundingEdge exercises RedeemFeePpm:
// 1 (0.0001%) — the smallest possible nonzero rate, and the tightest
// rounding case CalculateFeeSkimMloki's floor-division can produce
// (lokihub's transactions_service.go). amountMillis is chosen as exactly
// 1_000_000 so 1 ppm of it floors to exactly 1 millis of fee, not 0 (a
// smaller amount would round the fee away to nothing and this test
// wouldn't be exercising a nonzero-fee path at all). Proves the same-node-
// gets-rejected-once-any-fee-applies behavior (already proven at 10% in
// TestCashRedeemFee_SameNodeAutoInvoice_NowRejectedWithFeeConfigured) holds
// just as cleanly at the smallest possible fee — cashctl's own
// redeemInvoiceAmount has no fee-size-dependent branching to get wrong
// here, but the point is confirming that's actually true end to end, not
// just by reading the one-line function body.
func TestCashRedeemFee_SmallestNonzeroPpm_RoundingEdge(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	const redeemFeePpm = 1
	const amountMillis = uint64(1_000_000)
	const wantFeeMillis = uint64(1) // floor(1_000_000 * 1 / 1_000_000)
	const wantNetMillis = amountMillis - wantFeeMillis

	hub := setUpCashHubOpts(t, admin, cashHubOpts{
		RedeemFeePpm: redeemFeePpm,
		MaxExpSecs:   3600,
		// This test's amount (1,000,000 millis = 1,000 loki) exceeds
		// setUpCashHubOpts's own 10,000,000-mloki default ceiling headroom
		// otherwise reserved for other scenarios' funding — explicit here
		// only so a future default change elsewhere can't silently shrink
		// this test's own amount below the hub's per-wallet cap.
		PerWalletMaxMloki: 10_000_000,
	})

	f := newFixture(t)
	initResp := f.mustJSON("wallet", "init")
	npub, _ := initResp["npub"].(string)
	pubHex, err := npubToHex(npub)
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	token := mintPubkeyTokenFromHub(t, hub, pubHex, amountMillis)
	f.mustJSON("receive", token)

	row := recipientRow(t, f, amountMillis)
	if fee, _ := row["redeem_fee_millis"].(float64); uint64(fee) != wantFeeMillis {
		t.Fatalf("redeem_fee_millis = %v, want %d (1 ppm of %d, floored) — if this fails, the rounding edge itself doesn't land where this test assumed, not necessarily a cashctl bug", row["redeem_fee_millis"], wantFeeMillis, amountMillis)
	}
	if net, _ := row["net_redeemable_millis"].(float64); uint64(net) != wantNetMillis {
		t.Errorf("net_redeemable_millis = %v, want %d", row["net_redeemable_millis"], wantNetMillis)
	}

	// Same-node auto-invoice: cashctl's redeemInvoiceAmount always requests
	// the net amount once any fee applies, so this must be rejected exactly
	// like the 10%-fee case — the smallest possible fee still triggers the
	// same either/or, no special-cased "fee too small to bother" carve-out.
	res := f.run("redeem", "--into", hub.PairingUri, "--yes")
	if res.ExitCode != 3 {
		t.Fatalf("redeem (auto-invoice, same-node, 1 ppm fee configured): exit = %d, want 3 (invalid_input)\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if got := jsonErrorNWCCode(t, res.Stderr); got != "BAD_REQUEST" {
		t.Errorf("nwc_code = %q, want BAD_REQUEST: %s", got, res.Stderr)
	}

	// External redemption, correctly-quoted net amount: mirrors
	// TestCashRedeemFee_ExternalRedemption_FullAmountInvoiceRejected's own
	// full/net pair, at the 1 ppm edge instead of 10%.
	fullInvoice := mintInvoiceFromFlnd(t, amountMillis, "cashctl audit round3: 1ppm full-amount (buggy) request")
	fullRes := f.run("redeem", "--invoice", fullInvoice, "--yes")
	if fullRes.ExitCode != 3 {
		t.Fatalf("redeem with the full-amount invoice against a 1ppm-fee external redemption: exit = %d, want 3\nstdout: %s\nstderr: %s", fullRes.ExitCode, fullRes.Stdout, fullRes.Stderr)
	}
	if got := jsonErrorNWCCode(t, fullRes.Stderr); got != "BAD_REQUEST" {
		t.Errorf("nwc_code = %q, want BAD_REQUEST for the full-amount invoice at 1ppm: %s", got, fullRes.Stderr)
	}

	netInvoice := mintInvoiceFromFlnd(t, wantNetMillis, "cashctl audit round3: 1ppm correctly-quoted net amount")
	netRes := f.run("redeem", "--invoice", netInvoice, "--yes")
	if netRes.ExitCode != 0 {
		if got := jsonErrorNWCCode(t, netRes.Stderr); got == "BAD_REQUEST" {
			t.Errorf("redeem with the correctly-quoted net amount at 1ppm still hit BAD_REQUEST: %s", netRes.Stderr)
		}
	}
	t.Logf("1ppm external retry with net amount (%d millis): exit=%d stdout=%s stderr=%s", wantNetMillis, netRes.ExitCode, netRes.Stdout, netRes.Stderr)
}

// TestCashRedeemFee_MakeInvoiceAcceptsZeroAmount isolates one specific
// question from the 100%-fee scenario below: does lokihub's own
// make_invoice even accept Amount: 0 at all, independent of anything
// cash_redeem or cashctl's own redeem command does with the result?
// Dials the hub directly as a plain NWC client (bypassing cashctl
// entirely) so a failure here can't be confused with anything downstream.
// Neither make_invoice_controller.go nor transactions_service.go's own
// MakeInvoice (lokihub source, read directly for this audit) reject a
// zero amount anywhere — an amount-less/"any amount" BOLT11 invoice is
// valid by spec, so the expectation going in is that this succeeds.
func TestCashRedeemFee_MakeInvoiceAcceptsZeroAmount(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	hub := setUpCashHub(t, admin)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	nwc := dialNWC(t, ctx, hub.PairingUri)

	tx, err := nwc.MakeInvoice(ctx, nip47.MakeInvoiceParams{Amount: 0, Description: "cashctl audit round3: zero-amount invoice"})
	if err != nil {
		t.Fatalf("make_invoice with Amount: 0 against a real NWC wallet: %v — if this is now a hard rejection, cmd/cash_redeem.go's 100%%-fee auto-invoice path (redeemInvoiceAmount returning 0) needs its own explicit handling instead of relying on this succeeding", err)
	}
	if tx.Invoice == "" {
		t.Fatalf("make_invoice with Amount: 0 returned no invoice: %+v", tx)
	}
	t.Logf("make_invoice(Amount: 0) succeeded: invoice=%s", tx.Invoice)
}

// TestCashRedeemFee_HundredPercentPpm_ZeroNetRedeemable is the full,
// end-to-end 100%-fee edge case: RedeemFeePpm: 1_000_000 makes
// NetRedeemableMillis exactly 0 for every slice this hub mints. Checks,
// against a real lokihub instance:
//   - the confirmation message reads sensibly ("you receive 0 loki",
//     not something nonsensical) — captured via a real interactive prompt
//     (f.runInteractive), since f.run always passes --json, which skips
//     the confirmation text entirely (Confirm never prints under
//     --json/--yes);
//   - cashctl's own auto-invoice path does successfully get an invoice
//     back from make_invoice for Amount: 0 (proven in isolation by
//     TestCashRedeemFee_MakeInvoiceAcceptsZeroAmount above; here it's
//     implied by reaching cash_redeem at all rather than failing inside
//     DialGeneric/MakeInvoice's own classifyNWCErr path first);
//   - whatever cash_redeem itself does with a zero-amount invoice and a
//     100%-fee slice is cleanly classified, not a confusing raw/internal-
//     looking failure.
func TestCashRedeemFee_HundredPercentPpm_ZeroNetRedeemable(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	const redeemFeePpm = 1_000_000 // 100%
	const amountMillis = uint64(10_000)

	hub := setUpCashHubOpts(t, admin, cashHubOpts{RedeemFeePpm: redeemFeePpm, MaxExpSecs: 3600})

	f := newFixture(t)
	initResp := f.mustJSON("wallet", "init")
	npub, _ := initResp["npub"].(string)
	pubHex, err := npubToHex(npub)
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	token := mintPubkeyTokenFromHub(t, hub, pubHex, amountMillis)
	f.mustJSON("receive", token)

	row := recipientRow(t, f, amountMillis)
	if net, _ := row["net_redeemable_millis"].(float64); net != 0 {
		t.Fatalf("net_redeemable_millis = %v, want 0 for a 100%% RedeemFeePpm hub", row["net_redeemable_millis"])
	}
	if fee, _ := row["redeem_fee_millis"].(float64); uint64(fee) != amountMillis {
		t.Errorf("redeem_fee_millis = %v, want %d (the full amount, at 100%%)", row["redeem_fee_millis"], amountMillis)
	}

	// Capture the real confirmation prompt text — f.run always passes
	// --json, which would skip it. "y\n" accepts, so this also exercises
	// the full auto-invoice -> cash_redeem round trip past the prompt.
	interactive := f.runInteractive("y\n", "redeem", "--into", hub.PairingUri)
	if !strings.Contains(interactive.Combined(), "you receive 0") {
		t.Errorf("confirmation text doesn't mention receiving 0 %s sensibly:\n%s", "loki", interactive.Combined())
	}
	if strings.Contains(interactive.Combined(), "you receive 0") && !strings.Contains(interactive.Combined(), "you receive 0 loki") {
		t.Errorf("confirmation text's units look malformed around the 0 amount:\n%s", interactive.Combined())
	}
	t.Logf("100%% fee interactive confirmation text:\n%s", interactive.Combined())

	// Now the classified (--json) outcome. This redemption is same-node
	// (--into the same hub that minted the token, via cashctl's own
	// make_invoice) — expectedAmount server-side is the FULL amount for a
	// same-node payment (fee waived), but cashctl always requests the net
	// (here: 0) amount once any fee applies (redeemInvoiceAmount), so this
	// is expected to hit the same BAD_REQUEST amount-mismatch this suite's
	// other same-node-with-fee tests already prove at 1ppm/10% — the 100%
	// case doesn't get a free pass just because the requested amount
	// happens to be the round number 0. Whatever the real outcome, assert
	// it's cleanly classified (not exit 1/an unclassified error) rather
	// than asserting one specific code, since a real NWC wallet's exact
	// handling of a zero-amount invoice on the PAYING side (a separate
	// question from make_invoice accepting it on the RECEIVING side) isn't
	// fully pinned down by reading source alone.
	res := f.run("redeem", "--into", hub.PairingUri, "--yes")
	t.Logf("100%% fee auto-invoice redeem: exit=%d stdout=%s stderr=%s", res.ExitCode, res.Stdout, res.Stderr)
	if res.ExitCode == 0 {
		// Got past every check and actually completed — only possible if
		// this real wallet treats "pay this amount-less invoice with an
		// implied amount of 0" as a valid, zero-value same-node payment.
		// Not the outcome predicted above, but not a cashctl bug either
		// way: nothing about it would leave a user confused (0 fee, 0
		// received is at least internally consistent), so just record it.
		t.Logf("100%% fee redemption completed (exit 0) rather than being rejected — see this test's own comment")
		return
	}
	if res.ExitCode != 3 {
		t.Errorf("100%% fee auto-invoice redeem: exit = %d, want 3 (invalid_input/classified) or 0 (completed) — an unclassified failure here is the actual bug this test is checking for\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	var errBody struct {
		Code    string `json:"code"`
		NWCCode string `json:"nwc_code"`
		Error   string `json:"error"`
	}
	if jsonErr := json.Unmarshal([]byte(res.Stderr), &errBody); jsonErr != nil {
		t.Fatalf("100%% fee auto-invoice redeem: stderr isn't valid --json error shape (a confusing/raw failure, exactly what this test guards against): %v: %s", jsonErr, res.Stderr)
	}
	if errBody.Error == "" || errBody.Code == "" {
		t.Errorf("100%% fee auto-invoice redeem: error body missing code/error text — not a sensible classified failure: %+v", errBody)
	}
	t.Logf("100%% fee auto-invoice redeem classified error: code=%s nwc_code=%s error=%s", errBody.Code, errBody.NWCCode, errBody.Error)
}

// TestCashRedeemFee_OutOfRangePpm_RejectedAtHubCreation tries an
// out-of-range (>100%) RedeemFeePpm directly against the admin API's own
// createApp call (bypassing setUpCashHubOpts, which would t.Fatal on the
// very error this test expects). lokihub's apps/cash_hub_service.go
// validates 0 <= RedeemFeePpm <= constants.MAX_FEES_PPM (1_000_000) at
// hub-creation time (and again at update time) — confirmed by reading
// that source directly. If this DID somehow succeed, cashctl would be
// exposed to list_recipients_controller.go's own
// `AmountMillis - redeemFeeMloki` subtraction (uint64, no saturation)
// producing more fee than the slice is worth, which — since neither value
// is ever negative in Go's uint64 — would underflow into a huge
// NetRedeemableMillis rather than erroring, and cashctl's own
// redeemInvoiceAmount would then blindly request an invoice for that
// enormous (wrapped) amount. This test confirms that path is unreachable
// in practice: the guard is server-side, so there's nothing to fix here,
// but the underflow-shaped risk is exactly why this needed checking live
// rather than assumed from a doc comment.
func TestCashRedeemFee_OutOfRangePpm_RejectedAtHubCreation(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	const outOfRangePpm = 1_500_000 // 150% — constants.MAX_FEES_PPM is 1_000_000
	resp, err := admin.createApp(adminCreateAppRequest{
		Name:                  ephemeralFixtureNamePrefix + " cash_hub out-of-range ppm",
		Kind:                  "cash_hub",
		Scopes:                []string{"cash_hub", "pay_invoice", "make_invoice", "get_balance"},
		CashPerWalletMaxMloki: 10_000_000,
		CashMaxExpSecs:        3600,
		CashRedeemFeePpm:      outOfRangePpm,
	})
	if err == nil {
		// Unexpected given lokihub's own source — clean up what got
		// created (setUpCashHubOpts's own cleanup wiring never ran, since
		// this bypassed it deliberately) and check for the underflow this
		// test's own doc comment describes, rather than just failing
		// blind.
		t.Cleanup(func() {
			if delErr := admin.deleteApp(resp.ID); delErr != nil {
				t.Logf("cleanup: delete out-of-range-ppm cash_hub app_id=%d: %v", resp.ID, delErr)
			}
		})
		t.Errorf("createApp with RedeemFeePpm: %d (>100%%) succeeded — expected lokihub's own apps/cash_hub_service.go validation to reject it; checking for a downstream uint64 underflow instead of stopping here", outOfRangePpm)

		f := newFixture(t)
		initResp := f.mustJSON("wallet", "init")
		npub, _ := initResp["npub"].(string)
		pubHex, decErr := npubToHex(npub)
		if decErr != nil {
			t.Fatalf("decode local identity npub: %v", decErr)
		}
		const amountMillis = uint64(10_000)
		token := mintPubkeyTokenFromHub(t, resp, pubHex, amountMillis)
		f.mustJSON("receive", token)
		row := recipientRow(t, f, amountMillis)
		net, _ := row["net_redeemable_millis"].(float64)
		if uint64(net) > amountMillis {
			t.Errorf("BUG: net_redeemable_millis = %v > amount_millis = %d — uint64 underflow from an out-of-range RedeemFeePpm, exactly as this test's doc comment predicted; cashctl's redeemInvoiceAmount would request an invoice for this huge wrapped amount", row["net_redeemable_millis"], amountMillis)
		}
		return
	}
	if !strings.Contains(err.Error(), "redeem_fee_ppm") {
		t.Logf("createApp with an out-of-range RedeemFeePpm was rejected, but not with the exact message expected (still a pass — any rejection confirms the guard exists): %v", err)
	} else {
		t.Logf("createApp with RedeemFeePpm: %d correctly rejected: %v", outOfRangePpm, err)
	}
}
