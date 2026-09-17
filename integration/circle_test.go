//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nip47"
	relayclient "github.com/ohstr/nmilat/relay/client"
)

// circleHubOpts configures an ephemeral circle_hub's admin-set policy knobs
// for scenario-specific fixtures (fee/expiry audits vary these
// deliberately) — zero-valued fields fall back to setUpCircleHub's own
// long-standing defaults, so most callers only need to set what their
// scenario actually varies. Mirrors lokihub's own
// integration/ephemeral_test.go ephemeralCircleHubOpts pattern.
type circleHubOpts struct {
	MaxExpSecs        int    // 0 -> 86400
	PerWalletMaxMloki int    // 0 -> 1_000_000
	FeesPpm           int    // 0 is itself meaningful (no fee) - never defaulted
	MinBudgetRenewal  string // "" -> lokihub defaults to "monthly" server-side
	FundLoki          uint64 // 0 -> 100
}

func (o circleHubOpts) withDefaults() circleHubOpts {
	if o.MaxExpSecs == 0 {
		o.MaxExpSecs = 86400
	}
	if o.PerWalletMaxMloki == 0 {
		o.PerWalletMaxMloki = 1_000_000
	}
	if o.FundLoki == 0 {
		o.FundLoki = 100
	}
	return o
}

// setUpCircleHub provisions a throwaway allowlist-policy circle_hub,
// authorizes pubkeyHex under it, root-funds it, and registers cleanup —
// the shared fixture every circle test in this file builds on. Uses
// circleHubOpts's defaults — see setUpCircleHubOpts for fee/expiry-scenario
// fixtures.
func setUpCircleHub(t *testing.T, admin *adminClient, pubkeyHex string) adminCreateAppResponse {
	t.Helper()
	return setUpCircleHubOpts(t, admin, pubkeyHex, circleHubOpts{})
}

// setUpCircleHubOpts is setUpCircleHub's parametrized form — lets a
// scenario test configure a nonzero FeesPpm or a short MaxExpSecs without
// duplicating the create/fund/allowlist/cleanup wiring.
func setUpCircleHubOpts(t *testing.T, admin *adminClient, pubkeyHex string, opts circleHubOpts) adminCreateAppResponse {
	t.Helper()
	opts = opts.withDefaults()
	hubResp, err := admin.createApp(adminCreateAppRequest{
		Name: ephemeralFixtureNamePrefix + " circle_hub",
		Kind: "circle_hub",
		// Scope literal mirrors lokihub's constants.CIRCLE_WALLET_SCOPE
		// ("circle_wallet", not "circle_hub" — see cash_test.go's
		// setUpCashHub comment on why this suite names scopes directly
		// rather than importing lokihub's own package.
		Scopes:                  []string{"circle_wallet"},
		CircleIdentityName:      ephemeralFixtureNamePrefix + " circle identity",
		CirclePolicy:            "allowlist",
		CircleMaxExpSecs:        opts.MaxExpSecs,
		CirclePerWalletMaxMloki: opts.PerWalletMaxMloki,
		CircleFeesPpm:           opts.FeesPpm,
		CircleMinBudgetRenewal:  opts.MinBudgetRenewal,
	})
	if err != nil {
		t.Fatalf("create ephemeral circle_hub: %v", err)
	}
	// Registered before the reclaim sweep below so it runs second
	// (t.Cleanup is LIFO): the circle_wallet child `join` mints must be
	// reclaimed before the hub itself can be deleted.
	t.Cleanup(func() {
		if err := admin.deleteApp(hubResp.ID); err != nil {
			t.Logf("cleanup: delete ephemeral circle_hub app_id=%d: %v", hubResp.ID, err)
		}
	})
	t.Cleanup(func() {
		children, err := admin.listCircleChildren(hubResp.ID)
		if err != nil {
			t.Logf("cleanup: list circle children for ephemeral hub app_id=%d: %v", hubResp.ID, err)
			return
		}
		for _, child := range children {
			if err := admin.deleteCircleChild(hubResp.ID, child.AppID); err != nil {
				t.Logf("cleanup: delete ephemeral circle child app_id=%d: %v", child.AppID, err)
			}
		}
	})
	if err := admin.transfer(hubResp.ID, opts.FundLoki); err != nil {
		t.Fatalf("fund ephemeral circle_hub: %v", err)
	}
	if err := admin.addCircleAllowlistMember(hubResp.ID, pubkeyHex); err != nil {
		t.Fatalf("authorize identity under circle_hub allowlist: %v", err)
	}
	return hubResp
}

// TestCircleJoin_CreateWalletAndGetInfo provisions a throwaway allowlist-
// policy circle_hub, authorizes cashctl's freshly generated local identity
// under it, joins via the compiled binary (`cashctl join --hub <circlehub1...>`,
// exercising the real bech32 Circle Hub connection format end to end), and
// confirms the resulting personal wallet is live by calling get-info on it.
func TestCircleJoin_CreateWalletAndGetInfo(t *testing.T) {
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
	if npub == "" {
		t.Fatalf("wallet init: no npub in response: %v", initResp)
	}
	pubHex, err := npubToHex(npub)
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	hubResp := setUpCircleHub(t, admin, pubHex)
	if hubResp.CircleHubToken == nil || *hubResp.CircleHubToken == "" {
		t.Fatalf("create ephemeral circle_hub: no circleHubToken in response: %+v", hubResp)
	}

	joinResp := f.mustJSON("join", "--hub", *hubResp.CircleHubToken, "--max-amount", "100", "--yes")
	walletName, _ := joinResp["wallet"].(string)
	if walletName == "" {
		t.Fatalf("join: no wallet name in response: %v", joinResp)
	}
	if def, _ := joinResp["default"].(bool); !def {
		t.Errorf("join: first-ever wallet should be set as default, got: %v", joinResp)
	}

	infoResp := f.mustJSON("wallet", "get-info")
	if infoResp["alias"] == nil && infoResp["methods"] == nil {
		t.Errorf("wallet get-info: response doesn't look like a get_info result: %v", infoResp)
	}
}

// TestCircleJoin_ViaRawNWCURI joins the same kind of circle_hub as above,
// but passing its plain pairingUri instead of the circlehub1... bech32
// form — the fallback path for a Hub that hasn't adopted the new format
// yet (resolveHubConnection's dial.KindNWCURI branch).
func TestCircleJoin_ViaRawNWCURI(t *testing.T) {
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

	hubResp := setUpCircleHub(t, admin, pubHex)

	joinResp := f.mustJSON("join", "--hub", hubResp.PairingUri, "--max-amount", "100", "--yes")
	if walletName, _ := joinResp["wallet"].(string); walletName == "" {
		t.Fatalf("join --hub <raw NWC URI>: no wallet name in response: %v", joinResp)
	}
}

// TestDecodeCheck_CircleHub confirms `cashctl decode <circlehub1...>
// --check` reports joining as possible against a real, reachable
// circle_hub (reachable + advertises create_circle_wallet), without ever
// actually joining it — no wallet gets registered as a side effect,
// checked or not.
func TestDecodeCheck_CircleHub(t *testing.T) {
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

	hubResp := setUpCircleHub(t, admin, pubHex)
	if hubResp.CircleHubToken == nil || *hubResp.CircleHubToken == "" {
		t.Fatalf("create ephemeral circle_hub: no circleHubToken in response: %+v", hubResp)
	}

	decodeResp := f.mustJSON("decode", *hubResp.CircleHubToken, "--check")
	check, _ := decodeResp["check"].(map[string]any)
	if check == nil {
		t.Fatalf("decode --check: no check field in response: %v", decodeResp)
	}
	if ok, _ := check["ok"].(bool); !ok {
		t.Errorf("decode --check on a real, reachable circle_hub: ok = %v, want true (check: %v)", check["ok"], check)
	}

	listResp := f.mustJSON("connect", "list")
	if conns, _ := listResp["connections"].([]any); len(conns) != 0 {
		t.Errorf("decode --check must never register a wallet, but connect list reports: %v", conns)
	}
}

// TestCircleWallet_FullOps joins a circle and exercises every generic
// wallet operation against the resulting personal wallet — budget,
// invoice, pay (against a real invoice from a second ephemeral plain
// wallet), list-tx, and sign-message — not just get-info, which is all
// TestCircleJoin_CreateWalletAndGetInfo touches.
func TestCircleWallet_FullOps(t *testing.T) {
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

	hubResp := setUpCircleHub(t, admin, pubHex)
	if hubResp.CircleHubToken == nil {
		t.Fatalf("create ephemeral circle_hub: no circleHubToken in response: %+v", hubResp)
	}
	// --max-amount (in loki, cashctl's CLI-facing unit — see
	// output.CurrencyUnit) must fit within setUpCircleHub's own funding
	// (100 loki) — the Hub's create_circle_wallet commitment check
	// rejects a cap it can't actually back.
	if res := f.run("join", "--hub", *hubResp.CircleHubToken, "--max-amount", "100", "--yes"); res.ExitCode != 0 {
		t.Fatalf("join: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	if budgetResp := f.mustJSON("wallet", "budget"); budgetResp["total_budget"] == nil {
		t.Errorf("wallet budget: doesn't look like a real get_budget result: %v", budgetResp)
	}

	invoiceResp := f.mustJSON("wallet", "invoice", lokiArg(10000), "--desc", "circle wallet ops test")
	invoiceStr, _ := invoiceResp["invoice"].(string)
	if invoiceStr == "" {
		t.Fatalf("wallet invoice: no invoice in response: %v", invoiceResp)
	}

	// A second, plain ephemeral wallet acts as the payer: fund it, then
	// have the circle wallet pay its invoice — proves pay_invoice works
	// on a real circle_wallet child, not just make_invoice/get_info.
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
		t.Fatalf("payer failed to pay the circle wallet's invoice: %v", err)
	}

	payeeInvoice, err := payerClient.MakeInvoice(ctx, nip47.MakeInvoiceParams{Amount: 5000})
	if err != nil {
		t.Fatalf("payer make_invoice: %v", err)
	}
	payResp := f.mustJSON("wallet", "pay", payeeInvoice.Invoice)
	// fees_paid is `omitempty` and same-node payments are typically fee-free,
	// so it may legitimately be absent — preimage is the real proof of a
	// completed payment and is never omitted.
	if preimage, _ := payResp["preimage"].(string); preimage == "" {
		t.Errorf("wallet pay: no preimage in response: %v", payResp)
	}

	listTxResp := f.mustJSON("wallet", "list-tx")
	if listTxResp["transactions"] == nil {
		t.Errorf("wallet list-tx: doesn't look like a real list_transactions result: %v", listTxResp)
	}

	// sign-message is NOT tested against the circle wallet itself: a
	// circle_wallet child is only ever granted make_invoice/pay_invoice/
	// get_balance/lookup_invoice/list_transactions/get_info (see lokihub's
	// create_circle_wallet_controller.go's own circleScopes literal) —
	// sign_message is deliberately excluded, by design, for every circle
	// wallet (and in fact for every isolated/sub-wallet app at all — the
	// admin API itself refuses to grant it to one). Tested instead against
	// a standalone signer wallet, via -c/--connection so the default (the
	// circle wallet) is left untouched.
	signer := setUpSignerWallet(t, admin)
	if res := f.run("connect", "add", "signer", signer.PairingUri); res.ExitCode != 0 {
		t.Fatalf("connect add signer: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	signResp := f.mustJSON("-c", "signer", "wallet", "sign-message", "hello from the integration suite")
	if sig, _ := signResp["signature"].(string); sig == "" {
		t.Errorf("wallet sign-message (signer): no signature in response: %v", signResp)
	}
}
