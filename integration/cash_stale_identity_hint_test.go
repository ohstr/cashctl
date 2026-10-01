//go:build integration

package integration

// Closing pass: a bill whose identity_required hint has gone stale must still be
// receivable.
//
// identity_required is a hint carried in the token itself, and NIP-CASH §Redemption
// Metadata already says it is best-effort. It goes stale in one direction that costs
// the holder the bill: cash_transfer onto a pubkey identity reassigns a wallet IN
// PLACE, keeping the very same token string — that is the documented behaviour
// cashctl's own "a transferred token may legitimately return to a previous holder"
// ledger check relies on. So after such a reassignment the token still says
// identity_required: false while the Hub now requires identity.
//
// `cashctl receive` read that hint, concluded "cash-mode with no embedded secret,
// nothing to act on", and returned without saving. There was no other way in, because
// every later operation needs a ledger entry. The bill was unreceivable by the person
// who rightfully held it.
//
// The fix asks the Hub before giving up. The live answer discriminates exactly what
// the hint cannot: a genuinely cash-mode bill has no credential to offer without its
// secret, so the call is refused, while a reassigned one resolves against the local
// identity.
//
// Needs a live Hub: the discriminator IS the network call, so there is nothing here a
// unit test can stand in for. The guard itself is exercised offline by the unit suite
// through validateQuotedAmounts' siblings; this is the half that needs a real
// reassignment.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
)

func TestReceive_StaleCashModeHint_AfterReassignment_StillReceivable(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)

	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}

	const amountMillis = 40_000
	gift := mintCashGift(t, hub, amountMillis)
	token, secret, found := strings.Cut(gift, "#")
	if !found || secret == "" {
		t.Fatalf("mintCashGift returned no secret: %q", gift)
	}

	// Reassign the cash-mode slice onto this fixture's own identity. A cash-mode
	// source is single-recipient by construction, so this is an in-place transfer and
	// `token` is unchanged — still carrying identity_required: false.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := nipcashclient.Connect(ctx, token)
	if err != nil {
		t.Fatalf("dial the minted token: %v", err)
	}
	defer client.Close()
	if _, err := client.CashTransfer(ctx, nipcash.CashTransferParams{
		Credential:    nipcash.BySecret(secret),
		To:            nipcash.Pubkey(pub),
		CurrentAmount: amountMillis,
	}); err != nil {
		t.Fatalf("reassign the cash slice onto the local identity: %v", err)
	}

	// The token's own metadata must still claim cash-mode, or this test is not
	// reproducing the staleness it is named for.
	decoded, err := nipcash.Decode(token)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.IdentityRequired == nil || *decoded.IdentityRequired {
		t.Skipf("this Hub's token says identity_required=%v after reassignment, so the hint "+
			"is no longer stale and there is nothing to guard — if minting started stamping "+
			"this differently, retire this test rather than weakening it", decoded.IdentityRequired)
	}

	// Receive the ORIGINAL token, with no secret appended. Pre-fix this printed
	// "No spending secret" and exited 0 having saved nothing.
	res := f.run("receive", token, "--json", "--yes")
	if res.ExitCode != 0 {
		t.Fatalf("receive exited %d on a reassigned bill\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if strings.Contains(res.Stdout, "no_embedded_secret") {
		t.Fatalf("receive took the no-secret dead end on a bill this identity can claim — "+
			"the stale identity_required hint was trusted over the Hub's live answer\nstdout: %s", res.Stdout)
	}

	// Ledger truth, not the command's own report: the bill is held, and held as
	// IDENTITY-BOUND — the Hub's live answer — rather than as the cash-mode thing its
	// own metadata still claims. That second half is the point: a fix that received
	// the bill but recorded it from the stale hint would have left every later
	// operation on it reaching for a secret that does not exist.
	shown := f.mustJSON("wallet", "show", "--json")
	held, _ := shown["held_tokens"].([]any)
	if len(held) != 1 {
		t.Fatalf("wallet show has %d held tokens after a successful receive, want 1: %v", len(held), shown)
	}
	entry, _ := held[0].(map[string]any)
	if entry["token"] != token {
		t.Errorf("held token = %v, want the original %q", entry["token"], token)
	}
	if req, ok := entry["identity_required"].(bool); !ok || !req {
		t.Errorf("identity_required = %v in the ledger, want true — the save must carry "+
			"CheckClaim's live answer, not the token's stale hint", entry["identity_required"])
	}
	if amount, ok := entry["amount_millis"].(float64); !ok || uint64(amount) != amountMillis {
		t.Errorf("amount_millis = %v, want %d", entry["amount_millis"], amountMillis)
	}
	if verified, ok := entry["verified"].(bool); !ok || !verified {
		t.Errorf("verified = %v, want true", entry["verified"])
	}
}
