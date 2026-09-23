//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nipcash"
)

// mintSignedPubkeyTokenFromHub mints a pubkey-mode token from hub with
// MintSignature requested, and returns the token alongside decode's own
// report of whether a signature actually got attached — minting a
// provenance signature is best-effort server-side (NIP-CASH §Mint
// Provenance), so a caller needing one for a same-minter grouping
// scenario must check ok before relying on it, not assume the request
// was honored.
func mintSignedPubkeyTokenFromHub(t *testing.T, f *fixture, hub adminCreateAppResponse, pubkeyHex string, amountMillis uint64) (token string, minterPubkeyHex string, ok bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cashClient := dialCash(t, ctx, hub.PairingUri)
	result, err := cashClient.MintCash(ctx, nipcash.MintCashParams{
		Recipients:    []nipcash.Allocation{nipcash.Send(nipcash.Pubkey(pubkeyHex), amountMillis)},
		MintSignature: true,
	})
	if err != nil {
		t.Fatalf("mint_cash (MintSignature: true): %v", err)
	}
	decodeResp := f.mustJSON("decode", result.CashToken)
	minter, valid := decodeResp["minter_pubkey"].(string)
	return result.CashToken, minter, valid && minter != ""
}

// TestCashTransfer_CashSelection_ExactMatch covers docs/ux-review.md Part
// 2's case 1: a single held token's amount matches the requested transfer
// amount exactly, so it's sent whole (no split, no remainder) rather than
// prompting for which token to use.
func TestCashTransfer_CashSelection_ExactMatch(t *testing.T) {
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

	const amountMillis = uint64(45_000)
	token := mintPubkeyToken(t, admin, myPubHex, amountMillis)
	if res := f.run("receive", token); res.ExitCode != 0 {
		t.Fatalf("receive: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	targetHex := fakeHex32(t)
	// No --token: cash selection must pick the one held entry itself.
	resp := f.mustJSON("transfer", targetHex, lokiArg(int64(amountMillis)), "--yes")
	if resp["remaining_amount_millis"] != nil {
		t.Errorf("transfer (exact match): expected a full transfer with no remainder, got remaining_amount_millis = %v", resp["remaining_amount_millis"])
	}
	if cf, _ := resp["consolidated_from"].([]any); len(cf) != 0 {
		t.Errorf("transfer (exact match): expected no auto-consolidation, got consolidated_from = %v", resp["consolidated_from"])
	}
}

// TestCashTransfer_CashSelection_BestFitSplit covers case 2: several held
// tokens cover the requested amount, and cash selection must pick the
// smallest one that does (best-fit) rather than the largest available —
// verified by checking which token ends up with a remainder afterward.
func TestCashTransfer_CashSelection_BestFitSplit(t *testing.T) {
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

	const (
		bigAmount    = uint64(90_000)
		smallAmount  = uint64(30_000)
		targetAmount = uint64(20_000)
	)
	bigToken := mintPubkeyToken(t, admin, myPubHex, bigAmount)
	smallToken := mintPubkeyToken(t, admin, myPubHex, smallAmount)
	if res := f.run("receive", bigToken); res.ExitCode != 0 {
		t.Fatalf("receive (big): exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if res := f.run("receive", smallToken); res.ExitCode != 0 {
		t.Fatalf("receive (small): exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	targetHex := fakeHex32(t)
	resp := f.mustJSON("transfer", targetHex, lokiArg(int64(targetAmount)), "--yes")
	remaining, _ := resp["remaining_amount_millis"].(float64)
	if uint64(remaining) != smallAmount-targetAmount {
		t.Fatalf("transfer (best-fit split): remaining_amount_millis = %v, want %d (the smaller covering token's remainder, not the bigger one's)", resp["remaining_amount_millis"], smallAmount-targetAmount)
	}

	// The bigger token must be untouched: still held, at its original amount.
	showResp := f.mustJSON("wallet", "show")
	held, _ := showResp["held_tokens"].([]any)
	foundUntouchedBig := false
	for _, h := range held {
		e, _ := h.(map[string]any)
		if tok, _ := e["token"].(string); tok == bigToken {
			foundUntouchedBig = true
			if amt, _ := e["amount_millis"].(float64); uint64(amt) != bigAmount {
				t.Errorf("the larger token's amount changed: %v, want %d (best-fit must never touch it)", e["amount_millis"], bigAmount)
			}
		}
	}
	if !foundUntouchedBig {
		t.Errorf("the larger, non-covering-needed token is no longer held at all: %v", held)
	}
}

// TestCashTransfer_CashSelection_Fragmented covers case 4: the held total
// covers the requested amount, but it's split across two different
// minters and no single minter's tokens sum to it, so transfer must
// refuse outright with a usage error naming the shortfall, rather than
// silently sending as multiple transfers to different minters. This needs
// two distinct hubs (two distinct minters) — a single minter with an
// insufficient total is a genuinely different diagnosis (plain overdraft,
// ledger.InsufficientFundsError/invalid_input, not fragmentation) that
// cash_transfer.go deliberately no longer conflates with this one (see its
// own comment on the split).
func TestCashTransfer_CashSelection_Fragmented(t *testing.T) {
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

	hub1 := setUpCashHub(t, admin)
	hub2 := setUpCashHub(t, admin)
	const amountH1, amountH2 = uint64(300_000), uint64(300_000) // 300 loki each — neither alone covers the 500 loki target
	const target = amountH1 + amountH2 - 100_000                // 500,000: covered by the 600 loki total, but not by either hub alone

	tokenH1 := mintPubkeyTokenFromHub(t, hub1, myPubHex, amountH1)
	tokenH2 := mintPubkeyTokenFromHub(t, hub2, myPubHex, amountH2)
	if res := f.run("receive", tokenH1); res.ExitCode != 0 {
		t.Fatalf("receive (hub1): exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if res := f.run("receive", tokenH2); res.ExitCode != 0 {
		t.Fatalf("receive (hub2): exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	res := f.run("transfer", fakeHex32(t), lokiArg(int64(target)), "--yes")
	if res.ExitCode != 2 {
		t.Fatalf("transfer (fragmented across 2 hubs, 600 loki held): exit = %d, want 2 (usage)\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if !jsonErrorContains(t, res.Stderr, "usage", "fragmented") {
		t.Errorf("unexpected error body: %s", res.Stderr)
	}
	// The bug found auditing this: the shortfall used to print as two bare,
	// unlabeled integers ("you hold 600000 total... the 500000 you're
	// sending") — indistinguishable from any other pair of numbers, not
	// obviously an amount at all. Must now read as real loki amounts.
	if !jsonErrorContains(t, res.Stderr, "usage", "600 loki total") {
		t.Errorf("error doesn't name the held total in loki: %s", res.Stderr)
	}
	if !jsonErrorContains(t, res.Stderr, "usage", "500 loki you're sending") {
		t.Errorf("error doesn't name the requested amount in loki: %s", res.Stderr)
	}

	// Refusing must not touch anything held.
	if n := heldCount(t, f); n != 2 {
		t.Errorf("a refused transfer must leave held tokens untouched, got %d", n)
	}
}

// TestCashTransfer_CashSelection_AutoConsolidate covers case 3: no single
// held token covers the requested amount, but two from the same minter,
// summed, do — cash selection must consolidate them first, then transfer
// from the result, both under one confirmation. Needs the Hub to actually
// attach mint-provenance signatures (best-effort — see
// mintSignedPubkeyTokenFromHub); skips cleanly if it doesn't, since that's
// a Hub-configuration question, not a cashctl behavior this test can
// force.
func TestCashTransfer_CashSelection_AutoConsolidate(t *testing.T) {
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

	// Both sources must be children of the same Cash Hub (NIP-CASH
	// §Consolidating Tokens) — mint both from one shared hub.
	hub := setUpCashHub(t, admin)
	const (
		amount1      = uint64(20_000)
		amount2      = uint64(30_000)
		targetAmount = uint64(40_000) // covered by 1+2 (50000), not by either alone
	)
	token1, minter1, ok1 := mintSignedPubkeyTokenFromHub(t, f, hub, myPubHex, amount1)
	if !ok1 {
		t.Skip("skipping: this Hub did not attach a mint signature (best-effort — see NIP-CASH §Mint Provenance) — same-minter grouping can't be exercised without one")
	}
	token2, minter2, ok2 := mintSignedPubkeyTokenFromHub(t, f, hub, myPubHex, amount2)
	if !ok2 {
		t.Skip("skipping: this Hub did not attach a mint signature on the second token")
	}
	if minter1 != minter2 {
		t.Skipf("skipping: the two tokens' recovered minter pubkeys differ (%s vs %s) — can't exercise same-minter grouping", minter1, minter2)
	}

	if res := f.run("receive", token1); res.ExitCode != 0 {
		t.Fatalf("receive (token1): exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if res := f.run("receive", token2); res.ExitCode != 0 {
		t.Fatalf("receive (token2): exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	targetHex := fakeHex32(t)
	resp := f.mustJSON("transfer", targetHex, lokiArg(int64(targetAmount)), "--yes")
	consolidatedFrom, _ := resp["consolidated_from"].([]any)
	if len(consolidatedFrom) != 2 {
		t.Fatalf("transfer (auto-consolidate): consolidated_from = %v, want 2 entries", resp["consolidated_from"])
	}
	remaining, _ := resp["remaining_amount_millis"].(float64)
	if uint64(remaining) != amount1+amount2-targetAmount {
		t.Errorf("transfer (auto-consolidate): remaining_amount_millis = %v, want %d", resp["remaining_amount_millis"], amount1+amount2-targetAmount)
	}

	// Independently verify server-side: both original bills were
	// consolidated away, so neither exists any more.
	for _, tok := range []string{token1, token2} {
		requireBillSpentAway(t, tok, "consolidated source")
	}
}
