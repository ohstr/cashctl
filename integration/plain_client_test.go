//go:build integration

// What a NORMAL client sees.
//
// Every other verification in this suite now reads server truth through the
// admin API, because a holder's own connection can no longer answer "did the
// Hub really do this" — bill methods are served over the private transport,
// which authorizes and scopes per recipient. That is the correct channel for an
// assertion about the Hub's state, but it tests the Hub as its OPERATOR sees
// it.
//
// Nobody holding a lokicash is the operator. This file is the other half: a
// plain nmilat client, a token, and no admin credentials at all — exactly what
// a real recipient runs — asserting on what such a client can and cannot learn.
// The scoping rules are a privacy guarantee, and a guarantee only counts if it
// is observed from the outside.
package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/ohstr/nmilat/utils"
)

// plainClient dials a bill the way a recipient's own software does: the token,
// nothing else. No admin client, no hub pairing URI, no operator privilege.
func plainClient(t *testing.T, ctx context.Context, token string) *nipcashclient.Client {
	t.Helper()
	c, err := nipcashclient.Connect(ctx, token)
	if err != nil {
		t.Fatalf("plain client could not dial the bill: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// newIdentity returns a keypair this test controls both halves of, so it can
// both address a slice to it and sign as it.
func newIdentity(t *testing.T) (privHex, pubHex string) {
	t.Helper()
	privHex = fakeHex32(t)
	pubHex, err := utils.GetPublicKey(privHex)
	if err != nil {
		t.Fatalf("derive pubkey: %v", err)
	}
	return privHex, pubHex
}

// mintTwoRecipientBill mints one bill split between two pubkey identities and
// returns its token. Two recipients is the minimum shape in which "you see your
// own slice" is distinguishable from "you see everything".
func mintTwoRecipientBill(t *testing.T, hub adminCreateAppResponse, pubA, pubB string, amountA, amountB uint64) string {
	t.Helper()
	return retryEphemeralNWC(t, "mint_cash", func(ctx context.Context) (string, error) {
		result, err := dialCash(t, ctx, hub.PairingUri).MintCash(ctx, nipcash.MintCashParams{
			Recipients: []nipcash.Allocation{
				nipcash.Send(nipcash.Pubkey(pubA), amountA),
				nipcash.Send(nipcash.Pubkey(pubB), amountB),
			},
		})
		if err != nil {
			return "", err
		}
		return result.CashToken, nil
	})
}

func plainClientHub(t *testing.T) (*adminClient, adminCreateAppResponse) {
	t.Helper()
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}
	return admin, setUpCashHub(t, admin)
}

// TestPlainClient_DefaultScopeShowsOnlyOwnSlice is the privacy guarantee stated
// from the outside: a recipient who asks cash_status without naming a scope
// learns their own slice and nothing about who else is on the bill.
//
// The default matters more than the explicit case. A client that says nothing
// is the common client, and before scoping existed it received the entire
// roster — every co-recipient's pubkey and amount — simply for holding a token
// that happened to name it. The controller now defaults to "mine" for exactly
// that reason (nip47/controllers/cash_status_controller.go's
// resolveCashStatusScope: "a caller who says nothing learns nothing about their
// co-recipients"), and this asserts it over the wire rather than in a unit test
// that could agree with a broken server.
func TestPlainClient_DefaultScopeShowsOnlyOwnSlice(t *testing.T) {
	_, hub := plainClientHub(t)

	privA, pubA := newIdentity(t)
	_, pubB := newIdentity(t)
	const amountA, amountB = uint64(70_000), uint64(30_000)
	token := mintTwoRecipientBill(t, hub, pubA, pubB, amountA, amountB)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c := plainClient(t, ctx, token)

	// Deliberately the EMPTY scope, not ScopeMine. Asking for "mine" explicitly
	// would test the explicit case and leave the default — the one a client that
	// says nothing gets, and the one that used to disclose the whole roster —
	// unexercised. nmilat forwards an empty scope as absent (IsValidCashStatusScope
	// admits ""), so the server resolves it.
	res, err := c.CashStatus(ctx, nipcash.BySigning(privA), "")
	if err != nil {
		t.Fatalf("a recipient must be able to read their own slice: %v", err)
	}
	if len(res.Recipients) != 1 {
		t.Fatalf("scope=mine returned %d rows, want exactly 1 (the caller's own): %+v", len(res.Recipients), res.Recipients)
	}
	got := res.Recipients[0]
	if got.IdentityValue != pubA {
		t.Errorf("scope=mine returned someone else's row: identity_value = %q, want the caller's %q", got.IdentityValue, pubA)
	}
	if got.AmountMillis != amountA {
		t.Errorf("own slice amount = %d, want %d", got.AmountMillis, amountA)
	}
	for _, r := range res.Recipients {
		if r.IdentityValue == pubB {
			t.Errorf("the default scope leaked a co-recipient (%q) to a plain client", pubB)
		}
	}

	// The other half of the same guarantee: scope=all is how a recipient opts IN
	// to seeing their co-recipients, and it must still work. Scoping that cannot
	// be widened on request is not scoping, it is a removed feature — and
	// `cashctl cash list-recipients` asks for exactly this.
	all, err := c.CashStatus(ctx, nipcash.BySigning(privA), nipcash.ScopeAll)
	if err != nil {
		t.Fatalf("scope=all from an authorized recipient: %v", err)
	}
	seen := map[string]uint64{}
	for _, r := range all.Recipients {
		seen[r.IdentityValue] = r.AmountMillis
	}
	if seen[pubA] != amountA || seen[pubB] != amountB {
		t.Errorf("scope=all should show both slices (%s=%d, %s=%d), got %+v", pubA, amountA, pubB, amountB, all.Recipients)
	}
}

// TestPlainClient_NonRecipientLearnsNothing is the sharper half, and the one
// worth having: holding the token is NOT the same as holding a slice.
//
// The token is a connection — every recipient of a bill shares it, and it can
// be forwarded, logged or leaked. Authorization is the per-item proof, so a
// caller who signs correctly but holds no slice must come away with no rows.
// Not an error naming the bill's contents, not a full roster, not a hint that
// some other identity is on it: nothing.
//
// claimsForCaller returns an empty set on no match rather than falling back to
// the full roster, precisely so this cannot become a disclosure. That fallback
// is the kind of "be helpful on a miss" branch that reappears easily, and no
// unit test of a handler proves it is absent from the deployed path.
func TestPlainClient_NonRecipientLearnsNothing(t *testing.T) {
	_, hub := plainClientHub(t)

	_, pubA := newIdentity(t)
	_, pubB := newIdentity(t)
	privStranger, _ := newIdentity(t)
	token := mintTwoRecipientBill(t, hub, pubA, pubB, 70_000, 30_000)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := plainClient(t, ctx, token).CashStatus(ctx, nipcash.BySigning(privStranger), nipcash.ScopeMine)

	// A DEFINITE refusal, not silence — and that is new. This caller proved they
	// hold the bill (their item carried a valid kind-23193 bill proof, signed with
	// the token's connection secret), so telling them they hold no SLICE discloses
	// nothing further: they could have learned as much by trying to spend it.
	//
	// It used to be an information-free omission, which was correct while possession
	// was unprovable and cost real information: "this bill is not addressed to you"
	// was indistinguishable from "the Hub is unreachable", and was reported as
	// retryable, so a client retried forever a token that would never be theirs.
	if err == nil {
		if res != nil && len(res.Recipients) > 0 {
			t.Fatalf("a caller holding the token but no slice received %d roster row(s) — the token is a connection, not an entitlement: %+v",
				len(res.Recipients), res.Recipients)
		}
		t.Fatalf("expected a definite refusal for a holder with no slice, got a successful empty answer: %+v", res)
	}
	if !strings.Contains(err.Error(), "NOT_FOUND") {
		t.Errorf("refusal should be a definite NOT_FOUND, got %v", err)
	}
	if strings.Contains(err.Error(), "no answer") {
		t.Errorf("a holder must not be met with an omission any more: %v", err)
	}
	// And it must not name anyone else on the bill.
	if strings.Contains(err.Error(), pubA) || strings.Contains(err.Error(), pubB) {
		t.Errorf("the refusal leaked a co-recipient's identity: %v", err)
	}
}

// TestSpentBill_HolderGetsATombstone is the three-outcome answer model, end to end.
//
// A destroyed bill used to be pure silence on this transport, and silence cannot be
// told apart from a Hub that is slow or unreachable — which forces every client to
// pick a wrong default: treat it as retryable and a genuinely spent bill retries
// forever, or treat it as gone and an outage tells someone their funds are lost.
//
// The Hub can answer now because the item proves possession. The bill is destroyed,
// so there are no slices and no identity left to check — the kind-23193 bill proof,
// verified against a pairing key RE-DERIVED from the archived bill's app id, is the
// whole authorization. Nothing secret is retained to make that work.
func TestSpentBill_HolderGetsATombstone(t *testing.T) {
	admin, hub := plainClientHub(t)

	f := newFixture(t)
	initResp := f.mustJSON("wallet", "init")
	myPubHex, err := npubToHex(initResp["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	const amountMillis = uint64(120_000)
	token := mintPubkeyTokenFromHub(t, hub, myPubHex, amountMillis)
	if res := f.run("receive", token); res.ExitCode != 0 {
		t.Fatalf("receive: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	// Redeem the whole bill so nothing is left on it and the Hub destroys it.
	//
	// Deliberately NOT a full transfer to a pubkey target: that is an IN-PLACE
	// identity reassignment (NIP-CASH §Transferring and Splitting a Slice), so the
	// bill survives. Only emptying it destroys it — the first draft of this test got
	// that wrong and the Hub correctly still held a live slice.
	f.mustJSON("connect", "add", "dest", hub.PairingUri)
	if res := f.run("redeem", "--all", "--into", "dest", "--json", "--yes"); res.ExitCode != 0 {
		t.Fatalf("redeem: exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}

	// Server truth first, so the client-side assertion below is about a bill that is
	// definitely gone rather than about a timing window.
	requireBillSpentAway(t, admin, hub.ID, token, "the redeemed bill")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Dialled with the TOKEN, so the bill proof is signed with its connection
	// secret. The slice credential is irrelevant — there are no slices left.
	c, dialErr := nipcashclient.Connect(ctx, token)
	if dialErr != nil {
		t.Fatalf("a destroyed bill's token must still dial (it is a connection, not a claim): %v", dialErr)
	}
	defer c.Close()

	privAny, _ := newIdentity(t)
	res, err := c.CashStatus(ctx, nipcash.BySigning(privAny), nipcash.ScopeMine)
	if err != nil {
		t.Fatalf("the holder of a destroyed bill must get a definitive answer, not %v", err)
	}
	if !res.IsSpent() {
		t.Fatalf("want a spent tombstone, got %+v", res)
	}
	if res.RetainedUntil == nil {
		t.Error("a tombstone must say when the Hub will stop answering, so a client knows this answer is temporary")
	} else if *res.RetainedUntil <= time.Now().Unix() {
		t.Errorf("retained_until is already in the past (%d); the Hub answered outside its own window", *res.RetainedUntil)
	}
	if len(res.Recipients) != 0 {
		t.Errorf("a destroyed bill must disclose no roster: %+v", res.Recipients)
	}
}

// TestSpentBill_NonHolderGetsSilence is the gate that keeps the tombstone above from
// becoming an existence oracle for every bill this Hub ever minted.
//
// Same destroyed bill, same request — but signed by someone who does not hold the
// token. They must learn nothing, which on this transport means an omission.
func TestSpentBill_NonHolderGetsSilence(t *testing.T) {
	admin, hub := plainClientHub(t)

	f := newFixture(t)
	initResp := f.mustJSON("wallet", "init")
	myPubHex, err := npubToHex(initResp["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	token := mintPubkeyTokenFromHub(t, hub, myPubHex, 90_000)
	if res := f.run("receive", token); res.ExitCode != 0 {
		t.Fatalf("receive: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	f.mustJSON("connect", "add", "dest", hub.PairingUri)
	if res := f.run("redeem", "--all", "--into", "dest", "--json", "--yes"); res.ExitCode != 0 {
		t.Fatalf("redeem: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	requireBillSpentAway(t, admin, hub.ID, token, "the redeemed bill")

	// The attacker knows the wallet pubkey — it is public — but not the connection
	// secret, so their bill proof is signed with a key of their own.
	tok, err := nipcash.Decode(token)
	if err != nil {
		t.Fatalf("decode token: %v", err)
	}
	// Everything the real token carries EXCEPT its connection secret. The mint
	// provenance has to be kept: it is the only thing identifying the minting Hub, so
	// a client stripped of it cannot even find the Hub to ask — which would make this
	// test pass for the wrong reason (it did, on the first run).
	//
	// An attacker with all of this and no secret is the realistic case: a wallet
	// pubkey is public, relay hints travel with the bill, and provenance is
	// verifiable by anyone.
	forged, err := nipcash.Encode(nipcash.Token{
		HRP:                  tok.HRP,
		WalletPubkey:         tok.WalletPubkey,
		Secret:               fakeHex32(t),
		RelayURLs:            tok.RelayURLs,
		IdentityRequired:     tok.IdentityRequired,
		MintSignature:        tok.MintSignature,
		AttestedAmountMillis: tok.AttestedAmountMillis,
	})
	if err != nil {
		t.Fatalf("re-encode token with a forged connection secret: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, dialErr := nipcashclient.Connect(ctx, forged)
	if dialErr != nil {
		t.Logf("dial with a forged secret failed outright, which is also silence: %v", dialErr)
		return
	}
	defer c.Close()

	privAny, _ := newIdentity(t)
	res, err := c.CashStatus(ctx, nipcash.BySigning(privAny), nipcash.ScopeMine)
	if err == nil {
		t.Fatalf("a guessed wallet pubkey got an answer about a destroyed bill: %+v", res)
	}
	if !strings.Contains(err.Error(), "no answer") {
		t.Errorf("want an information-free omission, got %v", err)
	}
	if strings.Contains(err.Error(), "spent") {
		t.Errorf("the refusal confirmed the bill had existed: %v", err)
	}
}
