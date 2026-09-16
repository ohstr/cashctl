//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nip47"
)

// TestCircleWallet_BudgetCapDeclineIsCleanAndWalletStaysUsable joins a
// circle with a deliberately low spend cap, then tries to pay an invoice
// that exceeds it — a real client hits this constantly (a shared/limited
// wallet, a kid's allowance wallet, a per-app budget). The decline must
// surface as one clean classified error, on stderr only, and must NOT
// leave the wallet connection itself unusable afterward.
func TestCircleWallet_BudgetCapDeclineIsCleanAndWalletStaysUsable(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	u := newFixture(t)
	uInit := u.mustJSON("wallet", "init")
	uPub, err := npubToHex(uInit["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	circleHub := setUpCircleHub(t, admin, uPub)
	if circleHub.CircleHubToken == nil || *circleHub.CircleHubToken == "" {
		t.Fatalf("create ephemeral circle_hub: no circleHubToken in response: %+v", circleHub)
	}
	const capMloki = uint64(5_000)
	joinResp := u.mustJSON("join", "--hub", *circleHub.CircleHubToken, "--max-amount", "5000", "--yes")
	if walletName, _ := joinResp["wallet"].(string); walletName == "" {
		t.Fatalf("join: no wallet name in response: %v", joinResp)
	}

	// An invoice well beyond the cap, from an unrelated funded wallet.
	payHub := setUpCashHub(t, admin)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	payHubNWC := dialNWC(t, ctx, payHub.PairingUri)
	invoice, err := payHubNWC.MakeInvoice(ctx, nip47.MakeInvoiceParams{Amount: int64(capMloki * 10)})
	if err != nil {
		t.Fatalf("make_invoice (over budget cap): %v", err)
	}

	res := u.run("pay", invoice.Invoice, "--yes")
	if res.ExitCode == 0 {
		t.Fatalf("pay (over the wallet's own %d budget cap) unexpectedly succeeded: %s", capMloki, res.Stdout)
	}
	var body struct {
		Error     string `json:"error"`
		Code      string `json:"code"`
		Retryable bool   `json:"retryable"`
		NWCCode   string `json:"nwc_code"`
	}
	if err := json.Unmarshal([]byte(res.Stderr), &body); err != nil {
		t.Fatalf("decode pay's --json error body: %v\nstderr: %s", err, res.Stderr)
	}
	if body.Code == "" || body.Error == "" {
		t.Errorf("pay (over budget cap): incomplete error body: %+v", body)
	}
	t.Logf("budget-cap decline classified as code=%q nwc_code=%q message=%q", body.Code, body.NWCCode, body.Error)

	// The decline must be a pure no-op on the wallet connection itself —
	// still fully usable right afterward, not left in some broken state.
	if res := u.run("wallet", "get-info"); res.ExitCode != 0 {
		t.Errorf("wallet get-info (right after a declined pay): exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
}

// TestCircleWallet_ExpiredFallsBackToStrandedBalance confirms `wallet
// balance`'s documented fallback for an expired wallet: real money already
// redeemed into it before expiry must keep showing up (marked stranded),
// never silently drop off the unified total just because the wallet can
// no longer be queried live. Requires the Hub to actually enforce a
// requested short --expiry — if the resulting wallet is still answering
// get_balance live well past it, that's a Hub-configuration question, not
// a cashctl behavior this test can force, so it skips cleanly.
func TestCircleWallet_ExpiredFallsBackToStrandedBalance(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	u := newFixture(t)
	uInit := u.mustJSON("wallet", "init")
	uPub, err := npubToHex(uInit["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	circleHub := setUpCircleHub(t, admin, uPub)
	if circleHub.CircleHubToken == nil || *circleHub.CircleHubToken == "" {
		t.Fatalf("create ephemeral circle_hub: no circleHubToken in response: %+v", circleHub)
	}
	const expiry = 20 * time.Second
	joinTime := time.Now()
	joinResp := u.mustJSON("join", "--hub", *circleHub.CircleHubToken, "--max-amount", "100000", "--expiry", expiry.String(), "--yes")
	walletName, _ := joinResp["wallet"].(string)
	if walletName == "" {
		t.Fatalf("join: no wallet name in response: %v", joinResp)
	}

	// Redeem real cash into it before expiry, so the cached figure this
	// test checks for is actually meaningful, not just a stranded zero.
	cashHub := setUpCashHub(t, admin)
	const fundedAmount = uint64(8_000)
	token := mintPubkeyTokenFromHub(t, cashHub, uPub, fundedAmount)
	receiveResp := u.mustJSON("receive", token)
	entryID, _ := receiveResp["entry"].(map[string]any)["id"].(string)
	if res := u.run("redeem", "--token", entryID, "--yes"); res.ExitCode != 0 {
		t.Fatalf("redeem into the soon-to-expire wallet: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	// Cache a live figure while the wallet is still valid.
	preBalance := u.mustJSON("wallet", "balance", "--breakdown")
	preBreakdown, _ := preBalance["breakdown"].([]any)
	var preAmount float64
	foundPre := false
	for _, line := range preBreakdown {
		l, _ := line.(map[string]any)
		if name, _ := l["name"].(string); name == walletName {
			foundPre = true
			preAmount, _ = l["amount_mloki"].(float64)
			if stranded, _ := l["stranded"].(bool); stranded {
				t.Fatalf("wallet balance (pre-expiry): %q already reports stranded — expiry fired before this test could cache a live figure; the requested %s expiry may be too short for this environment", walletName, expiry)
			}
		}
	}
	if !foundPre || preAmount <= 0 {
		t.Fatalf("wallet balance (pre-expiry): no live, positive figure cached for %q: %v", walletName, preBreakdown)
	}

	// Wait past expiry (plus margin), then re-check.
	elapsed := time.Since(joinTime)
	if wait := expiry + 5*time.Second - elapsed; wait > 0 {
		time.Sleep(wait)
	}

	postBalance := u.mustJSON("wallet", "balance", "--breakdown")
	postBreakdown, _ := postBalance["breakdown"].([]any)
	strandedTotal, _ := postBalance["stranded_mloki"].(float64)
	foundPost := false
	for _, line := range postBreakdown {
		l, _ := line.(map[string]any)
		if name, _ := l["name"].(string); name == walletName {
			foundPost = true
			stranded, _ := l["stranded"].(bool)
			if !stranded {
				t.Skip("skipping: this wallet is still answering get_balance live past its requested expiry — Hub doesn't enforce it that precisely, not a cashctl gap")
			}
			postAmount, _ := l["amount_mloki"].(float64)
			if postAmount != preAmount {
				t.Errorf("wallet balance (post-expiry, stranded): amount_mloki = %v, want the same cached %v as before expiry", postAmount, preAmount)
			}
		}
	}
	if !foundPost {
		t.Fatalf("wallet balance (post-expiry): %q vanished from the breakdown entirely — an expired wallet's money must never go silently invisible: %v", walletName, postBreakdown)
	}
	if strandedTotal <= 0 {
		t.Errorf("wallet balance (post-expiry): stranded_mloki = %v, want > 0", strandedTotal)
	}
}
