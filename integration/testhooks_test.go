//go:build integration

package integration

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// The regtest dev stack's lokihub serves chain and payer test hooks under
// /api/test (a testhooks build with LOKIHUB_TEST_HOOKS=1, regtest only),
// behind the same full-access admin token as the rest of the admin API. The
// tests that need a payer or a chain use them and skip cleanly against a Hub
// without them.
//
// CASHCTL_E2E_HOOKS=0 turns them off even when the Hub has them.

var (
	hooksOnce sync.Once
	hooksErr  error
)

// hooksOrSkip returns the admin client when the Hub answers the test hooks,
// and skips the test otherwise.
func hooksOrSkip(t *testing.T) *adminClient {
	t.Helper()
	admin := adminOrSkip(t)
	if os.Getenv("CASHCTL_E2E_HOOKS") == "0" {
		t.Skip("skipping: CASHCTL_E2E_HOOKS=0")
	}
	hooksOnce.Do(func() {
		var tip hookTip
		hooksErr = admin.doBody(http.MethodGet, "/api/test/chain/tip", nil, &tip)
	})
	if hooksErr != nil {
		t.Skipf("skipping: the Hub serves no regtest test hooks (%v) — see lokihub's integration/README.md", hooksErr)
	}
	return admin
}

type hookTip struct {
	Height int32  `json:"height"`
	Hash   string `json:"hash"`
}

type hookTx struct {
	Txid  string `json:"txid"`
	Hex   string `json:"hex"`
	Depth int64  `json:"depth"`
}

type hookPayment struct {
	Status        string `json:"status"`
	Preimage      string `json:"preimage,omitempty"`
	FailureReason string `json:"failure_reason,omitempty"`
}

// hookMine mines n blocks to the payer, leaving out withheld transactions.
func (c *adminClient) hookMine(t *testing.T, n int) {
	t.Helper()
	if err := c.doBody(http.MethodPost, "/api/test/chain/mine", map[string]any{"blocks": n}, nil); err != nil {
		t.Fatalf("mine %d: %v", n, err)
	}
}

func (c *adminClient) hookTip(t *testing.T) hookTip {
	t.Helper()
	var tip hookTip
	if err := c.doBody(http.MethodGet, "/api/test/chain/tip", nil, &tip); err != nil {
		t.Fatalf("chain tip: %v", err)
	}
	return tip
}

// hookTx reads txid from the chain node; ok is false when it does not know it.
func (c *adminClient) hookTx(t *testing.T, txid string) (hookTx, bool) {
	t.Helper()
	var tx hookTx
	if err := c.doBody(http.MethodGet, "/api/test/chain/tx/"+txid, nil, &tx); err != nil {
		if strings.Contains(err.Error(), "status 404") {
			return tx, false
		}
		t.Fatalf("chain tx %s: %v", txid, err)
	}
	return tx, true
}

// hookWithhold keeps txid (and its descendants) out of mined blocks while on.
func (c *adminClient) hookWithhold(t *testing.T, txid string, on bool) {
	t.Helper()
	if err := c.doBody(http.MethodPost, "/api/test/chain/withhold", map[string]any{"txid": txid, "on": on}, nil); err != nil {
		t.Fatalf("withhold %s=%t: %v", txid, on, err)
	}
}

// hookPayInvoice pays bolt11 from the payer node and waits for the outcome.
func (c *adminClient) hookPayInvoice(t *testing.T, bolt11 string) hookPayment {
	t.Helper()
	slow := *c
	slow.http = &http.Client{Timeout: 100 * time.Second}
	var p hookPayment
	if err := slow.doBody(http.MethodPost, "/api/test/payer/pay-invoice", map[string]any{"invoice": bolt11}, &p); err != nil {
		t.Fatalf("payer pay-invoice: %v", err)
	}
	return p
}

// hookPayAddress pays millis (whole loki) on-chain from the payer to address
// and returns the txid.
func (c *adminClient) hookPayAddress(t *testing.T, address string, millis uint64) string {
	t.Helper()
	var out struct {
		Txid string `json:"txid"`
	}
	if err := c.doBody(http.MethodPost, "/api/test/payer/pay-address", map[string]any{"address": address, "amount_millis": millis}, &out); err != nil {
		t.Fatalf("payer pay-address: %v", err)
	}
	return out.Txid
}

// hookPayerInvoice is an invoice of the payer node: a genuinely external payee
// for the Hub under test.
func (c *adminClient) hookPayerInvoice(amountMillis uint64) (string, error) {
	var out struct {
		Invoice        string `json:"invoice"`
		PaymentRequest string `json:"payment_request"`
	}
	if err := c.doBody(http.MethodPost, "/api/test/payer/invoice", map[string]any{"amount_millis": amountMillis}, &out); err != nil {
		return "", err
	}
	if out.Invoice != "" {
		return out.Invoice, nil
	}
	if out.PaymentRequest != "" {
		return out.PaymentRequest, nil
	}
	return "", errors.New("payer invoice: empty answer")
}

// hookPayerAddress is a fresh payer address: an address the Hub does not
// control, for withdrawals.
func (c *adminClient) hookPayerAddress(t *testing.T) string {
	t.Helper()
	var out struct {
		Address string `json:"address"`
	}
	if err := c.doBody(http.MethodGet, "/api/test/payer/address", nil, &out); err != nil {
		t.Fatalf("payer address: %v", err)
	}
	if out.Address == "" {
		t.Fatal("payer address: empty answer")
	}
	return out.Address
}

// mineUntil mines one block between polls of cond, up to limit blocks.
func (c *adminClient) mineUntil(t *testing.T, limit int, what string, cond func() bool) {
	t.Helper()
	for i := 0; i <= limit; i++ {
		if cond() {
			return
		}
		if i == limit {
			break
		}
		c.hookMine(t, 1)
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("%s: not reached after mining %d blocks", what, limit)
}

// eventually polls cond every 3 s until it holds or d passes.
func eventually(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: not reached within %s", what, d)
		}
		time.Sleep(3 * time.Second)
	}
}

// mustDo is an admin API call that fails the test on error.
func (c *adminClient) mustDo(t *testing.T, method, path string, body, out any) {
	t.Helper()
	if err := c.doBody(method, path, body, out); err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
}

func hubPath(id uint, rest string) string { return fmt.Sprintf("/api/cash-hubs/%d/%s", id, rest) }
