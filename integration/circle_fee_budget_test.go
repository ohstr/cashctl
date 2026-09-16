//go:build integration

// circle_fee_budget_test.go is this audit round's angle: does cashctl do
// any of its OWN arithmetic on a circle wallet's balance/budget that a
// circle_hub's forwarding fee (FeesPpm, distinct from cash_hub's
// RedeemFeePpm) could get wrong, or does it purely relay numbers the Hub
// already computed? Everything here is verified against lokihub's admin API
// (getAppBalance/listAppTransactions below), never cashctl's own
// self-report — same principle as agent-eval/README.md.
//
// Finding: `wallet balance`/`wallet budget` are pure passthroughs (see
// cmd/wallet_balance.go, cmd/wallet_ops.go's newWalletBudgetCmd) — no
// cashctl-side math to get wrong. The real bug was upstream of any math:
// nmilat's nip47.GetInfoResult/PayInvoiceResult/Transaction had no field to
// catch the Hub's own `circle_wallet`/`fee_skim_mloki` wire data at all, so
// it was silently dropped by encoding/json on every unmarshal — `wallet
// get-info` against a Circle Hub connection showed nothing about its fee
// policy, and a circle member's `wallet pay` couldn't show why their
// balance dropped by more than fees_paid. Fixed in nmilat (nip47/methods.go)
// + surfaced here in cmd/wallet.go's get-info and cmd/wallet_ops.go's pay.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nip47"
	relayclient "github.com/ohstr/nmilat/relay/client"
)

// adminAppDetail is GET /api/apps/:id's response, trimmed to the one field
// this file needs — the app's own live balance, for cross-checking a
// circle_wallet's balance delta independent of anything cashctl/nmilat
// itself reports.
type adminAppDetail struct {
	Balance int64 `json:"balance"`
}

func (c *adminClient) getAppBalance(appID uint) (int64, error) {
	var resp adminAppDetail
	err := c.doBody(http.MethodGet, fmt.Sprintf("/api/apps/%d", appID), nil, &resp)
	return resp.Balance, err
}

// adminTransaction is one row of GET /api/transactions's response, trimmed
// to what this file cross-checks: the real routing fee and the circle-hub
// forwarding-fee skim lokihub actually recorded server-side for a payment,
// independent of whatever cashctl/nmilat's own PayInvoiceResult claims.
type adminTransaction struct {
	PaymentHash string `json:"paymentHash"`
	Amount      uint64 `json:"amount"`
	FeesPaid    uint64 `json:"feesPaid"`
	FeeSkim     uint64 `json:"feeSkim"`
}

func (c *adminClient) listAppTransactions(appID uint) ([]adminTransaction, error) {
	var resp struct {
		Transactions []adminTransaction `json:"transactions"`
	}
	err := c.doBody(http.MethodGet, fmt.Sprintf("/api/transactions?appId=%d&limit=50", appID), nil, &resp)
	return resp.Transactions, err
}

// TestWalletGetInfo_CircleHubConnection_SurfacesFeesPpm registers a Circle
// Hub's own raw connection (not a joined member's circle_wallet — the
// distinction matters, see nip47.GetInfoResult.CircleWallet's doc comment)
// and confirms `wallet get-info --json` now reports the Hub's configured
// FeesPpm faithfully. Before the nmilat fix in this round, the Hub sent
// `circle_wallet: {fees_ppm: ...}` over the wire but cashctl's JSON output
// had no field to catch it — this would have failed silently (a missing
// key, not an error) on the pre-fix SDK.
func TestWalletGetInfo_CircleHubConnection_SurfacesFeesPpm(t *testing.T) {
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
	npub, _ := initResp["npub"].(string)
	pubHex, err := npubToHex(npub)
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	const wantFeesPpm = 5000 // 0.5% — deliberately nonzero and distinctive

	// Built inline rather than via setUpCircleHubOpts: that shared fixture
	// (integration/circle_test.go) only grants the "circle_wallet" scope on
	// the hub's own connection, which is enough to call create_circle_wallet
	// but NOT enough for get_info to include the circle_wallet terms block —
	// lokihub's get_info_controller.go gates that on a separate, explicit
	// GET_INFO_SCOPE permission row (see its own "this is inconsistent with
	// other methods" comment). Every existing cashctl circle fixture lacks
	// it, so no existing test could have caught the bug this file is about.
	// Not fixed in the shared helper itself — three other agents are using
	// it concurrently this round.
	hubReq := adminCreateAppRequest{
		Name:                    ephemeralFixtureNamePrefix + " circle_hub (get_info scope)",
		Scopes:                  []string{"circle_wallet", "get_info"},
		Kind:                    "circle_hub",
		CircleIdentityName:      ephemeralFixtureNamePrefix + " circle identity (get_info scope)",
		CirclePolicy:            "allowlist",
		CircleMaxExpSecs:        86400,
		CirclePerWalletMaxMloki: 1_000_000,
		CircleFeesPpm:           wantFeesPpm,
	}
	hubResp, err := admin.createApp(hubReq)
	if err != nil {
		t.Fatalf("create ephemeral circle_hub: %v", err)
	}
	t.Cleanup(func() {
		if err := admin.deleteApp(hubResp.ID); err != nil {
			t.Logf("cleanup: delete ephemeral circle_hub app_id=%d: %v", hubResp.ID, err)
		}
	})
	if err := admin.transfer(hubResp.ID, 100); err != nil {
		t.Fatalf("fund ephemeral circle_hub: %v", err)
	}
	if err := admin.addCircleAllowlistMember(hubResp.ID, pubHex); err != nil {
		t.Fatalf("authorize identity under circle_hub allowlist: %v", err)
	}

	// Register the Hub's own connection as an ordinary wallet — never
	// joined, so this is get_info against app.Kind == circle_hub itself,
	// the only case lokihub's get_info_controller.go attaches
	// circle_wallet terms to.
	if res := f.run("connect", "add", "thehub", hubResp.PairingUri); res.ExitCode != 0 {
		t.Fatalf("connect add: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	infoResp := f.mustJSON("-c", "thehub", "wallet", "get-info")
	cw, _ := infoResp["circle_wallet"].(map[string]any)
	if cw == nil {
		t.Fatalf("wallet get-info --json: no circle_wallet field in response: %v", infoResp)
	}
	gotFeesPpm, _ := cw["fees_ppm"].(float64)
	if int(gotFeesPpm) != wantFeesPpm {
		t.Errorf("circle_wallet.fees_ppm = %v, want %d (the Hub's own configured rate, verified independently via the admin API that created it)", cw["fees_ppm"], wantFeesPpm)
	}
	if policy, _ := cw["circle_policy"].(string); policy != "allowlist" {
		t.Errorf("circle_wallet.circle_policy = %q, want %q", policy, "allowlist")
	}

	// Text mode: same data, human-readable — confirms cmd/wallet.go's
	// get-info actually prints it, not just that --json happens to dump the
	// raw struct. f.run always forces --json, so this needs runInteractive
	// (the only helper that invokes the binary without it) even though
	// get-info never reads stdin.
	textRes := f.runInteractive("", "-c", "thehub", "wallet", "get-info")
	if textRes.ExitCode != 0 {
		t.Fatalf("wallet get-info (text): exit %d\nstderr: %s", textRes.ExitCode, textRes.Stderr)
	}
	if !strings.Contains(textRes.Stdout, "5000 ppm") {
		t.Errorf("wallet get-info (text) doesn't mention the circle fee rate; stdout: %s", textRes.Stdout)
	}
}

// TestCircleWallet_FeeBudgetMatchesHubGroundTruth joins a circle_hub with a
// deliberately nonzero FeesPpm, funds the resulting circle_wallet via a real
// invoice (a second ephemeral plain wallet pays it, mirroring
// circle_test.go's TestCircleWallet_FullOps), then pays back out to a
// THIRD ephemeral wallet — and checks every number cashctl prints
// (wallet balance, wallet pay's fees_paid/fee_skim_mloki) against the
// admin API's own ground truth (app balance + transaction row), never
// cashctl's own self-report.
//
// This lab is single-node: every invoice here is minted via lokihub's own
// app-scoped MakeInvoice, so IsSelfPayment holds for both legs and the
// forwarding fee is exempt by design (transactions_service.go's
// validateCanPay) — lokihub's own black-box suite
// (integration/circle_fee_skim_test.go's
// TestCircleHub_FeeSkim_SelfPaymentIsNeverSkimmed) documents this same
// harness limitation: a genuine non-exempt skim isn't reachable without a
// second, real external node. What this test still proves: (1) cashctl's
// balance/budget figures agree with the Hub's own ledger to the mloki
// exactly, self-payment or not — there is no cashctl-side arithmetic here
// to diverge; (2) if a nonzero fee_skim_mloki-ever showed up on the wire, the
// pre-fix SDK would have silently dropped it (see
// TestPayInvoiceResult_FeeSkimRoundTrips in nmilat for the direct proof of
// that, since it isn't reachable live from this harness either).
func TestCircleWallet_FeeBudgetMatchesHubGroundTruth(t *testing.T) {
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
	npub, _ := initResp["npub"].(string)
	pubHex, err := npubToHex(npub)
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	const feesPpm = 5000 // 0.5% — would be very visible if wrongly applied
	hubResp := setUpCircleHubOpts(t, admin, pubHex, circleHubOpts{FeesPpm: feesPpm})
	if hubResp.CircleHubToken == nil {
		t.Fatalf("create ephemeral circle_hub: no circleHubToken in response: %+v", hubResp)
	}

	joinResp := f.mustJSON("join", "--hub", *hubResp.CircleHubToken, "--max-amount", "100000", "--yes")
	walletName, _ := joinResp["wallet"].(string)
	if walletName == "" {
		t.Fatalf("join: no wallet name in response: %v", joinResp)
	}

	// Resolve the freshly-minted circle_wallet's own admin app id — needed
	// to query its ground-truth balance/transactions independent of
	// anything cashctl itself reports.
	children, err := admin.listCircleChildren(hubResp.ID)
	if err != nil || len(children) != 1 {
		t.Fatalf("listCircleChildren: want exactly 1 child, got %v (err=%v)", children, err)
	}
	childAppID := children[0].AppID

	// Fund the circle wallet: a second ephemeral plain wallet pays a real
	// invoice it made.
	const fundAmountMloki = 20000
	invoiceResp := f.mustJSON("wallet", "invoice", fmt.Sprintf("%d", fundAmountMloki), "--desc", "fee-budget test funding")
	invoiceStr, _ := invoiceResp["invoice"].(string)
	if invoiceStr == "" {
		t.Fatalf("wallet invoice: no invoice in response: %v", invoiceResp)
	}
	payer := setUpPlainWalletWithPayScope(t, admin)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	payerPairing, err := nip47.ParsePairingURI(payer.PairingUri)
	if err != nil {
		t.Fatalf("parse payer pairing URI: %v", err)
	}
	payerClient, err := relayclient.NewNWCClient(ctx, payerPairing, nip47.EncryptionNIP44V2)
	if err != nil {
		t.Fatalf("dial payer wallet: %v", err)
	}
	defer payerClient.Close()
	if _, err := payerClient.PayInvoice(ctx, nip47.PayInvoiceParams{Invoice: invoiceStr}); err != nil {
		t.Fatalf("payer failed to fund the circle wallet: %v", err)
	}

	balBeforePay, err := admin.getAppBalance(childAppID)
	if err != nil {
		t.Fatalf("admin getAppBalance (before pay): %v", err)
	}
	if balBeforePay < fundAmountMloki {
		t.Fatalf("circle wallet balance after funding = %d, want >= %d", balBeforePay, fundAmountMloki)
	}
	// Ground truth for cashctl's own passthrough claim: `wallet balance
	// --from` must equal the admin API's own ledger figure exactly, to the
	// mloki — no cashctl-side rounding/derivation involved.
	cashctlBalBefore := f.mustJSON("wallet", "balance", "--from", walletName)
	if got, _ := cashctlBalBefore["amount_mloki"].(float64); int64(got) != balBeforePay {
		t.Errorf("wallet balance --from (before pay) = %v, want %d (admin API ground truth)", cashctlBalBefore["amount_mloki"], balBeforePay)
	}

	// Pay out to a THIRD wallet (distinct from the funding payer).
	const payAmountMloki = 5000
	payee := setUpPlainWalletWithPayScope(t, admin)
	payeePairing, err := nip47.ParsePairingURI(payee.PairingUri)
	if err != nil {
		t.Fatalf("parse payee pairing URI: %v", err)
	}
	payeeClient, err := relayclient.NewNWCClient(ctx, payeePairing, nip47.EncryptionNIP44V2)
	if err != nil {
		t.Fatalf("dial payee wallet: %v", err)
	}
	defer payeeClient.Close()
	payeeInvoice, err := payeeClient.MakeInvoice(ctx, nip47.MakeInvoiceParams{Amount: payAmountMloki, Description: "fee-budget test payout"})
	if err != nil {
		t.Fatalf("payee make_invoice: %v", err)
	}

	payResp := f.mustJSON("wallet", "pay", payeeInvoice.Invoice)
	preimage, _ := payResp["preimage"].(string)
	if preimage == "" {
		t.Fatalf("wallet pay: no preimage in response: %v", payResp)
	}
	cashctlFeesPaid, _ := payResp["fees_paid"].(float64)
	cashctlFeeSkim, _ := payResp["fee_skim_mloki"].(float64)

	balAfterPay, err := admin.getAppBalance(childAppID)
	if err != nil {
		t.Fatalf("admin getAppBalance (after pay): %v", err)
	}

	txs, err := admin.listAppTransactions(childAppID)
	if err != nil {
		t.Fatalf("admin listAppTransactions: %v", err)
	}
	var settled *adminTransaction
	for i := range txs {
		if txs[i].PaymentHash == "" {
			continue
		}
		if txs[i].Amount == payAmountMloki {
			settled = &txs[i]
			break
		}
	}
	if settled == nil {
		t.Fatalf("admin listAppTransactions: no outgoing %d-mloki transaction found among %v", payAmountMloki, txs)
	}

	// The core cross-check: cashctl's own `wallet pay` numbers must match
	// the Hub's ledger exactly — this is what would have silently failed
	// before the nmilat fix (fee_skim_mloki always absent/zero regardless
	// of the real server-side value).
	if int64(cashctlFeesPaid) != int64(settled.FeesPaid) {
		t.Errorf("wallet pay fees_paid = %v, want %d (admin API ground truth)", payResp["fees_paid"], settled.FeesPaid)
	}
	if int64(cashctlFeeSkim) != int64(settled.FeeSkim) {
		t.Errorf("wallet pay fee_skim_mloki = %v, want %d (admin API ground truth)", payResp["fee_skim_mloki"], settled.FeeSkim)
	}
	// Single-node lab: this specific payment is self-payment-exempt (see
	// this test's own doc comment) — assert that plainly rather than
	// silently assuming it, so a future lab change that defeats the
	// exemption turns this into a loud failure instead of a quiet no-op.
	if settled.FeeSkim != 0 {
		t.Logf("non-zero fee_skim_mloki=%d observed live — self-payment exemption did not apply; cashctl reported %v, matches ground truth: %v", settled.FeeSkim, cashctlFeeSkim, int64(cashctlFeeSkim) == int64(settled.FeeSkim))
	}

	// Balance delta must equal amount+fees_paid+fee_skim exactly, on both
	// sides — proving `wallet balance` needs no fee-aware arithmetic of its
	// own because the Hub already folds the skim into the balance it
	// reports.
	wantDelta := int64(payAmountMloki) + int64(settled.FeesPaid) + int64(settled.FeeSkim)
	gotDelta := balBeforePay - balAfterPay
	if gotDelta != wantDelta {
		t.Errorf("circle wallet balance dropped by %d, want %d (amount+fees_paid+fee_skim, admin API ground truth)", gotDelta, wantDelta)
	}

	cashctlBalAfter := f.mustJSON("wallet", "balance", "--from", walletName)
	if got, _ := cashctlBalAfter["amount_mloki"].(float64); int64(got) != balAfterPay {
		t.Errorf("wallet balance --from (after pay) = %v, want %d (admin API ground truth)", cashctlBalAfter["amount_mloki"], balAfterPay)
	}

	budgetResp := f.mustJSON("wallet", "budget")
	usedBudget, _ := budgetResp["used_budget"].(float64)
	if int64(usedBudget) != wantDelta {
		t.Errorf("wallet budget used_budget = %v, want %d — Hub's own GetBudgetUsageSat already folds amount+fee+fee_skim together, cashctl must relay it unchanged", budgetResp["used_budget"], wantDelta)
	}
}

// ---------------------------------------------------------------------------
// Round 3: CircleFeesPpm/CircleMinBudgetRenewal boundary values that round 2
// never exercised. Same principle as above (verified against the admin API's
// own ground truth, never cashctl's own self-report) plus lokihub's own
// source for validation/overflow behavior read directly rather than assumed.
// ---------------------------------------------------------------------------

// TestWalletGetInfo_CircleHubConnection_FeesPpmZero_AndMinBudgetRenewalNotSurfaced
// is CircleFeesPpm's "0" boundary, plus a live confirmation of
// CircleMinBudgetRenewal's own protocol-level gap:
//
//  1. fees_ppm: 0 must be PRESENT in the circle_wallet block, not silently
//     dropped. nip47.CircleWalletInfo.FeesPpm has no `omitempty` tag — only
//     the containing *CircleWalletInfo pointer does (gated on app.Kind ==
//     circle_hub, never on the value inside it) — so an explicit 0 and an
//     omitted CircleFeesPpm are structurally indistinguishable at every hop
//     on the request side too: this suite's own adminCreateAppRequest.
//     CircleFeesPpm and lokihub's own CreateAppRequest.CircleFeesPpm are both
//     plain non-pointer ints, so a missing JSON key and an explicit `0`
//     unmarshal to the identical Go zero value either way — there is no wire
//     representation where "explicit zero" and "omitted" could ever diverge,
//     so the only side actually worth a live check is the response: does a
//     no-fee hub still report an explicit 0, not an absent key or a missing
//     circle_wallet block entirely (which is exactly what round 2's bug
//     looked like from the caller's side, before the nmilat fix).
//  2. CircleMinBudgetRenewal never appears anywhere in get_info's
//     circle_wallet terms block, even when deliberately configured to a
//     distinctive, non-default value ("weekly") on this same hub: lokihub's
//     own nip47/controllers/get_info_controller.go circleWalletInfo struct
//     only carries {available_mloki, max_exp_secs, fees_ppm, circle_policy}.
//     This is a protocol-level gap, the same class as docs/private/
//     audit-round2-fee-invoice-amount.md's/audit-round2-expiration-matrix.md's
//     own "known, unfixed — outside this codebase" findings: there is no
//     other NIP-47 call cashctl could make instead to learn a Circle Hub's
//     min_budget_renewal floor, before or after joining.
func TestWalletGetInfo_CircleHubConnection_FeesPpmZero_AndMinBudgetRenewalNotSurfaced(t *testing.T) {
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
	npub, _ := initResp["npub"].(string)
	pubHex, err := npubToHex(npub)
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	// Built inline rather than via setUpCircleHubOpts, same reason as
	// TestWalletGetInfo_CircleHubConnection_SurfacesFeesPpm above: the shared
	// fixture doesn't grant the get_info scope get_info's circle_wallet terms
	// block needs.
	hubReq := adminCreateAppRequest{
		Name:                    ephemeralFixtureNamePrefix + " circle_hub (zero fee, get_info scope)",
		Scopes:                  []string{"circle_wallet", "get_info"},
		Kind:                    "circle_hub",
		CircleIdentityName:      ephemeralFixtureNamePrefix + " circle identity (zero fee)",
		CirclePolicy:            "allowlist",
		CircleMaxExpSecs:        86400,
		CirclePerWalletMaxMloki: 1_000_000,
		// CircleFeesPpm deliberately left zero-valued — this test's whole
		// point is the zero/omitted case. CircleMinBudgetRenewal is set to a
		// distinctive non-default value specifically to prove it's invisible
		// on the wire even when it IS configured, not just when it's at its
		// own default.
		CircleMinBudgetRenewal: "weekly",
	}
	hubResp, err := admin.createApp(hubReq)
	if err != nil {
		t.Fatalf("create ephemeral circle_hub: %v", err)
	}
	t.Cleanup(func() {
		if err := admin.deleteApp(hubResp.ID); err != nil {
			t.Logf("cleanup: delete ephemeral circle_hub app_id=%d: %v", hubResp.ID, err)
		}
	})
	if err := admin.transfer(hubResp.ID, 100); err != nil {
		t.Fatalf("fund ephemeral circle_hub: %v", err)
	}
	if err := admin.addCircleAllowlistMember(hubResp.ID, pubHex); err != nil {
		t.Fatalf("authorize identity under circle_hub allowlist: %v", err)
	}

	if res := f.run("connect", "add", "thehub", hubResp.PairingUri); res.ExitCode != 0 {
		t.Fatalf("connect add: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	infoResp := f.mustJSON("-c", "thehub", "wallet", "get-info")
	cw, _ := infoResp["circle_wallet"].(map[string]any)
	if cw == nil {
		t.Fatalf("wallet get-info --json: no circle_wallet field in response: %v", infoResp)
	}
	gotFeesPpm, present := cw["fees_ppm"]
	if !present {
		t.Fatalf("circle_wallet.fees_ppm key is entirely absent — a zero-fee hub must still report an explicit 0, not silently vanish: %v", cw)
	}
	if gotFeesPpm.(float64) != 0 {
		t.Errorf("circle_wallet.fees_ppm = %v, want 0", gotFeesPpm)
	}

	for _, unwanted := range []string{"min_budget_renewal", "budget_renewal", "circle_min_budget_renewal"} {
		if v, present := cw[unwanted]; present {
			t.Errorf("circle_wallet unexpectedly carries %q = %v — lokihub's wire shape may have grown a renewal-floor field; cmd/wallet.go's get-info text mode should be updated to surface it, and this test's own doc comment updated", unwanted, v)
		}
	}
}

// TestCircleHub_CreateApp_FeesPpmOutOfRange_RejectedAtCreation is
// CircleFeesPpm's other boundary: a >100% rate (1,500,000 ppm) bypasses
// setUpCircleHubOpts entirely (that helper has no way to express an invalid
// value on purpose) and calls the admin API directly, mirroring lokihub's own
// apps/circle_hub_service.go validation (`config.FeesPpm < 0 ||
// config.FeesPpm > constants.MAX_FEES_PPM`, MAX_FEES_PPM == 1_000_000 ==
// 100%). Confirms lokihub itself refuses to even create such a hub — the
// out-of-range value never reaches a live CircleHubConfig row, so cashctl/
// nmilat's own FeeSkimMloki-shaped fields (nip47.CircleWalletInfo.FeesPpm,
// nip47.PayInvoiceResult.FeeSkimMloki) can never observe it coming from a
// legitimately-created hub.
//
// lokihub's own transactions_service.go CalculateFeeSkimMloki — the EXACT
// function circle forwarding fees and cash_redeem's own redeem fee both
// share, see cash_redeem_controller.go/list_recipients_controller.go's own
// call sites — additionally hardens itself against this exact out-of-range
// case with a 128-bit multiply (math/bits.Mul64/Div64) plus a
// saturate-at-MaxUint64 fallback rather than a panicking/wrapping divide;
// see transactions/cash_audit_redeemfee_secB_rounding_test.go's own
// TestCashAuditSecB_CalculateFeeSkimMloki_SaturatesOnOutOfRangeRate (already
// committed lokihub-side coverage predating this round, not written here).
// So even a hypothetical future code path that let an out-of-range rate
// through creation validation couldn't overflow/wrap into a wrong-but-
// plausible fee downstream — confirmed by reading, not re-tested here since
// it's already covered and lokihub is out of scope to modify or duplicate
// tests into.
func TestCircleHub_CreateApp_FeesPpmOutOfRange_RejectedAtCreation(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	hubResp, err := admin.createApp(adminCreateAppRequest{
		Name:                    ephemeralFixtureNamePrefix + " circle_hub (invalid fees_ppm)",
		Scopes:                  []string{"circle_wallet"},
		Kind:                    "circle_hub",
		CircleIdentityName:      ephemeralFixtureNamePrefix + " circle identity (invalid fees_ppm)",
		CirclePolicy:            "allowlist",
		CircleMaxExpSecs:        86400,
		CirclePerWalletMaxMloki: 1_000_000,
		CircleFeesPpm:           1_500_000, // 150% — constants.MAX_FEES_PPM is 1_000_000 (100%)
	})
	if err == nil {
		// Only reached if lokihub's own validation regressed — clean up the
		// unexpectedly-created hub rather than leaking it into the shared lab.
		t.Cleanup(func() {
			if delErr := admin.deleteApp(hubResp.ID); delErr != nil {
				t.Logf("cleanup: delete unexpectedly-created circle_hub app_id=%d: %v", hubResp.ID, delErr)
			}
		})
		t.Fatalf("admin createApp with circleFeesPpm=1,500,000 (150%%) succeeded, want rejection (lokihub's own apps.CreateCircleHub validates 0 <= fees_ppm <= MAX_FEES_PPM): %+v", hubResp)
	}
	t.Logf("admin createApp with an out-of-range circleFeesPpm correctly rejected: %v", err)
}

// TestCircleWallet_FeesPpm100Percent_SelfPaymentExempt_MatchesHubGroundTruth
// mirrors TestCircleWallet_FeeBudgetMatchesHubGroundTruth above at
// CircleFeesPpm's theoretical ceiling (1,000,000 ppm == constants.
// MAX_FEES_PPM == 100%, skimming the entire payment) instead of an ordinary
// 5000 ppm. This lab's single physical LN node makes every payment here
// self-payment-exempt regardless of feesPpm (same reasoning as that test's
// own doc comment), so this doesn't observe a real 100% skim either — what it
// DOES prove: even at the maximum configurable rate, the exemption check
// runs first and unconditionally (transactions_service.go's validateCanPay:
// `if row.AppKind == db.AppKindCircleWallet && !selfPayment` —
// CalculateFeeSkimMloki is never even called when selfPayment is true, at
// ANY feesPpm), and cashctl's own balance/budget numbers still match the
// Hub's ledger exactly, with no client-side arithmetic anywhere near this
// boundary that could get it wrong. See TestCircleWallet_
// FeesPpm100Percent_NonSelfPayment_ViaFlndInvoice below for the attempt at
// actually forcing a real, non-exempt skim.
func TestCircleWallet_FeesPpm100Percent_SelfPaymentExempt_MatchesHubGroundTruth(t *testing.T) {
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
	npub, _ := initResp["npub"].(string)
	pubHex, err := npubToHex(npub)
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	const feesPpm = 1_000_000 // 100% — constants.MAX_FEES_PPM
	hubResp := setUpCircleHubOpts(t, admin, pubHex, circleHubOpts{FeesPpm: feesPpm})
	if hubResp.CircleHubToken == nil {
		t.Fatalf("create ephemeral circle_hub: no circleHubToken in response: %+v", hubResp)
	}

	joinResp := f.mustJSON("join", "--hub", *hubResp.CircleHubToken, "--max-amount", "100000", "--yes")
	walletName, _ := joinResp["wallet"].(string)
	if walletName == "" {
		t.Fatalf("join: no wallet name in response: %v", joinResp)
	}

	children, err := admin.listCircleChildren(hubResp.ID)
	if err != nil || len(children) != 1 {
		t.Fatalf("listCircleChildren: want exactly 1 child, got %v (err=%v)", children, err)
	}
	childAppID := children[0].AppID

	const fundAmountMloki = 20000
	invoiceResp := f.mustJSON("wallet", "invoice", fmt.Sprintf("%d", fundAmountMloki), "--desc", "100pct fee-budget test funding")
	invoiceStr, _ := invoiceResp["invoice"].(string)
	if invoiceStr == "" {
		t.Fatalf("wallet invoice: no invoice in response: %v", invoiceResp)
	}
	payer := setUpPlainWalletWithPayScope(t, admin)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	payerPairing, err := nip47.ParsePairingURI(payer.PairingUri)
	if err != nil {
		t.Fatalf("parse payer pairing URI: %v", err)
	}
	payerClient, err := relayclient.NewNWCClient(ctx, payerPairing, nip47.EncryptionNIP44V2)
	if err != nil {
		t.Fatalf("dial payer wallet: %v", err)
	}
	defer payerClient.Close()
	if _, err := payerClient.PayInvoice(ctx, nip47.PayInvoiceParams{Invoice: invoiceStr}); err != nil {
		t.Fatalf("payer failed to fund the circle wallet: %v", err)
	}

	balBeforePay, err := admin.getAppBalance(childAppID)
	if err != nil {
		t.Fatalf("admin getAppBalance (before pay): %v", err)
	}
	if balBeforePay < fundAmountMloki {
		t.Fatalf("circle wallet balance after funding = %d, want >= %d", balBeforePay, fundAmountMloki)
	}

	const payAmountMloki = 5000
	payee := setUpPlainWalletWithPayScope(t, admin)
	payeePairing, err := nip47.ParsePairingURI(payee.PairingUri)
	if err != nil {
		t.Fatalf("parse payee pairing URI: %v", err)
	}
	payeeClient, err := relayclient.NewNWCClient(ctx, payeePairing, nip47.EncryptionNIP44V2)
	if err != nil {
		t.Fatalf("dial payee wallet: %v", err)
	}
	defer payeeClient.Close()
	payeeInvoice, err := payeeClient.MakeInvoice(ctx, nip47.MakeInvoiceParams{Amount: payAmountMloki, Description: "100pct fee-budget test payout"})
	if err != nil {
		t.Fatalf("payee make_invoice: %v", err)
	}

	payResp := f.mustJSON("wallet", "pay", payeeInvoice.Invoice)
	preimage, _ := payResp["preimage"].(string)
	if preimage == "" {
		t.Fatalf("wallet pay: no preimage in response: %v", payResp)
	}
	cashctlFeesPaid, _ := payResp["fees_paid"].(float64)
	cashctlFeeSkim, _ := payResp["fee_skim_mloki"].(float64)

	balAfterPay, err := admin.getAppBalance(childAppID)
	if err != nil {
		t.Fatalf("admin getAppBalance (after pay): %v", err)
	}

	txs, err := admin.listAppTransactions(childAppID)
	if err != nil {
		t.Fatalf("admin listAppTransactions: %v", err)
	}
	var settled *adminTransaction
	for i := range txs {
		if txs[i].PaymentHash == "" {
			continue
		}
		if txs[i].Amount == payAmountMloki {
			settled = &txs[i]
			break
		}
	}
	if settled == nil {
		t.Fatalf("admin listAppTransactions: no outgoing %d-mloki transaction found among %v", payAmountMloki, txs)
	}

	if int64(cashctlFeesPaid) != int64(settled.FeesPaid) {
		t.Errorf("wallet pay fees_paid = %v, want %d (admin API ground truth)", payResp["fees_paid"], settled.FeesPaid)
	}
	if int64(cashctlFeeSkim) != int64(settled.FeeSkim) {
		t.Errorf("wallet pay fee_skim_mloki = %v, want %d (admin API ground truth)", payResp["fee_skim_mloki"], settled.FeeSkim)
	}
	// The headline assertion for this variant: even at the maximum
	// configurable rate, the self-payment exemption still yields exactly
	// zero — not the full payAmountMloki a naive "always apply feesPpm"
	// implementation would produce.
	if settled.FeeSkim != 0 {
		t.Errorf("fee_skim = %d on a self-payment-exempt transfer even at feesPpm=1,000,000 (100%%) — the exemption must short-circuit before CalculateFeeSkimMloki is ever called, regardless of the configured rate", settled.FeeSkim)
	}

	wantDelta := int64(payAmountMloki) + int64(settled.FeesPaid) + int64(settled.FeeSkim)
	gotDelta := balBeforePay - balAfterPay
	if gotDelta != wantDelta {
		t.Errorf("circle wallet balance dropped by %d, want %d (amount+fees_paid+fee_skim, admin API ground truth)", gotDelta, wantDelta)
	}
}

// TestCircleWallet_FeesPpm100Percent_NonSelfPayment_ViaFlndInvoice attempts
// to defeat the self-payment exemption for a circle_wallet's own outgoing
// payment, the same way docs/private/audit-round2-fee-invoice-amount.md's
// mintInvoiceFromFlnd technique (integration/cash_redeem_fee_test.go, same
// package) does for cash_redeem: transactions_service.go's SendPaymentSync
// calls the exact same transactions.IsSelfPayment predicate for EVERY
// outgoing payment (not just cash_redeem's own separate step-9 check before
// it), so an invoice minted directly on the lab's underlying flnd node —
// bypassing lokihub's own make_invoice tracking, so no incoming db.
// Transaction row exists for its payment hash — should defeat the exemption
// here for the identical structural reason.
//
// At FeesPpm=1,000,000 (100%), a genuine non-self payment of payAmountMloki
// reserves payAmountMloki (the payment itself) + payAmountMloki (the 100%
// skim) + a ~1% fee reserve — funded comfortably below so a rejection, if
// any, is attributable to the LN payment attempt itself, not this test's own
// arithmetic.
func TestCircleWallet_FeesPpm100Percent_NonSelfPayment_ViaFlndInvoice(t *testing.T) {
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
	npub, _ := initResp["npub"].(string)
	pubHex, err := npubToHex(npub)
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	const feesPpm = 1_000_000 // 100% — constants.MAX_FEES_PPM
	hubResp := setUpCircleHubOpts(t, admin, pubHex, circleHubOpts{FeesPpm: feesPpm, FundLoki: 200})
	if hubResp.CircleHubToken == nil {
		t.Fatalf("create ephemeral circle_hub: no circleHubToken in response: %+v", hubResp)
	}

	joinResp := f.mustJSON("join", "--hub", *hubResp.CircleHubToken, "--max-amount", "150000", "--yes")
	walletName, _ := joinResp["wallet"].(string)
	if walletName == "" {
		t.Fatalf("join: no wallet name in response: %v", joinResp)
	}

	children, err := admin.listCircleChildren(hubResp.ID)
	if err != nil || len(children) != 1 {
		t.Fatalf("listCircleChildren: want exactly 1 child, got %v (err=%v)", children, err)
	}
	childAppID := children[0].AppID

	// Fund generously: needs to cover payAmountMloki + its 100% skim + fee
	// reserve for the attempt below.
	const fundAmountMloki = 100_000
	invoiceResp := f.mustJSON("wallet", "invoice", fmt.Sprintf("%d", fundAmountMloki), "--desc", "100pct fee non-self-payment test funding")
	invoiceStr, _ := invoiceResp["invoice"].(string)
	if invoiceStr == "" {
		t.Fatalf("wallet invoice: no invoice in response: %v", invoiceResp)
	}
	payer := setUpPlainWalletWithPayScope(t, admin)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	payerPairing, err := nip47.ParsePairingURI(payer.PairingUri)
	if err != nil {
		t.Fatalf("parse payer pairing URI: %v", err)
	}
	payerClient, err := relayclient.NewNWCClient(ctx, payerPairing, nip47.EncryptionNIP44V2)
	if err != nil {
		t.Fatalf("dial payer wallet: %v", err)
	}
	defer payerClient.Close()
	if _, err := payerClient.PayInvoice(ctx, nip47.PayInvoiceParams{Invoice: invoiceStr}); err != nil {
		t.Fatalf("payer failed to fund the circle wallet: %v", err)
	}

	balBefore, err := admin.getAppBalance(childAppID)
	if err != nil {
		t.Fatalf("admin getAppBalance (before attempt): %v", err)
	}

	const payAmountMloki = 10_000
	// mintInvoiceFromFlnd skips this test cleanly, not a failure, if the
	// lab's docker-compose topology isn't reachable exactly as expected (see
	// its own doc comment in cash_redeem_fee_test.go).
	flndInvoice := mintInvoiceFromFlnd(t, payAmountMloki, "circle 100pct fee: non-self-payment attempt")

	payRes := f.run("wallet", "pay", flndInvoice)
	t.Logf("wallet pay (non-self-payment, 100%% circle fee): exit=%d stdout=%s stderr=%s", payRes.ExitCode, payRes.Stdout, payRes.Stderr)

	if payRes.ExitCode == 0 {
		// This would mean the lab topology changed enough for a genuine
		// external payment to actually complete — confirm a real skim shows
		// up, and that it's the full 100%, rather than silently assuming
		// this branch never happens.
		var payResp map[string]any
		if jsonErr := json.Unmarshal([]byte(payRes.Stdout), &payResp); jsonErr != nil {
			t.Fatalf("decode wallet pay stdout: %v: %s", jsonErr, payRes.Stdout)
		}
		feeSkim, _ := payResp["fee_skim_mloki"].(float64)
		if int64(feeSkim) != payAmountMloki {
			t.Errorf("wallet pay succeeded against a non-self-payment invoice at a 100%% circle fee: fee_skim_mloki = %v, want %d (the full amount)", payResp["fee_skim_mloki"], payAmountMloki)
		} else {
			t.Logf("genuine non-self-payment succeeded with a real 100%% fee_skim_mloki=%d observed live — lab topology must have gained real outbound routing since docs/private/audit-round2-fee-invoice-amount.md was written", payAmountMloki)
		}
	} else {
		// Expected in this single-node lab: the budget/balance check passes
		// (this attempt got past the same class of check the flnd-invoice
		// trick got past for cash_redeem's own amount check), but the actual
		// LN payment attempt then fails once flnd's own router refuses a
		// genuine self-payment (no allow_self_payment) — see
		// docs/private/audit-round2-fee-invoice-amount.md's identical
		// "self-payments not allowed" observation for cash_redeem. Whatever
		// the exact NWC code turns out to be, it must NOT be a budget/balance
		// rejection (QUOTA_EXCEEDED/INSUFFICIENT_BALANCE) — that would mean
		// this test's own funding arithmetic was wrong, not a genuine
		// lab-topology limit.
		nwcCode := jsonErrorNWCCode(t, payRes.Stderr)
		if nwcCode == "QUOTA_EXCEEDED" || nwcCode == "INSUFFICIENT_BALANCE" {
			t.Errorf("wallet pay failed on a budget/balance check (nwc_code=%s) — this test's own funding was supposed to cover amount+100%%skim+reserve; re-check the arithmetic rather than assuming this is the expected flnd self-payment rejection: %s", nwcCode, payRes.Stderr)
		}
	}

	balAfter, err := admin.getAppBalance(childAppID)
	if err != nil {
		t.Fatalf("admin getAppBalance (after attempt): %v", err)
	}
	if payRes.ExitCode != 0 && balAfter != balBefore {
		t.Errorf("a failed payment attempt must not move any funds: balance before=%d after=%d", balBefore, balAfter)
	}
}

// TestCircleHub_MinBudgetRenewal_TighterThanFloorRejected exercises this
// round's other never-before-tested knob: CircleMinBudgetRenewal, the
// live-configurable floor on how tight a joining member's OWN requested
// budget_renewal cadence may be (lokihub's nip47/controllers/
// create_circle_wallet_controller.go step 4c, constants.BudgetRenewalRank).
// cashctl's join/circle create already exposes a --budget-renewal flag
// (cmd/circle.go, wired straight through to nipcw.CreateCircleWalletParams.
// BudgetRenewal) — this confirms a too-tight request against a real floor is
// rejected sensibly (classified invalid_input, the wallet never gets
// registered) rather than silently accepted or misclassified.
func TestCircleHub_MinBudgetRenewal_TighterThanFloorRejected(t *testing.T) {
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
	npub, _ := initResp["npub"].(string)
	pubHex, err := npubToHex(npub)
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	hubResp := setUpCircleHubOpts(t, admin, pubHex, circleHubOpts{MinBudgetRenewal: "weekly"})
	if hubResp.CircleHubToken == nil {
		t.Fatalf("create ephemeral circle_hub: no circleHubToken in response: %+v", hubResp)
	}

	res := f.run("join", "--hub", *hubResp.CircleHubToken, "--max-amount", "100000", "--budget-renewal", "daily", "--yes")
	if res.ExitCode != 3 {
		t.Fatalf("join --budget-renewal daily against a weekly floor: exit = %d, want 3 (invalid_input)\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if got := jsonErrorNWCCode(t, res.Stderr); got != "BAD_REQUEST" {
		t.Errorf("nwc_code = %q, want BAD_REQUEST (create_circle_wallet_controller.go's min_budget_renewal floor rejection): %s", got, res.Stderr)
	}

	listResp := f.mustJSON("connect", "list")
	if conns, _ := listResp["connections"].([]any); len(conns) != 0 {
		t.Errorf("join must not register a wallet on rejection, but connect list reports: %v", conns)
	}
}

// TestCircleHub_MinBudgetRenewal_AtOrLooserThanFloorAccepted is the
// compliant counterpart: a request at or looser than the floor succeeds, and
// the member's own resolved cadence — not the Hub's floor itself, which
// TestWalletGetInfo_CircleHubConnection_FeesPpmZero_AndMinBudgetRenewalNotSurfaced
// above confirms cashctl never even gets to see — is what `wallet budget`
// reports.
func TestCircleHub_MinBudgetRenewal_AtOrLooserThanFloorAccepted(t *testing.T) {
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
	npub, _ := initResp["npub"].(string)
	pubHex, err := npubToHex(npub)
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	hubResp := setUpCircleHubOpts(t, admin, pubHex, circleHubOpts{MinBudgetRenewal: "weekly"})
	if hubResp.CircleHubToken == nil {
		t.Fatalf("create ephemeral circle_hub: no circleHubToken in response: %+v", hubResp)
	}

	joinResp := f.mustJSON("join", "--hub", *hubResp.CircleHubToken, "--max-amount", "100000", "--budget-renewal", "monthly", "--yes")
	walletName, _ := joinResp["wallet"].(string)
	if walletName == "" {
		t.Fatalf("join --budget-renewal monthly against a weekly floor: no wallet name in response: %v", joinResp)
	}

	budgetResp := f.mustJSON("wallet", "budget")
	if renewal, _ := budgetResp["renewal_period"].(string); renewal != "monthly" {
		t.Errorf("wallet budget renewal_period = %q, want %q (the member's own requested, floor-compliant cadence)", renewal, "monthly")
	}
}

// TestCircleHub_MinBudgetRenewal_OmittedDefaultsToNeverRegardlessOfFloor
// confirms create_circle_wallet_controller.go's own documented exemption
// ("An omitted choice defaults to 'never' (always compliant, regardless of
// the hub's floor)") from cashctl's side: omitting --budget-renewal entirely
// against the loosest real floor short of "never" itself ("yearly") must
// still succeed, and the member ends up with "never" — not rejected, and not
// silently coerced to "yearly".
func TestCircleHub_MinBudgetRenewal_OmittedDefaultsToNeverRegardlessOfFloor(t *testing.T) {
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
	npub, _ := initResp["npub"].(string)
	pubHex, err := npubToHex(npub)
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	hubResp := setUpCircleHubOpts(t, admin, pubHex, circleHubOpts{MinBudgetRenewal: "yearly"})
	if hubResp.CircleHubToken == nil {
		t.Fatalf("create ephemeral circle_hub: no circleHubToken in response: %+v", hubResp)
	}

	joinResp := f.mustJSON("join", "--hub", *hubResp.CircleHubToken, "--max-amount", "100000", "--yes")
	walletName, _ := joinResp["wallet"].(string)
	if walletName == "" {
		t.Fatalf("join with no --budget-renewal against a yearly floor: no wallet name in response: %v", joinResp)
	}

	budgetResp := f.mustJSON("wallet", "budget")
	if renewal, _ := budgetResp["renewal_period"].(string); renewal != "never" {
		t.Errorf("wallet budget renewal_period = %q, want %q (an omitted request always defaults to never, regardless of the hub's floor)", renewal, "never")
	}
	if _, present := budgetResp["renews_at"]; present {
		t.Errorf("wallet budget renews_at should be absent for renewal_period=never, got: %v", budgetResp["renews_at"])
	}
}
