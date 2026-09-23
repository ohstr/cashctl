//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nip47"
	"github.com/ohstr/nmilat/nipcash"
)

// adminOrSkip is the standard "needs a live lokihub" preamble: loads the
// integration config and returns the admin client, skipping the test cleanly
// when this suite isn't configured (see integration/README.md).
func adminOrSkip(t *testing.T) *adminClient {
	t.Helper()
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}
	return admin
}

// mintCashGift mints a cash-mode token from hub and returns the shareable
// "<token>#<cash_secret>" gift string, the form `cashctl receive` takes.
func mintCashGift(t *testing.T, hub adminCreateAppResponse, amountMillis uint64) string {
	t.Helper()
	return retryEphemeralNWC(t, "mint_cash (cash)", func(ctx context.Context) (string, error) {
		cashClient := dialCash(t, ctx, hub.PairingUri)
		result, err := cashClient.MintCash(ctx, nipcash.MintCashParams{
			Recipients: []nipcash.Allocation{nipcash.Send(nipcash.Anyone(), amountMillis)},
		})
		if err != nil {
			return "", err
		}
		if len(result.Recipients) != 1 || result.Recipients[0].CashSecret == "" {
			t.Fatalf("mint_cash (cash): expected exactly one recipient with a cash_secret: %+v", result.Recipients)
		}
		return result.CashToken + "#" + result.Recipients[0].CashSecret, nil
	})
}

// makeHubInvoice has hub's own NWC connection make an invoice for exactly
// amountMillis — what `cashctl redeem --invoice` pays out to, from the same
// hub the token was minted by (the way cash_test.go's lifecycle test does).
func makeHubInvoice(t *testing.T, hub adminCreateAppResponse, amountMillis uint64) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	nwc := dialNWC(t, ctx, hub.PairingUri)
	inv, err := nwc.MakeInvoice(ctx, nip47.MakeInvoiceParams{Amount: int64(amountMillis)})
	if err != nil {
		t.Fatalf("make_invoice(%d mloki): %v", amountMillis, err)
	}
	return inv.Invoice
}

// assertNoIdentityDemand fails the test if res (a step in a flow that must
// never need a local identity) asked the user to run `cashctl init`.
func assertNoIdentityDemand(t *testing.T, step string, res result) {
	t.Helper()
	if strings.Contains(res.Stderr, "cashctl init") || strings.Contains(res.Stdout, "cashctl init") {
		t.Fatalf("%s: demanded a local identity from a cash-mode-only wallet (exit %d)\nstdout: %s\nstderr: %s", step, res.ExitCode, res.Stdout, res.Stderr)
	}
}

// entryID pulls the ledger id out of a receive/consolidate "entry"-shaped
// JSON object.
func entryID(t *testing.T, entry any) string {
	t.Helper()
	m, _ := entry.(map[string]any)
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatalf("no id in entry: %v", entry)
	}
	return id
}

// mloki reads a JSON number field as whole mloki.
func mloki(m map[string]any, key string) uint64 {
	v, _ := m[key].(float64)
	return uint64(v)
}

// mustDecodeJSON decodes raw as a JSON object, failing the test with the
// step name and the raw text if it isn't one.
func mustDecodeJSON(t *testing.T, step, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("%s: stdout is not a JSON object: %v\nstdout: %s", step, err, raw)
	}
	return out
}
