//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

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

// TestCashReceive_TruncatedCashSecretIsRefused: a Hub answers about a cash-mode
// bill only to its own secret (NIP-CASH §Bearer Items; lokihub #119), so a
// truncated secret is refused at receive, saying the secret does not match, and
// nothing is saved. It used to be received and only fail at spend time.
func TestCashReceive_TruncatedCashSecretIsRefused(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	const amountMillis = uint64(25_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cashClient := dialCash(t, ctx, hub.PairingUri)
	mintResult, err := cashClient.MintCash(ctx, nipcash.MintCashParams{
		Recipients: []nipcash.Allocation{nipcash.Send(newCashRecipient(t), amountMillis)},
	})
	cancel()
	if err != nil {
		t.Fatalf("mint_cash (cash): %v", err)
	}
	realSecret := secretFor(mintResult.Recipients[0])
	truncatedSecret := realSecret[:len(realSecret)/2]

	f := newFixture(t)
	f.mustJSON("wallet", "init")
	res := f.run("receive", mintResult.CashToken+"#"+truncatedSecret)
	if res.ExitCode != 3 || !jsonErrorContains(t, res.Stderr, "invalid_input", "does not match") {
		t.Errorf("receive (truncated cash secret): exit %d, want 3 naming the mismatched secret\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if n := heldCount(t, f); n != 0 {
		t.Errorf("a bill with a wrong secret was saved: %d held", n)
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
