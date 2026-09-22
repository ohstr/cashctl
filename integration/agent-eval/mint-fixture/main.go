// Command mint-fixture is a tiny dev-only helper for the agent-eval
// harness (bin/run.sh's prepare_r1 step): mint_cash is a wallet-side NIP-47
// call over Nostr, not a REST admin-API endpoint, so unlike every other
// fixture this harness provisions (plain bash + curl against lokihub's
// admin API), minting an actual cash token needs a real NWC dial —
// this program is that one Go-shaped exception. Not part of cashctl itself;
// run with `go run`, never built/shipped.
//
// Usage:
//
//	mint-fixture mint   <admin-base-url> <admin-token> <amount-millis>
//	  -> {"cash_token","hub_app_id"} on stdout
//	mint-fixture cleanup <admin-base-url> <admin-token> <hub-app-id>
//	mint-fixture probe  <admin-base-url> <admin-token>
//	  -> exit 0 if a real NWC round trip (mint_cash) succeeds, self-cleaning
//	     either way; for polling whether the dev stack's NWC backend is up.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: mint-fixture mint|cleanup|probe <admin-base-url> <admin-token> [amount-millis|hub-app-id]")
		os.Exit(2)
	}
	mode, baseURL, token := os.Args[1], os.Args[2], os.Args[3]
	switch mode {
	case "mint":
		amount, err := strconv.ParseUint(os.Args[4], 10, 64)
		must(err)
		must(mint(baseURL, token, amount))
	case "cleanup":
		appID, err := strconv.ParseUint(os.Args[4], 10, 64)
		must(err)
		must(cleanup(baseURL, token, uint(appID)))
	case "probe":
		must(probe(baseURL, token))
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", mode)
		os.Exit(2)
	}
}

// probe creates a throwaway cash_hub, attempts a real mint_cash round trip
// with a short timeout, and deletes the hub again either way — a
// self-contained, self-cleaning liveness check for the dev stack's NWC
// backend specifically (not just its admin REST API, which can be up
// while the wallet/NWC side is down).
func probe(baseURL, token string) error {
	var created struct {
		ID         uint   `json:"id"`
		PairingUri string `json:"pairingUri"`
	}
	err := doAdmin(baseURL, token, http.MethodPost, "/api/apps", map[string]any{
		"name":                  "cashctl agent-eval nwc-probe",
		"kind":                  "cash_hub",
		"scopes":                []string{"cash_hub"},
		"cashPerWalletMaxMloki": 10_000_000,
		"cashMaxExpSecs":        3600,
	}, &created)
	if err != nil {
		return fmt.Errorf("create probe cash_hub: %w", err)
	}
	defer func() {
		_ = doAdmin(baseURL, token, http.MethodDelete, fmt.Sprintf("/api/apps/%d", created.ID), nil, nil)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := nipcashclient.Connect(ctx, created.PairingUri)
	if err != nil {
		return fmt.Errorf("dial probe cash_hub: %w", err)
	}
	defer client.Close()

	result, err := client.MintCash(ctx, nipcash.MintCashParams{
		Recipients: []nipcash.Allocation{nipcash.Send(nipcash.Anyone(), 1000)},
	})
	if err != nil {
		return fmt.Errorf("mint_cash: %w", err)
	}

	// The probe's own minted child needs reclaiming before the deferred
	// hub delete above can succeed (apps.DeleteApp refuses a cash_hub
	// with any child still attached) — best-effort, the hub's own
	// t.Cleanup-style delete above will just log/skip if this misses.
	_ = result
	var claims struct {
		Claims []struct {
			WalletAppID uint `json:"wallet_app_id"`
		} `json:"claims"`
	}
	if err := doAdmin(baseURL, token, http.MethodGet, fmt.Sprintf("/api/apps/%d/cash-wallets?limit=0", created.ID), nil, &claims); err == nil {
		for _, c := range claims.Claims {
			_ = doAdmin(baseURL, token, http.MethodDelete, fmt.Sprintf("/api/apps/%d/cash-wallets/%d", created.ID, c.WalletAppID), nil, nil)
		}
	}
	return nil
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "mint-fixture:", err)
		os.Exit(1)
	}
}

func doAdmin(baseURL, token, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, baseURL+path, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("admin API %s %s: status %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(respBody))
	}
	if out != nil && len(respBody) > 0 {
		return json.Unmarshal(respBody, out)
	}
	return nil
}

func mint(baseURL, token string, amountMillis uint64) error {
	var created struct {
		ID         uint   `json:"id"`
		PairingUri string `json:"pairingUri"`
	}
	err := doAdmin(baseURL, token, http.MethodPost, "/api/apps", map[string]any{
		"name":                  "cashctl agent-eval r1-cash-lifecycle",
		"kind":                  "cash_hub",
		"scopes":                []string{"cash_hub"},
		"cashPerWalletMaxMloki": 10_000_000,
		"cashMaxExpSecs":        3600,
	}, &created)
	if err != nil {
		return fmt.Errorf("create ephemeral cash_hub: %w", err)
	}

	if err := doAdmin(baseURL, token, http.MethodPost, "/api/transfers", map[string]any{
		"toAppId": created.ID, "amountLoki": 2000,
	}, nil); err != nil {
		return fmt.Errorf("fund ephemeral cash_hub: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := nipcashclient.Connect(ctx, created.PairingUri)
	if err != nil {
		return fmt.Errorf("dial cash_hub: %w", err)
	}
	defer client.Close()

	result, err := client.MintCash(ctx, nipcash.MintCashParams{
		Recipients: []nipcash.Allocation{nipcash.Send(nipcash.Anyone(), amountMillis)},
	})
	if err != nil {
		return fmt.Errorf("mint_cash: %w", err)
	}

	out, _ := json.Marshal(map[string]any{"cash_token": result.CashToken, "hub_app_id": created.ID})
	fmt.Println(string(out))
	return nil
}

// cleanup reclaims any cash_wallet child minted under hubAppID and deletes
// the ephemeral hub itself — apps.DeleteApp refuses a cash_hub with any
// child still attached.
func cleanup(baseURL, token string, hubAppID uint) error {
	var claims struct {
		Claims []struct {
			WalletAppID uint `json:"wallet_app_id"`
		} `json:"claims"`
	}
	if err := doAdmin(baseURL, token, http.MethodGet, fmt.Sprintf("/api/apps/%d/cash-wallets?limit=0", hubAppID), nil, &claims); err != nil {
		return fmt.Errorf("list cash wallet children: %w", err)
	}
	seen := map[uint]bool{}
	for _, c := range claims.Claims {
		if seen[c.WalletAppID] {
			continue
		}
		seen[c.WalletAppID] = true
		if err := doAdmin(baseURL, token, http.MethodDelete, fmt.Sprintf("/api/apps/%d/cash-wallets/%d", hubAppID, c.WalletAppID), nil, nil); err != nil {
			fmt.Fprintf(os.Stderr, "mint-fixture: cleanup: delete cash wallet child app_id=%d: %v\n", c.WalletAppID, err)
		}
	}
	return doAdmin(baseURL, token, http.MethodDelete, fmt.Sprintf("/api/apps/%d", hubAppID), nil, nil)
}
