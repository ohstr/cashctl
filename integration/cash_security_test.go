//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nip47"
	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
)

// assertTerminalFailure pins the error contract on a spend that must be
// refused permanently. Checking only "exit != 0" lets a misclassification
// through: a permanently-doomed spend reported as retryable (network, or a
// retryable conflict) makes an agent back off and loop forever on it, which
// is exactly the branching AGENTS.md's `retryable` field exists to drive.
func assertTerminalFailure(t *testing.T, res result, what string, wantAnyCode ...string) {
	t.Helper()
	report := parseErrorReport(t, res)
	if report.Retryable {
		t.Errorf("%s: code=%q retryable=true — a permanently-refused spend must not tell an agent to retry", what, report.Code)
	}
	for _, code := range wantAnyCode {
		if report.Code == code {
			return
		}
	}
	t.Errorf("%s: code=%q, want one of %v", what, report.Code, wantAnyCode)
}

// TestWalletShow_NeverLeaksSecrets confirms wallet show's --json output
// never echoes back a held entry's actual spending/dialing secrets — the
// token's own NWC pairing secret, or (for a cash-mode entry) the real
// cash_secret — for both a pubkey-mode and a cash-mode held token.
// ledger.Entry tags both fields for JSON (Secret has no omitempty at
// all), so this is checked directly against real values, not assumed
// safe just because printCashBill/decode are careful elsewhere.
func TestWalletShow_NeverLeaksSecrets(t *testing.T) {
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
	myPubHex, err := npubToHex(npub)
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	pubkeyToken := mintPubkeyToken(t, admin, myPubHex, 15_000)
	if res := f.run("receive", pubkeyToken); res.ExitCode != 0 {
		t.Fatalf("receive (pubkey): exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	hub := setUpCashHub(t, admin)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cashClient := dialCash(t, ctx, hub.PairingUri)
	mintResult, err := cashClient.MintCash(ctx, nipcash.MintCashParams{
		Recipients: []nipcash.Allocation{nipcash.Send(nipcash.Anyone(), 15_000)},
	})
	if err != nil {
		t.Fatalf("mint_cash (cash): %v", err)
	}
	cashSecret := mintResult.Recipients[0].CashSecret
	if res := f.run("receive", mintResult.CashToken+"#"+cashSecret); res.ExitCode != 0 {
		t.Fatalf("receive (cash): exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	// receive's own auto-secure step re-keys this cash-mode token immediately
	// (a fresh secret only cashctl now knows) — cashSecret itself is
	// already dead by the time wallet show runs below, so this check is
	// about the ORIGINAL secret specifically never having been echoed
	// back at any point, not about it still being "the" secret now.
	res := f.run("wallet", "show")
	if strings.Contains(res.Stdout, cashSecret) {
		t.Errorf("wallet show leaked the cash_secret into its output: %s", res.Stdout)
	}

	// decode itself never exposes a token's own pairing secret (by
	// design), so there's no independent value to string-match against
	// for that field — check structurally instead: no held_tokens entry
	// may carry a non-empty "secret" or "cash_secret" field at all.
	showResp := f.mustJSON("wallet", "show")
	held, _ := showResp["held_tokens"].([]any)
	if len(held) != 2 {
		t.Fatalf("wallet show: expected 2 held tokens, got %d: %v", len(held), held)
	}
	for _, h := range held {
		entry, _ := h.(map[string]any)
		if v, present := entry["secret"]; present && v != "" && v != nil {
			t.Errorf("wallet show: held_tokens entry leaks a non-empty \"secret\" field: %v", entry)
		}
		if v, present := entry["cash_secret"]; present && v != "" && v != nil {
			t.Errorf("wallet show: held_tokens entry leaks a non-empty \"cash_secret\" field: %v", entry)
		}
	}
}

// TestDecode_NeverLeaksTokenSecret confirms decode never echoes a cash
// token's own NWC pairing secret back in its output, for a token whose
// secret is known in advance — entirely local, no live server needed.
func TestDecode_NeverLeaksTokenSecret(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")

	cashMode := false
	token := fakeCashToken(t, &cashMode)
	// fakeCashToken's own secret is opaque to this test — decode the
	// token ourselves via the same SDK to know exactly what to look for.
	tok, err := nipcash.Decode(token)
	if err != nil {
		t.Fatalf("decode fake token locally: %v", err)
	}
	if tok.Secret == "" {
		t.Fatal("fakeCashToken produced an empty secret — nothing to check")
	}

	res := f.run("decode", token)
	if strings.Contains(res.Stdout, tok.Secret) {
		t.Errorf("decode leaked the token's own pairing secret into its output: %s", res.Stdout)
	}
}

// TestCashTransfer_OverdraftAttempt confirms that asking to split off more
// than a held token's own known amount fails cleanly — cashctl has no
// client-side check of splitFlag against the token's cached amount
// (CashTransferParams.SplitAmount is sent as given), so this is really
// checking the Hub's own rejection surfaces as a classified error and
// that the token is left untouched locally, not that cashctl catches it
// itself before the wire call.
func TestCashTransfer_OverdraftAttempt(t *testing.T) {
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
	myPubHex, err := npubToHex(npub)
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	const amountMillis = uint64(10_000)
	token := mintPubkeyToken(t, admin, myPubHex, amountMillis)
	if res := f.run("receive", token); res.ExitCode != 0 {
		t.Fatalf("receive: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	res := f.run("transfer", fakeHex32(t), "--amount", "999999.999", "--yes")
	if res.ExitCode == 0 {
		t.Fatalf("transfer --amount (more than held): unexpectedly succeeded: %s", res.Stdout)
	}
	assertTerminalFailure(t, res, "overdraft transfer", "invalid_input")

	// Whatever the failure shape, the token must be left exactly as it
	// was — a rejected overdraft must never partially apply.
	showResp := f.mustJSON("wallet", "show")
	held, _ := showResp["held_tokens"].([]any)
	if len(held) != 1 {
		t.Fatalf("a rejected overdraft must leave the token held, got %d held tokens: %v", len(held), held)
	}
	entry, _ := held[0].(map[string]any)
	if status, _ := entry["status"].(string); status != "" && status != "held" {
		t.Errorf("token status = %q after a rejected overdraft, want \"held\" (untouched): %v", status, entry)
	}
	if amt, _ := entry["amount_millis"].(float64); uint64(amt) != amountMillis {
		t.Errorf("token amount_millis = %v after a rejected overdraft, want unchanged %d: %v", entry["amount_millis"], amountMillis, entry)
	}
}

// TestCashRedeem_AlreadyRedeemedTokenRejectedOnRetry drains a token via a
// real redeem, then tries to redeem the same local entry a second time by
// its --token ID (bypassing Held()'s own now-correct exclusion of a
// redeemed entry) — proving the actual enforcement against reusing an
// already-claimed slice lives server-side, not merely in cashctl's own
// local status bookkeeping, and that the rejection surfaces as a clean,
// classified error rather than corrupting the ledger.
func TestCashRedeem_AlreadyRedeemedTokenRejectedOnRetry(t *testing.T) {
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
	myPubHex, err := npubToHex(npub)
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	const amountMillis = uint64(20_000)
	hub := setUpCashHub(t, admin)
	token := mintPubkeyTokenFromHub(t, hub, myPubHex, amountMillis)
	receiveResp := f.mustJSON("receive", token)
	entry, _ := receiveResp["entry"].(map[string]any)
	tokenID, _ := entry["id"].(string)
	if tokenID == "" {
		t.Fatalf("receive: no entry id in response: %v", receiveResp)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	hubClient := dialNWC(t, ctx, hub.PairingUri)
	invoice1, err := hubClient.MakeInvoice(ctx, nip47.MakeInvoiceParams{Amount: int64(amountMillis)})
	if err != nil {
		t.Fatalf("make_invoice (1): %v", err)
	}
	if res := f.run("redeem", "--token", tokenID, "--invoice", invoice1.Invoice, "--yes"); res.ExitCode != 0 {
		t.Fatalf("first redeem: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	// Second redeem attempt against the SAME now-drained local entry.
	invoice2, err := hubClient.MakeInvoice(ctx, nip47.MakeInvoiceParams{Amount: int64(amountMillis)})
	if err != nil {
		t.Fatalf("make_invoice (2): %v", err)
	}
	res := f.run("redeem", "--token", tokenID, "--invoice", invoice2.Invoice, "--yes")
	if res.ExitCode == 0 {
		t.Fatalf("redeeming an already-redeemed token unexpectedly succeeded: %s", res.Stdout)
	}
	assertTerminalFailure(t, res, "redeem of an already-redeemed token", "not_found", "invalid_input", "conflict")

	// The ledger must still show exactly one entry, still marked redeemed
	// (not reverted, not duplicated) — a rejected re-redemption must be a
	// pure no-op locally.
	showResp := f.mustJSON("wallet", "show")
	held, _ := showResp["held_tokens"].([]any)
	if len(held) != 0 {
		t.Errorf("an already-redeemed token must not reappear as held after a rejected retry: %v", held)
	}
	historyResp := f.mustJSON("wallet", "history")
	history, _ := historyResp["history"].([]any)
	redeemCount := 0
	for _, h := range history {
		e, _ := h.(map[string]any)
		if action, _ := e["action"].(string); action == "redeem" {
			redeemCount++
		}
	}
	if redeemCount != 1 {
		t.Errorf("wallet history shows %d \"redeem\" entries, want exactly 1 (the rejected retry must not be recorded as a success): %v", redeemCount, history)
	}
}

// TestCashConsolidate_ReusingAlreadyConsolidatedSourceRejected
// consolidates two tokens, then tries to consolidate one of the
// now-already-claimed sources again (paired with a fresh third token) —
// proving the Hub's own "already claimed" rejection is what actually
// protects a source from being reused, not merely cashctl's local
// StatusConsolidated bookkeeping.
func TestCashConsolidate_ReusingAlreadyConsolidatedSourceRejected(t *testing.T) {
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
	myPubHex, err := npubToHex(npub)
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	hub := setUpCashHub(t, admin)
	tokenA := mintPubkeyTokenFromHub(t, hub, myPubHex, 10_000)
	tokenB := mintPubkeyTokenFromHub(t, hub, myPubHex, 10_000)
	tokenC := mintPubkeyTokenFromHub(t, hub, myPubHex, 10_000)

	respA := f.mustJSON("receive", tokenA)
	respB := f.mustJSON("receive", tokenB)
	respC := f.mustJSON("receive", tokenC)
	idA, _ := respA["entry"].(map[string]any)["id"].(string)
	idB, _ := respB["entry"].(map[string]any)["id"].(string)
	idC, _ := respC["entry"].(map[string]any)["id"].(string)

	if res := f.run("consolidate", "--sources", idA+","+idB, "--yes"); res.ExitCode != 0 {
		t.Fatalf("first consolidate: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	// idA is now claimed terminal server-side. Reuse it alongside the
	// still-held idC.
	res := f.run("consolidate", "--sources", idA+","+idC, "--yes")
	if res.ExitCode == 0 {
		t.Fatalf("consolidating an already-consolidated source unexpectedly succeeded: %s", res.Stdout)
	}
	assertTerminalFailure(t, res, "consolidate reusing a spent source", "not_found", "invalid_input", "conflict")

	// idC must still be untouched and held — a rejected consolidate must
	// never partially claim its sources.
	showResp := f.mustJSON("wallet", "show")
	held, _ := showResp["held_tokens"].([]any)
	foundC := false
	for _, h := range held {
		e, _ := h.(map[string]any)
		if e["id"] == idC {
			foundC = true
			if status, _ := e["status"].(string); status != "held" {
				t.Errorf("idC status = %q after a rejected consolidate, want \"held\" (untouched)", status)
			}
		}
	}
	if !foundC {
		t.Errorf("idC is no longer held after a rejected consolidate attempt that included it: %v", held)
	}
}

// TestCashTransfer_ToCashTarget_SecretMustBeRecoverable transfers a held
// pubkey-mode token to "cash" — cashctl generates a fresh
// cash_secret client-side for this (NIP-CASH §Cash-Mode Slices: unlike
// mint_cash's cash-mode recipient, cash_transfer's cash-mode target does NOT
// get a wallet-generated secret; the caller supplies the commitment
// themselves) — and confirms that secret is actually surfaced back to
// the human/agent, not silently generated and discarded once the NWC
// call returns. A secret nobody can recover is money nobody can ever
// spend — checked by actually redeeming with the secret extracted from
// transfer's own --json response, proving it's the genuine credential,
// not merely present-looking output.
func TestCashTransfer_ToCashTarget_SecretMustBeRecoverable(t *testing.T) {
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
	myPubHex, err := npubToHex(npub)
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	const amountMillis = uint64(35_000)
	hub := setUpCashHub(t, admin)
	token := mintPubkeyTokenFromHub(t, hub, myPubHex, amountMillis)
	if res := f.run("receive", token); res.ExitCode != 0 {
		t.Fatalf("receive: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	transferResp := f.mustJSON("transfer", "cash", "--yes")
	resolved, _ := transferResp["target_resolved"].(string)
	if resolved == "" {
		t.Fatalf("transfer to cash: target_resolved is empty — the generated cash secret was never surfaced anywhere, making the transferred funds permanently unspendable: %v", transferResp)
	}

	// Extract the secret and prove it's the real credential: redeem with
	// it, for real, against the same (now-reassigned) connection.
	secret := extractHexSecret(t, resolved)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	verifyClient, err := nipcashclient.Connect(ctx, token)
	if err != nil {
		t.Fatalf("dial the original (now-reassigned) connection: %v", err)
	}
	defer verifyClient.Close()
	hubNWC := dialNWC(t, ctx, hub.PairingUri)
	invoiceTx, err := hubNWC.MakeInvoice(ctx, nip47.MakeInvoiceParams{Amount: int64(amountMillis)})
	if err != nil {
		t.Fatalf("make_invoice: %v", err)
	}
	redeemResult, err := verifyClient.CashRedeem(ctx, nipcash.CashRedeemParams{
		Invoice: invoiceTx.Invoice, Credential: nipcash.BySecret(secret),
	})
	if err != nil {
		t.Fatalf("redeeming with the secret extracted from transfer's own output failed — it wasn't the real credential: %v", err)
	}
	if redeemResult.Preimage == "" {
		t.Error("redeem with the extracted secret returned no preimage")
	}
}

// extractHexSecret pulls the first 64-hex-char substring out of s — used
// to recover a cash secret from a human-readable "resolves to:"-style
// message without hard-coding its exact wording.
func extractHexSecret(t *testing.T, s string) string {
	t.Helper()
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return !strings.ContainsRune("0123456789abcdefABCDEF", r)
	})
	for _, f := range fields {
		if len(f) == 64 {
			return f
		}
	}
	t.Fatalf("no 64-hex-char secret found in %q", s)
	return ""
}
