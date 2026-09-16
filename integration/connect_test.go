//go:build integration

package integration

import (
	"testing"
)

// setUpPlainWallet provisions a throwaway, plain (non-hub) isolated app
// granted get_info+get_balance — good enough for `connect add`/`wallet
// get-info` to prove a registered connection actually dials, without
// needing any cash/circle machinery.
func setUpPlainWallet(t *testing.T, admin *adminClient) adminCreateAppResponse {
	t.Helper()
	resp, err := admin.createApp(adminCreateAppRequest{
		Name:   ephemeralFixtureNamePrefix + " plain wallet",
		Scopes: []string{"get_info", "get_balance"},
	})
	if err != nil {
		t.Fatalf("create ephemeral plain wallet: %v", err)
	}
	t.Cleanup(func() {
		if err := admin.deleteApp(resp.ID); err != nil {
			t.Logf("cleanup: delete ephemeral plain wallet app_id=%d: %v", resp.ID, err)
		}
	})
	return resp
}

// setUpSignerWallet provisions a throwaway, non-isolated (standard) app
// granted only sign_message — no funding needed, since signing never
// touches balance. sign_message specifically MUST NOT be requested on an
// isolated/sub-wallet app (the admin API itself rejects that: "sub-wallet
// app connection cannot have sign_message scope") — a standard app is the
// only kind that can ever be granted it.
func setUpSignerWallet(t *testing.T, admin *adminClient) adminCreateAppResponse {
	t.Helper()
	resp, err := admin.createApp(adminCreateAppRequest{
		Name:   ephemeralFixtureNamePrefix + " signer wallet",
		Scopes: []string{"sign_message"},
	})
	if err != nil {
		t.Fatalf("create ephemeral signer wallet: %v", err)
	}
	t.Cleanup(func() {
		if err := admin.deleteApp(resp.ID); err != nil {
			t.Logf("cleanup: delete ephemeral signer wallet app_id=%d: %v", resp.ID, err)
		}
	})
	return resp
}

// setUpPlainWalletWithPayScope is setUpPlainWallet plus pay_invoice/
// make_invoice and real root funding — good enough to act as an external
// payer/payee in tests that need to actually move money in or out of
// another wallet (e.g. paying a circle wallet's invoice).
func setUpPlainWalletWithPayScope(t *testing.T, admin *adminClient) adminCreateAppResponse {
	t.Helper()
	resp, err := admin.createApp(adminCreateAppRequest{
		Name:   ephemeralFixtureNamePrefix + " plain payer wallet",
		Scopes: []string{"get_info", "get_balance", "make_invoice", "pay_invoice"},
		// "isolated" is required for /api/transfers to fund it — a
		// non-isolated app shares the node's own real balance instead of
		// having one of its own.
		Kind: "isolated",
	})
	if err != nil {
		t.Fatalf("create ephemeral plain payer wallet: %v", err)
	}
	t.Cleanup(func() {
		if err := admin.deleteApp(resp.ID); err != nil {
			t.Logf("cleanup: delete ephemeral plain payer wallet app_id=%d: %v", resp.ID, err)
		}
	})
	if err := admin.transfer(resp.ID, 2000); err != nil {
		t.Fatalf("fund ephemeral plain payer wallet: %v", err)
	}
	return resp
}

// TestConnect_AddListUseRm exercises the full connect/wallet-use surface
// against two real, live wallet connections: registering, listing,
// switching the default, the -c/--connection override, and removal —
// verified by actually dialing (`wallet get-info`), not just checking the
// local cashctl.db state.
func TestConnect_AddListUseRm(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	f := newFixture(t)
	f.mustJSON("wallet", "init")

	work := setUpPlainWallet(t, admin)
	second := setUpPlainWallet(t, admin)

	addWork := f.mustJSON("connect", "add", "work", work.PairingUri)
	if name, _ := addWork["name"].(string); name != "work" {
		t.Errorf("connect add work: name = %v, want \"work\"", addWork["name"])
	}
	if def, _ := addWork["default"].(bool); !def {
		t.Errorf("connect add work (first-ever wallet): expected default=true, got %v", addWork)
	}

	listResp := f.mustJSON("connect", "list")
	if def, _ := listResp["default"].(string); def != "work" {
		t.Errorf("connect list: default = %v, want \"work\"", listResp["default"])
	}

	infoResp := f.mustJSON("wallet", "get-info")
	if infoResp["methods"] == nil {
		t.Errorf("wallet get-info (default = work): doesn't look like a real get_info result: %v", infoResp)
	}

	addSecond := f.mustJSON("connect", "add", "second", second.PairingUri)
	if def, _ := addSecond["default"].(bool); def {
		t.Errorf("connect add second (not first-ever): expected default=false (least-surprise), got %v", addSecond)
	}
	listResp2 := f.mustJSON("connect", "list")
	if def, _ := listResp2["default"].(string); def != "work" {
		t.Errorf("connect list after adding second: default should still be \"work\", got %v", listResp2["default"])
	}

	useResp := f.mustJSON("connect", "use", "second")
	if def, _ := useResp["default"].(string); def != "second" {
		t.Errorf("connect use second: default = %v, want \"second\"", useResp["default"])
	}

	// -c/--connection overrides the (now "second") default for one call.
	overrideResp := f.mustJSON("--connection", "work", "wallet", "get-info")
	if overrideResp["methods"] == nil {
		t.Errorf("wallet get-info -c work: doesn't look like a real get_info result: %v", overrideResp)
	}

	rmResp := f.mustJSON("connect", "rm", "work")
	if removed, _ := rmResp["removed"].(string); removed != "work" {
		t.Errorf("connect rm work: removed = %v, want \"work\"", rmResp["removed"])
	}
	listResp3 := f.mustJSON("connect", "list")
	conns, _ := listResp3["connections"].([]any)
	for _, c := range conns {
		entry, _ := c.(map[string]any)
		if entry["name"] == "work" {
			t.Errorf("connect list after rm work: \"work\" still present: %v", conns)
		}
	}
}
