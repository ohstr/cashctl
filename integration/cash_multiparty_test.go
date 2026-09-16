//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nip47"
	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
)

// This file proves cashctl's real-world shape: a "lambda" client never
// transacts against a synthetic target — every send has a second live
// human on the other end who must actually `receive` what landed before
// it's theirs. Every other integration test in this suite sends to
// fakeHex32(t) (nobody's listening) or verifies a bearer secret directly
// via the raw SDK; these tests instead drive two or three fully
// independent cashctl fixtures transacting with each other, hop by hop,
// exactly as a phone-to-phone cash hand-off works in practice.

// recipientTokenFromTransfer returns the cash token string a transfer's
// recipient needs to `receive` what was sent. A split spins off a
// genuinely new wallet for the sent portion (new_wallet_token is
// populated); a full (non-split) transfer may instead reassign the SAME
// underlying wallet connection in place (new_wallet_token empty) — see
// cash_security_test.go's TestCashTransfer_ToBearerTarget_SecretMustBeRecoverable,
// which reconnects to the ORIGINAL token string after a full bearer-target
// transfer. sourceToken is the entry the transfer acted on, used as the
// fallback for the in-place case either way.
func recipientTokenFromTransfer(resp map[string]any, sourceToken string) string {
	if nt, _ := resp["new_wallet_token"].(string); nt != "" {
		return nt
	}
	return sourceToken
}

// heldCount is a small assertion helper: how many tokens f currently holds.
func heldCount(t *testing.T, f *fixture) int {
	t.Helper()
	showResp := f.mustJSON("wallet", "show")
	held, _ := showResp["held_tokens"].([]any)
	return len(held)
}

// TestMultiParty_BearerRegiftChain_AtoBtoC follows one bearer note through
// two real hand-offs: A earns pubkey-mode cash and turns it into a bearer
// gift, B receives and auto-secures it then re-gifts a slice onward to C,
// C receives and redeems it for real. Three independent cashctl
// identities, each only ever seeing what the previous hop physically
// handed them (a token/gift string), the way this would actually happen
// over chat or in person.
//
// This test caught a real cross-repo bug during development: B's
// `receive` step was refused with "no matching recipient," even though
// list_recipients independently confirmed a real, unclaimed bearer
// allocation existed. Root-caused to nipcash/client.CheckClaim
// (nipcash/client/check_claim.go) recomputing isBearer from the TOKEN'S
// OWN embedded identity_required flag — exactly the field NIP-CASH
// §Redemption Metadata calls "a best-effort hint... NOT a live
// guarantee," explicitly warning it goes stale after exactly this
// scenario (a full transfer to bearer-target reassigns the SAME wallet in
// place, per cash_transfer.go's own TransferFromSources comment — the
// token A hands to B still says identity_required:true from its original
// pubkey-mode mint). Fixed by changing CheckClaim's own signature to take
// isBearer as an explicit caller-supplied parameter instead of
// re-deriving it internally, and updating every cashctl call site
// (cash_receive.go, cash_redeem.go, decode.go) to pass its own
// already-corrected determination through.
func TestMultiParty_BearerRegiftChain_AtoBtoC(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	hub := setUpCashHub(t, admin)

	// --- A: earns pubkey-mode cash, then gifts it all as a bearer note ---
	a := newFixture(t)
	aInit := a.mustJSON("wallet", "init")
	aPub, err := npubToHex(aInit["npub"].(string))
	if err != nil {
		t.Fatalf("decode A's local identity npub: %v", err)
	}

	const originalAmount = uint64(60_000)
	original := mintPubkeyTokenFromHub(t, hub, aPub, originalAmount)
	if res := a.run("receive", original); res.ExitCode != 0 {
		t.Fatalf("A receive: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	aTransfer := a.mustJSON("transfer", "bearer-target", "--yes")
	aResolved, _ := aTransfer["target_resolved"].(string)
	if aResolved == "" {
		t.Fatalf("A transfer bearer-target: target_resolved empty, secret never surfaced: %v", aTransfer)
	}
	giftToB := recipientTokenFromTransfer(aTransfer, original) + "#" + extractHexSecret(t, aResolved)
	if heldCount(t, a) != 0 {
		t.Errorf("A should hold nothing after a full (non-split) transfer, got %d", heldCount(t, a))
	}

	// --- B: receives A's gift (auto-secured), re-gifts a slice to C ---
	b := newFixture(t)
	b.mustJSON("wallet", "init")

	bReceive := b.mustJSON("receive", giftToB)
	bEntry, _ := bReceive["entry"].(map[string]any)
	if amt, _ := bEntry["amount_millis"].(float64); uint64(amt) != originalAmount {
		t.Fatalf("B receive: amount_millis = %v, want %d", bEntry["amount_millis"], originalAmount)
	}
	bSecured, _ := bReceive["secured"].(map[string]any)
	if status, _ := bSecured["status"].(string); status != "rekeyed" {
		t.Errorf("B's auto-secure status = %q, want \"rekeyed\" (B held nothing else to merge with): %v", status, bSecured)
	}
	bOwnToken, _ := bEntry["token"].(string)

	const regiftAmount = uint64(20_000)
	bTransfer := b.mustJSON("transfer", "bearer-target", fmt.Sprintf("%d", regiftAmount), "--yes")
	bResolved, _ := bTransfer["target_resolved"].(string)
	if bResolved == "" {
		t.Fatalf("B transfer (regift split) bearer-target: target_resolved empty: %v", bTransfer)
	}
	remaining, _ := bTransfer["remaining_amount_millis"].(float64)
	if uint64(remaining) != originalAmount-regiftAmount {
		t.Errorf("B transfer (regift split): remaining_amount_millis = %v, want %d", bTransfer["remaining_amount_millis"], originalAmount-regiftAmount)
	}
	giftToC := recipientTokenFromTransfer(bTransfer, bOwnToken) + "#" + extractHexSecret(t, bResolved)

	// --- C: receives B's re-gift (auto-secured), redeems it for real ---
	c := newFixture(t)
	c.mustJSON("wallet", "init")

	cReceive := c.mustJSON("receive", giftToC)
	cEntry, _ := cReceive["entry"].(map[string]any)
	if amt, _ := cEntry["amount_millis"].(float64); uint64(amt) != regiftAmount {
		t.Fatalf("C receive: amount_millis = %v, want %d", cEntry["amount_millis"], regiftAmount)
	}
	cSecured, _ := cReceive["secured"].(map[string]any)
	if status, _ := cSecured["status"].(string); status != "rekeyed" {
		t.Errorf("C's auto-secure status = %q, want \"rekeyed\": %v", status, cSecured)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	hubNWC := dialNWC(t, ctx, hub.PairingUri)
	invoice, err := hubNWC.MakeInvoice(ctx, nip47.MakeInvoiceParams{Amount: int64(regiftAmount)})
	if err != nil {
		t.Fatalf("make_invoice for C's redeem: %v", err)
	}
	redeemResp := c.mustJSON("redeem", "--invoice", invoice.Invoice, "--yes")
	if preimage, _ := redeemResp["preimage"].(string); preimage == "" {
		t.Errorf("C redeem: no preimage in response: %v", redeemResp)
	}

	// Final state, across all three real identities: A has nothing left, B
	// kept the remainder, C redeemed away what it received.
	if n := heldCount(t, a); n != 0 {
		t.Errorf("A: expected 0 held tokens at the end, got %d", n)
	}
	if n := heldCount(t, b); n != 1 {
		t.Errorf("B: expected 1 held token (the regift remainder) at the end, got %d", n)
	}
	if n := heldCount(t, c); n != 0 {
		t.Errorf("C: expected 0 held tokens after redeeming, got %d", n)
	}
}

// TestMultiParty_ForwardPortionKeepRemainder_AtoBtoC is the session's
// motivating example: B holds several same-minter cash tokens that
// individually don't cover what it owes a third party — cash selection
// must auto-consolidate a subset first, send exactly the owed amount to a
// REAL second cashctl identity (not a throwaway pubkey nobody's holding),
// and keep the rest as B's own new token. Unlike
// TestCashTransfer_CashSelection_AutoConsolidate (which only checks the
// wire response and a direct SDK list_recipients call), this proves the
// loop actually closes: the recipient can `receive` and spend what
// arrived.
func TestMultiParty_ForwardPortionKeepRemainder_AtoBtoC(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	b := newFixture(t)
	bInit := b.mustJSON("wallet", "init")
	bPub, err := npubToHex(bInit["npub"].(string))
	if err != nil {
		t.Fatalf("decode B's local identity npub: %v", err)
	}

	// Two same-minter tokens that don't individually cover the 45,000
	// B is about to owe C, but sum (60,000) to more than enough.
	hub := setUpCashHub(t, admin)
	const (
		amount1      = uint64(25_000)
		amount2      = uint64(35_000)
		owedToC      = uint64(45_000)
		wantRemainder = amount1 + amount2 - owedToC
	)
	token1, minter1, ok1 := mintSignedPubkeyTokenFromHub(t, b, hub, bPub, amount1)
	if !ok1 {
		t.Skip("skipping: this Hub did not attach a mint signature to the first token")
	}
	token2, minter2, ok2 := mintSignedPubkeyTokenFromHub(t, b, hub, bPub, amount2)
	if !ok2 {
		t.Skip("skipping: this Hub did not attach a mint signature to the second token")
	}
	if minter1 != minter2 {
		t.Skipf("skipping: recovered minter pubkeys differ (%s vs %s)", minter1, minter2)
	}
	if res := b.run("receive", token1); res.ExitCode != 0 {
		t.Fatalf("B receive (token1): exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if res := b.run("receive", token2); res.ExitCode != 0 {
		t.Fatalf("B receive (token2): exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	// A real, separate cashctl identity — C — is who B actually owes.
	c := newFixture(t)
	cInit := c.mustJSON("wallet", "init")
	cPub, err := npubToHex(cInit["npub"].(string))
	if err != nil {
		t.Fatalf("decode C's local identity npub: %v", err)
	}

	transferResp := b.mustJSON("transfer", cPub, fmt.Sprintf("%d", owedToC), "--yes")
	consolidatedFrom, _ := transferResp["consolidated_from"].([]any)
	if len(consolidatedFrom) != 2 {
		t.Fatalf("transfer: consolidated_from = %v, want 2 entries", transferResp["consolidated_from"])
	}
	remaining, _ := transferResp["remaining_amount_millis"].(float64)
	if uint64(remaining) != wantRemainder {
		t.Fatalf("transfer: remaining_amount_millis = %v, want %d (B keeps this, doesn't send it)", transferResp["remaining_amount_millis"], wantRemainder)
	}

	// B's own side: exactly the remainder, nothing else, still B's.
	showB := b.mustJSON("wallet", "show")
	heldB, _ := showB["held_tokens"].([]any)
	if len(heldB) != 1 {
		t.Fatalf("B: expected 1 held token (the remainder) after the partial send, got %d: %v", len(heldB), heldB)
	}
	if amt, _ := heldB[0].(map[string]any)["amount_millis"].(float64); uint64(amt) != wantRemainder {
		t.Errorf("B's remaining held token = %v, want %d", heldB[0].(map[string]any)["amount_millis"], wantRemainder)
	}

	// C's side: nothing arrives automatically — C must actually receive
	// what B sent, the same as any real recipient would.
	if n := heldCount(t, c); n != 0 {
		t.Fatalf("C: expected 0 held tokens before receiving anything, got %d", n)
	}
	recipientToken := recipientTokenFromTransfer(transferResp, token1)
	cReceive := c.mustJSON("receive", recipientToken)
	cEntry, _ := cReceive["entry"].(map[string]any)
	if amt, _ := cEntry["amount_millis"].(float64); uint64(amt) != owedToC {
		t.Fatalf("C receive: amount_millis = %v, want %d", cEntry["amount_millis"], owedToC)
	}

	// Prove it's real, spendable money: C redeems it.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	hubNWC := dialNWC(t, ctx, hub.PairingUri)
	invoice, err := hubNWC.MakeInvoice(ctx, nip47.MakeInvoiceParams{Amount: int64(owedToC)})
	if err != nil {
		t.Fatalf("make_invoice for C's redeem: %v", err)
	}
	redeemResp := c.mustJSON("redeem", "--invoice", invoice.Invoice, "--yes")
	if preimage, _ := redeemResp["preimage"].(string); preimage == "" {
		t.Errorf("C redeem: no preimage in response: %v", redeemResp)
	}

	// Independently verify server-side: both of B's original tokens are
	// now fully claimed, not just reported as such by cashctl.
	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer verifyCancel()
	for _, tok := range []string{token1, token2} {
		client, err := nipcashclient.Connect(verifyCtx, tok)
		if err != nil {
			t.Fatalf("dial original token %q: %v", tok, err)
		}
		recipients, err := client.ListRecipients(verifyCtx)
		client.Close()
		if err != nil {
			t.Fatalf("list_recipients on original token %q: %v", tok, err)
		}
		var unclaimed uint64
		for _, r := range recipients.Recipients {
			if !r.Claimed {
				unclaimed += r.AmountMillis
			}
		}
		if unclaimed != 0 {
			t.Errorf("original token %q still has %d unclaimed after consolidate+transfer", tok, unclaimed)
		}
	}
}

// TestMultiParty_MixedIdentityModeCashSelection confirms cash selection's
// same-minter grouping (internal/ledger.GroupableForConsolidation) holds up
// live with a real recipient on the other end: B holds one bearer-mode
// note AND two pubkey-mode same-minter tokens. Paying C an amount only the
// pubkey-mode pair can reach must consolidate exactly those two — never
// sweeping in the bearer note, even though (sized wrong) it could
// otherwise look like an easy single-token cover.
func TestMultiParty_MixedIdentityModeCashSelection(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	b := newFixture(t)
	bInit := b.mustJSON("wallet", "init")
	bPub, err := npubToHex(bInit["npub"].(string))
	if err != nil {
		t.Fatalf("decode B's local identity npub: %v", err)
	}

	hub := setUpCashHub(t, admin)

	// A bearer note smaller than the target amount, so it can never be
	// picked as a single-token best-fit cover (case 2) either — it must be
	// excluded purely on identity-mode grounds, not because it's too small
	// to matter.
	const bearerAmount = uint64(15_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cashClient := dialCash(t, ctx, hub.PairingUri)
	mintResult, err := cashClient.MintCash(ctx, nipcash.MintCashParams{
		Recipients: []nipcash.Allocation{nipcash.Send(nipcash.Anyone(), bearerAmount)},
	})
	cancel()
	if err != nil {
		t.Fatalf("mint_cash (bearer): %v", err)
	}
	if res := b.run("receive", mintResult.CashToken+"#"+mintResult.Recipients[0].BearerSecret); res.ExitCode != 0 {
		t.Fatalf("B receive (bearer): exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	const (
		amount1 = uint64(20_000)
		amount2 = uint64(30_000)
		target  = amount1 + amount2 // exact sum: a full transfer, no remainder
	)
	token1, minter1, ok1 := mintSignedPubkeyTokenFromHub(t, b, hub, bPub, amount1)
	if !ok1 {
		t.Skip("skipping: this Hub did not attach a mint signature to the first pubkey token")
	}
	token2, minter2, ok2 := mintSignedPubkeyTokenFromHub(t, b, hub, bPub, amount2)
	if !ok2 {
		t.Skip("skipping: this Hub did not attach a mint signature to the second pubkey token")
	}
	if minter1 != minter2 {
		t.Skipf("skipping: recovered minter pubkeys differ (%s vs %s)", minter1, minter2)
	}
	if res := b.run("receive", token1); res.ExitCode != 0 {
		t.Fatalf("B receive (token1): exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if res := b.run("receive", token2); res.ExitCode != 0 {
		t.Fatalf("B receive (token2): exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	c := newFixture(t)
	cInit := c.mustJSON("wallet", "init")
	cPub, err := npubToHex(cInit["npub"].(string))
	if err != nil {
		t.Fatalf("decode C's local identity npub: %v", err)
	}

	transferResp := b.mustJSON("transfer", cPub, fmt.Sprintf("%d", target), "--yes")
	consolidatedFrom, _ := transferResp["consolidated_from"].([]any)
	if len(consolidatedFrom) != 2 {
		t.Fatalf("transfer: consolidated_from = %v, want exactly 2 (the two pubkey-mode tokens, not the bearer note)", transferResp["consolidated_from"])
	}
	if remaining := transferResp["remaining_amount_millis"]; remaining != nil {
		t.Errorf("transfer: remaining_amount_millis = %v, want absent/nil (sum matched target exactly)", remaining)
	}

	// B's own side: only the untouched bearer note remains, at its
	// original amount and still bearer-mode.
	showB := b.mustJSON("wallet", "show")
	heldB, _ := showB["held_tokens"].([]any)
	if len(heldB) != 1 {
		t.Fatalf("B: expected 1 held token (the untouched bearer note), got %d: %v", len(heldB), heldB)
	}
	remainingEntry, _ := heldB[0].(map[string]any)
	if amt, _ := remainingEntry["amount_millis"].(float64); uint64(amt) != bearerAmount {
		t.Errorf("B's remaining token amount = %v, want %d (the bearer note, untouched)", remainingEntry["amount_millis"], bearerAmount)
	}

	recipientToken := recipientTokenFromTransfer(transferResp, token1)
	cReceive := c.mustJSON("receive", recipientToken)
	cEntry, _ := cReceive["entry"].(map[string]any)
	if amt, _ := cEntry["amount_millis"].(float64); uint64(amt) != target {
		t.Fatalf("C receive: amount_millis = %v, want %d", cEntry["amount_millis"], target)
	}
}

// TestMultiParty_RoundTrip_AtoB_BtoA closes the loop: A sends its entire
// holding to B, B sends a portion back to A, keeping the rest. Confirms
// cashctl handles being on BOTH ends of a live exchange within one
// session (receive-then-immediately-act-as-sender), and that value is
// conserved exactly across two real identities and two hops.
func TestMultiParty_RoundTrip_AtoB_BtoA(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	a := newFixture(t)
	aInit := a.mustJSON("wallet", "init")
	aPub, err := npubToHex(aInit["npub"].(string))
	if err != nil {
		t.Fatalf("decode A's local identity npub: %v", err)
	}
	b := newFixture(t)
	bInit := b.mustJSON("wallet", "init")
	bPub, err := npubToHex(bInit["npub"].(string))
	if err != nil {
		t.Fatalf("decode B's local identity npub: %v", err)
	}

	const totalAmount = uint64(40_000)
	hub := setUpCashHub(t, admin)
	original := mintPubkeyTokenFromHub(t, hub, aPub, totalAmount)
	if res := a.run("receive", original); res.ExitCode != 0 {
		t.Fatalf("A receive: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	// A -> B, in full (no amount given: transfers everything held).
	aTransfer := a.mustJSON("transfer", bPub, "--yes")
	if remaining := aTransfer["remaining_amount_millis"]; remaining != nil {
		t.Errorf("A transfer (full): remaining_amount_millis = %v, want absent (nothing kept back)", remaining)
	}
	bReceive := b.mustJSON("receive", recipientTokenFromTransfer(aTransfer, original))
	bEntry, _ := bReceive["entry"].(map[string]any)
	if amt, _ := bEntry["amount_millis"].(float64); uint64(amt) != totalAmount {
		t.Fatalf("B receive: amount_millis = %v, want %d", bEntry["amount_millis"], totalAmount)
	}
	bOwnToken, _ := bEntry["token"].(string)

	// B -> A, partial: keeps the rest.
	const sentBack = uint64(15_000)
	bTransfer := b.mustJSON("transfer", aPub, fmt.Sprintf("%d", sentBack), "--yes")
	remaining, _ := bTransfer["remaining_amount_millis"].(float64)
	if uint64(remaining) != totalAmount-sentBack {
		t.Fatalf("B transfer (partial back to A): remaining_amount_millis = %v, want %d", bTransfer["remaining_amount_millis"], totalAmount-sentBack)
	}
	aReceive2 := a.mustJSON("receive", recipientTokenFromTransfer(bTransfer, bOwnToken))
	aEntry2, _ := aReceive2["entry"].(map[string]any)
	if amt, _ := aEntry2["amount_millis"].(float64); uint64(amt) != sentBack {
		t.Fatalf("A receive (round trip): amount_millis = %v, want %d", aEntry2["amount_millis"], sentBack)
	}

	// Final state: value is conserved exactly across both identities.
	showA := a.mustJSON("wallet", "show")
	heldA, _ := showA["held_tokens"].([]any)
	if len(heldA) != 1 {
		t.Fatalf("A: expected 1 held token at the end, got %d", len(heldA))
	}
	if amt, _ := heldA[0].(map[string]any)["amount_millis"].(float64); uint64(amt) != sentBack {
		t.Errorf("A's final held amount = %v, want %d", heldA[0].(map[string]any)["amount_millis"], sentBack)
	}
	showB := b.mustJSON("wallet", "show")
	heldB, _ := showB["held_tokens"].([]any)
	if len(heldB) != 1 {
		t.Fatalf("B: expected 1 held token at the end, got %d", len(heldB))
	}
	if amt, _ := heldB[0].(map[string]any)["amount_millis"].(float64); uint64(amt) != totalAmount-sentBack {
		t.Errorf("B's final held amount = %v, want %d", heldB[0].(map[string]any)["amount_millis"], totalAmount-sentBack)
	}
}
