//go:build integration

// admin_client.go is a thin client for lokihub's own admin HTTP API (the
// same one the frontend calls). It exists ONLY for test fixture setup/
// teardown — minting a throwaway cash_hub/circle_hub, funding it, deleting
// it afterward — never for the actual test assertions themselves, which
// stay a black-box NWC/CLI client (see integration/README.md).
package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type adminClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// newAdminClient returns ok=false if admin_api isn't configured, so callers
// can skip cleanly rather than requiring it.
func newAdminClient(cfg *Config) (client *adminClient, ok bool) {
	if cfg.AdminAPI.BaseURL == "" || cfg.AdminAPI.Token == "" {
		return nil, false
	}
	return &adminClient{
		baseURL: strings.TrimRight(cfg.AdminAPI.BaseURL, "/"),
		token:   cfg.AdminAPI.Token,
		http:    &http.Client{Timeout: 15 * time.Second},
	}, true
}

func (c *adminClient) doBody(method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequest(method, c.baseURL+path, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("admin API %s %s: status %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(respBody))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("admin API %s %s: decode response: %w", method, path, err)
		}
	}
	return nil
}

// adminCreateAppRequest mirrors lokihub's api.CreateAppRequest, kept to just
// the fields this suite's fixtures need.
type adminCreateAppRequest struct {
	Name          string   `json:"name"`
	MaxAmountLoki uint64   `json:"maxAmount"`
	ExpiresAt     string   `json:"expiresAt,omitempty"` // RFC3339, empty = never
	Scopes        []string `json:"scopes"`
	Kind          string   `json:"kind"`

	// cash_hub-only fields — required (must be positive) when Kind is
	// "cash_hub" (see lokihub's apps.CreateCashHub). CashRedeemFeePpm is the
	// default per-million cash_redeem fee (0 = free) a freshly-minted
	// wallet's slices inherit — see lokihub's api.CreateAppRequest and
	// NIP-CASH §The Redeem Fee.
	CashPerWalletMaxMloki int   `json:"cashPerWalletMaxMloki,omitempty"`
	CashMaxExpSecs        int   `json:"cashMaxExpSecs,omitempty"`
	CashRedeemFeePpm      int   `json:"cashRedeemFeePpm,omitempty"`
	CashMinTransferMloki  int64 `json:"cashMinTransferMloki,omitempty"`

	// circle_hub-only fields (Kind == "circle_hub") — CircleIdentityName/
	// CirclePolicy create a brand-new CircleIdentity; this suite always
	// uses "allowlist" (no synthetic Nostr follow-graph needed).
	// CircleFeesPpm is the circle's own forwarding fee (0 = none, and never
	// omitted-as-default the way the other fields here are, since 0 is
	// itself a meaningful, deliberately-chosen value for most fixtures).
	CircleIdentityName      string `json:"circleIdentityName,omitempty"`
	CirclePolicy            string `json:"circlePolicy,omitempty"`
	CircleMaxExpSecs        int    `json:"circleMaxExpSecs,omitempty"`
	CirclePerWalletMaxMloki int    `json:"circlePerWalletMaxMloki,omitempty"`
	CircleFeesPpm           int    `json:"circleFeesPpm,omitempty"`
	CircleMinBudgetRenewal  string `json:"circleMinBudgetRenewal,omitempty"`
}

// adminCreateAppResponse mirrors lokihub's api.CreateAppResponse, including
// the cashHubToken/circleHubToken fields this suite exists to exercise.
type adminCreateAppResponse struct {
	ID             uint    `json:"id"`
	WalletPubkey   string  `json:"walletPubkey"`
	PairingUri     string  `json:"pairingUri"`
	CashHubToken   *string `json:"cashHubToken,omitempty"`
	CircleHubToken *string `json:"circleHubToken,omitempty"`
}

// createApp creates a new app via the admin API (POST /api/apps) with a
// server-generated pairing keypair.
func (c *adminClient) createApp(req adminCreateAppRequest) (adminCreateAppResponse, error) {
	var resp adminCreateAppResponse
	err := c.doBody(http.MethodPost, "/api/apps", req, &resp)
	return resp, err
}

// deleteApp deletes an app by id (DELETE /api/apps/:id) — only valid for an
// app with no children of its own; apps.DeleteApp refuses otherwise, so a
// cash_hub/circle_hub with any minted child must have it reclaimed first
// (see listCashWalletClaims/deleteCashWallet, listCircleChildren/
// deleteCircleChild below).
func (c *adminClient) deleteApp(id uint) error {
	return c.doBody(http.MethodDelete, fmt.Sprintf("/api/apps/%d", id), nil, nil)
}

type adminCashWalletClaim struct {
	ID          uint `json:"id"`
	WalletAppID uint `json:"wallet_app_id"`
	// Archived marks a slice whose bill no longer exists. The Hub deletes a
	// bill once nothing is left on it and keeps its slices in an archive, so
	// this listing returns both and a caller asserting on live state must
	// filter. Status is how the slice ended: unclaimed, redeemed, split,
	// expired, reclaimed or written-off.
	Archived bool   `json:"archived"`
	Status   string `json:"status"`
}

// listCashWalletClaims returns every recipient-slice claim row under
// hubAppID — cash_wallet children are excluded from the general /api/apps
// listing, so this is how a cash_wallet child's admin app id (WalletAppID)
// is resolved for deleteCashWallet.
func (c *adminClient) listCashWalletClaims(hubAppID uint) ([]adminCashWalletClaim, error) {
	var resp struct {
		Claims []adminCashWalletClaim `json:"claims"`
	}
	err := c.doBody(http.MethodGet, fmt.Sprintf("/api/apps/%d/cash-wallets?limit=0", hubAppID), nil, &resp)
	return resp.Claims, err
}

// deleteCashWallet reclaims walletAppID's remaining balance to hubAppID and
// hard-deletes it. Retries on "still settling" — a real, expected-transient
// guard server-side against deleting a wallet mid-payment-settlement.
func (c *adminClient) deleteCashWallet(hubAppID, walletAppID uint) error {
	const maxAttempts = 5
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err = c.doBody(http.MethodDelete, fmt.Sprintf("/api/apps/%d/cash-wallets/%d", hubAppID, walletAppID), nil, nil)
		if err == nil || !strings.Contains(err.Error(), "still settling") {
			return err
		}
		time.Sleep(time.Duration(attempt) * time.Second)
	}
	return err
}

type adminCircleChild struct {
	AppID uint `json:"appId"`
}

// listCircleChildren returns every circle_wallet child currently minted
// under hubAppID.
func (c *adminClient) listCircleChildren(hubAppID uint) ([]adminCircleChild, error) {
	var resp struct {
		Children []adminCircleChild `json:"children"`
	}
	err := c.doBody(http.MethodGet, fmt.Sprintf("/api/apps/%d/circle/children?limit=0", hubAppID), nil, &resp)
	return resp.Children, err
}

// deleteCircleChild reclaims childAppID's remaining balance to its parent
// hub and hard-deletes it. Retries on "still settling", same as
// deleteCashWallet.
func (c *adminClient) deleteCircleChild(hubAppID, childAppID uint) error {
	const maxAttempts = 5
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err = c.doBody(http.MethodDelete, fmt.Sprintf("/api/apps/%d/circle/children/%d", hubAppID, childAppID), nil, nil)
		if err == nil || !strings.Contains(err.Error(), "still settling") {
			return err
		}
		time.Sleep(time.Duration(attempt) * time.Second)
	}
	return err
}

// transfer moves amountLoki into toAppID's isolated balance via a real
// internal self-payment (api.Transfer), rooted at the node's own real
// balance (fromAppId omitted) — how every ephemeral hub in this suite
// self-funds without depending on a pre-provisioned funding source.
func (c *adminClient) transfer(toAppID uint, amountLoki uint64) error {
	body := map[string]any{"toAppId": toAppID, "amountLoki": amountLoki}
	return c.doBody(http.MethodPost, "/api/transfers", body, nil)
}

// addCircleAllowlistMember authorizes pubkeyHex under an allowlist-policy
// circle_hub — the allowlist endpoint is full-replace-only (PUT), so this
// reads the current list first and appends to it.
func (c *adminClient) addCircleAllowlistMember(hubAppID uint, pubkeyHex string) error {
	var current struct {
		Pubkeys []string `json:"pubkeys"`
	}
	if err := c.doBody(http.MethodGet, fmt.Sprintf("/api/apps/%d/circle/allowlist", hubAppID), nil, &current); err != nil {
		return err
	}
	body := map[string][]string{"pubkeys": append(current.Pubkeys, pubkeyHex)}
	return c.doBody(http.MethodPut, fmt.Sprintf("/api/apps/%d/circle/allowlist", hubAppID), body, nil)
}
