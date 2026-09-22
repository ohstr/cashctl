//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nip47"
	"github.com/ohstr/nmilat/nipcash"
)

// This file covers parsing/paste edge cases: stray whitespace, a mangled
// gift string, an accidental double-paste. The first two tests guard
// against dial.Sniff trimming for its own classification while the
// untrimmed string still reaches the decoder — fixed in decode.go/
// cash_receive.go by trimming once, up front.

// TestDecode_WhitespacePaddedTokenIsTrimmed is entirely local — no
// admin_api/live server needed.
func TestDecode_WhitespacePaddedTokenIsTrimmed(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")

	pubkeyMode := true
	token := fakeCashToken(t, &pubkeyMode)
	resp := f.mustJSON("decode", "  \n"+token+"  \n")
	if wp, _ := resp["wallet_pubkey"].(string); wp == "" {
		t.Errorf("decode (whitespace-padded token): empty wallet_pubkey — trimming regressed: %v", resp)
	}
}

// TestCashReceive_WhitespacePaddedTokenParsesBeforeNetworkCall is also
// entirely local: the token's own relay is intentionally fake/unreachable
// (fakeCashToken never dials anything real), so a correctly-trimmed
// receive must fail with a NETWORK error (couldn't reach the Cash Hub to
// check the claim) — exit 6 — never an invalid_input parse failure (exit
// 3), which is exactly what the whitespace bug produced before the fix.
func TestCashReceive_WhitespacePaddedTokenParsesBeforeNetworkCall(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")

	pubkeyMode := true
	token := fakeCashToken(t, &pubkeyMode)
	res := f.run("receive", "  \n"+token+"  \n")
	if res.ExitCode == 3 {
		t.Fatalf("receive (whitespace-padded token): exit 3 (invalid_input) — the whitespace-trim fix regressed, this must parse successfully and fail on the network step instead: %s", res.Stderr)
	}
	if res.ExitCode != 6 {
		t.Errorf("receive (whitespace-padded token): exit = %d, want 6 (network — this token's relay is deliberately fake): %s", res.ExitCode, res.Stderr)
	}
	showResp := f.mustJSON("wallet", "show")
	if held, _ := showResp["held_tokens"].([]any); len(held) != 0 {
		t.Errorf("a network-check failure must not save anything: %v", held)
	}
}

// TestDecode_DoubledHashInGiftStringSplitsOnFirstHash documents
// SplitCashSliceString's behavior for a mangled gift string with an
// extra "#": splits on the first one only, treating the rest as one
// opaque secret value.
func TestDecode_DoubledHashInGiftStringSplitsOnFirstHash(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")

	cashMode := false
	token := fakeCashToken(t, &cashMode)
	secret := fakeHex32(t)
	combined := token + "#" + secret + "#unexpected-trailing-garbage"

	resp := f.mustJSON("decode", combined)
	if present, _ := resp["embedded_cash_secret_present"].(bool); !present {
		t.Errorf("decode (doubled '#' gift string): embedded_cash_secret_present = %v, want true", resp["embedded_cash_secret_present"])
	}
	if wp, _ := resp["wallet_pubkey"].(string); wp == "" {
		t.Errorf("decode (doubled '#' gift string): empty wallet_pubkey — the token half before the first '#' must still decode cleanly: %v", resp)
	}
	res := f.run("decode", combined)
	if strings.Contains(res.Stdout, secret) {
		t.Errorf("decode leaked part of the cash secret into stdout: %s", res.Stdout)
	}
}

// TestCashReceive_TruncatedCashSecretSucceedsButIsUnspendable captures
// a protocol-documented gotcha, not a cashctl bug: CheckClaim only
// proves *some* unclaimed cash-mode recipient exists, never that this
// specific secret is valid. A truncated secret still makes `receive`
// report success and save a held entry — but it's unspendable. This test
// proves both halves: receive "succeeds," and a real spend then fails.
func TestCashReceive_TruncatedCashSecretSucceedsButIsUnspendable(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	hub := setUpCashHub(t, admin)
	const amountMillis = uint64(25_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cashClient := dialCash(t, ctx, hub.PairingUri)
	mintResult, err := cashClient.MintCash(ctx, nipcash.MintCashParams{
		Recipients: []nipcash.Allocation{nipcash.Send(nipcash.Anyone(), amountMillis)},
	})
	cancel()
	if err != nil {
		t.Fatalf("mint_cash (cash): %v", err)
	}
	realSecret := mintResult.Recipients[0].CashSecret
	if len(realSecret) < 32 {
		t.Fatalf("real cash secret unexpectedly short (%d chars), can't truncate meaningfully: %q", len(realSecret), realSecret)
	}
	truncatedSecret := realSecret[:len(realSecret)/2]

	f := newFixture(t)
	f.mustJSON("wallet", "init")

	receiveResp := f.mustJSON("receive", mintResult.CashToken+"#"+truncatedSecret)
	entry, _ := receiveResp["entry"].(map[string]any)
	if entry == nil {
		t.Fatalf("receive (truncated cash secret): expected a saved entry (CheckClaim only verifies the allocation exists, not the secret) — got: %v", receiveResp)
	}
	if amt, _ := entry["amount_millis"].(float64); uint64(amt) != amountMillis {
		t.Errorf("receive (truncated cash secret): entry.amount_millis = %v, want %d", entry["amount_millis"], amountMillis)
	}
	if n := heldCount(t, f); n != 1 {
		t.Fatalf("expected 1 held token after receive, got %d", n)
	}

	// The rekey attempt itself must fail, and cashctl must recognize (via
	// NOT_FOUND) that this looks like a wrong secret — see
	// receive_secure.go's isWrongSecretDecline.
	secured, _ := receiveResp["secured"].(map[string]any)
	if status, _ := secured["status"].(string); status != "failed" {
		t.Errorf("receive (truncated cash secret): secured.status = %q, want \"failed\" (the rekey attempt must also fail with the wrong secret): %v", status, secured)
	}
	if likely, _ := secured["likely_wrong_secret"].(bool); !likely {
		t.Errorf("receive (truncated cash secret): secured.likely_wrong_secret = %v, want true — cashctl should recognize a NOT_FOUND decline here as a wrong-secret signal, not a generic failure: %v", secured["likely_wrong_secret"], secured)
	}

	// The decisive check: this "held" money must not actually be
	// spendable, since the stored secret is wrong.
	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer verifyCancel()
	hubNWC := dialNWC(t, verifyCtx, hub.PairingUri)
	invoice, err := hubNWC.MakeInvoice(verifyCtx, nip47.MakeInvoiceParams{Amount: int64(amountMillis)})
	if err != nil {
		t.Fatalf("make_invoice: %v", err)
	}
	redeemRes := f.run("redeem", "--invoice", invoice.Invoice, "--yes")
	if redeemRes.ExitCode == 0 {
		t.Fatalf("redeeming with a truncated cash secret unexpectedly succeeded — either the secret wasn't actually truncated, or the Hub doesn't validate it at spend time: %s", redeemRes.Stdout)
	}
	t.Logf("redeem with truncated secret failed as expected: %s", redeemRes.Stderr)

	// The unspendable entry must still be visible in wallet show — the
	// user needs to be able to SEE something's wrong, not have it vanish.
	if n := heldCount(t, f); n != 1 {
		t.Errorf("expected the (unspendable) held token to remain visible after a failed redeem attempt, got %d held", n)
	}
}

// TestCashReceive_RepastingAlreadyHeldTokenMidSession covers a mundane
// but real client mistake: nervously re-pasting the same message twice,
// or a chat client that renders (and lets you re-copy) the same token
// string a second time. Must be refused as a clean, classified conflict —
// never silently duplicated, never crashing.
func TestCashReceive_RepastingAlreadyHeldTokenMidSession(t *testing.T) {
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
	myPubHex, err := npubToHex(initResp["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	token := mintPubkeyToken(t, admin, myPubHex, 15_000)
	if res := f.run("receive", token); res.ExitCode != 0 {
		t.Fatalf("receive (1st paste): exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	res := f.run("receive", token)
	if res.ExitCode != 5 {
		t.Fatalf("receive (re-paste of the same token): exit = %d, want 5 (conflict)\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if !jsonErrorContains(t, res.Stderr, "conflict", "already") {
		t.Errorf("unexpected error body: %s", res.Stderr)
	}

	if n := heldCount(t, f); n != 1 {
		t.Errorf("a re-pasted duplicate must never be added a second time, got %d held", n)
	}
}
