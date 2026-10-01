//go:build integration

// The possession gate, attacked against a REAL hub.
//
// The unit tests in lokihub cover the same forgeries against the dispatch function
// directly. These exist because that is not the same thing: here the item is sealed
// into a real NIP-44 envelope, published to a real relay, unwrapped by the real hub
// process, and the answer (or the absence of one) comes back over the wire. A gate
// that is enforced in the function but bypassed by the path that reaches it would
// pass every unit test and fail here.
//
// Every case must be met with SILENCE — an omission, not an error. An error would
// confirm the bill exists, and a wallet pubkey is public and guessable, so a hub
// that answers becomes an oracle for every bill it ever minted.
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

// retoken re-encodes tok with a different connection secret, keeping everything
// else — including the mint provenance, without which a client cannot locate the
// hub at all and would fail for an unrelated reason.
func retoken(t *testing.T, tok nipcash.Token, secret string) string {
	t.Helper()
	forged, err := nipcash.Encode(nipcash.Token{
		HRP:                  tok.HRP,
		WalletPubkey:         tok.WalletPubkey,
		Secret:               secret,
		RelayURLs:            tok.RelayURLs,
		IdentityRequired:     tok.IdentityRequired,
		MintSignature:        tok.MintSignature,
		AttestedAmountMillis: tok.AttestedAmountMillis,
	})
	if err != nil {
		t.Fatalf("re-encode token: %v", err)
	}
	return forged
}

// TestBillProofAttack_WrongConnectionSecretGetsSilence is the core attack: everything
// public about a bill, and a possession proof signed with a key of the attacker's own.
//
// This is what the gate exists for. Before it, this request was served — the hub had
// no way to tell this caller from the bill's real holder, because nothing in the
// envelope was tied to the token.
func TestBillProofAttack_WrongConnectionSecretGetsSilence(t *testing.T) {
	_, hub := plainClientHub(t)

	privA, pubA := newIdentity(t)
	_, pubB := newIdentity(t)
	token := mintTwoRecipientBill(t, hub, pubA, pubB, 40_000, 60_000)

	tok, err := nipcash.Decode(token)
	if err != nil {
		t.Fatalf("decode token: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// A real recipient's slice credential — privA genuinely holds a slice — paired
	// with a forged possession proof. So the ONLY thing wrong is possession, which is
	// what isolates this test to the gate under examination.
	c, err := nipcashclient.Connect(ctx, retoken(t, tok, fakeHex32(t)))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	res, err := c.CashStatus(ctx, nipcash.BySigning(privA), nipcash.ScopeMine)
	if err == nil {
		t.Fatalf("a forged possession proof was SERVED (%+v) — a guessable wallet pubkey is now an oracle", res)
	}
	if !strings.Contains(err.Error(), "no answer") {
		t.Errorf("want an information-free omission, got %v", err)
	}
	// It must not leak that the bill exists, nor anything on it.
	for _, leak := range []string{"NOT_FOUND", "no slice", "spent", pubA, pubB} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("the omission leaked %q: %v", leak, err)
		}
	}
}

// TestBillProofAttack_RealRecipientStillWorks is the control.
//
// Without it the test above proves only that something failed. Same bill, same
// relay, same hub, same slice credential — the one difference is a genuine
// connection secret — and it must be answered.
func TestBillProofAttack_RealRecipientStillWorks(t *testing.T) {
	_, hub := plainClientHub(t)

	privA, pubA := newIdentity(t)
	_, pubB := newIdentity(t)
	const amountA = uint64(40_000)
	token := mintTwoRecipientBill(t, hub, pubA, pubB, amountA, 60_000)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	res, err := plainClient(t, ctx, token).CashStatus(ctx, nipcash.BySigning(privA), nipcash.ScopeMine)
	if err != nil {
		t.Fatalf("the real holder must be served: %v", err)
	}
	if len(res.Recipients) != 1 || res.Recipients[0].IdentityValue != pubA {
		t.Fatalf("want exactly the caller's own row, got %+v", res.Recipients)
	}
	if res.Recipients[0].AmountMillis != amountA {
		t.Errorf("own slice = %d, want %d", res.Recipients[0].AmountMillis, amountA)
	}
}

// TestBillProofAttack_AGuessedWalletPubkeyCannotEvenReachTheHub records a second,
// independent barrier that turned up while trying to mount the enumeration attack
// here: it cannot be mounted with a real client at all.
//
// The mint signature is over MintPayload(HRP, walletPubkey, amount), so it BINDS the
// wallet pubkey. Swap in a guessed pubkey and the provenance no longer verifies;
// provenance is the only thing identifying the minting hub, so the client has no hub
// identity to verify an announcement against and stops before sending anything.
//
// Defence in depth, and worth pinning as such: the possession gate is the barrier
// that must hold, and this one sits in front of it. The hub-side behaviour for a
// hand-crafted envelope that skips the client entirely — which is what a real
// attacker would send — is covered by lokihub's own dispatch tests, where the
// envelope is built by hand for exactly this reason.
func TestBillProofAttack_AGuessedWalletPubkeyCannotEvenReachTheHub(t *testing.T) {
	_, hub := plainClientHub(t)

	_, pubA := newIdentity(t)
	_, pubB := newIdentity(t)
	real := mintTwoRecipientBill(t, hub, pubA, pubB, 40_000, 60_000)
	realTok, err := nipcash.Decode(real)
	if err != nil {
		t.Fatalf("decode token: %v", err)
	}

	// The real bill's relays and provenance, a wallet pubkey that is pure invention.
	guessed, err := nipcash.Encode(nipcash.Token{
		HRP:                  realTok.HRP,
		WalletPubkey:         fakeHex32(t),
		Secret:               fakeHex32(t),
		RelayURLs:            realTok.RelayURLs,
		MintSignature:        realTok.MintSignature,
		AttestedAmountMillis: realTok.AttestedAmountMillis,
	})
	if err != nil {
		t.Fatalf("encode guessed token: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, dialErr := nipcashclient.Connect(ctx, guessed)
	if dialErr != nil {
		t.Logf("dial failed outright: %v", dialErr)
		return
	}
	defer c.Close()

	privAny, _ := newIdentity(t)
	res, err := c.CashStatus(ctx, nipcash.BySigning(privAny), nipcash.ScopeMine)
	if err == nil {
		t.Fatalf("a guessed wallet pubkey was answered: %+v", res)
	}
	// Either barrier is a pass; what must never happen is an answer. Asserting only
	// "no answer" here would have been wrong — the request never reaches the hub, so
	// there is no omission to observe, and a test demanding one fails for a reason
	// that is actually good news.
	msg := err.Error()
	if !strings.Contains(msg, "announcement") && !strings.Contains(msg, "mint signature") &&
		!strings.Contains(msg, "no answer") {
		t.Errorf("unexpected failure mode for a guessed wallet pubkey: %v", err)
	}
	// And nothing about the real bill leaked either way.
	for _, leak := range []string{pubA, pubB} {
		if strings.Contains(msg, leak) {
			t.Errorf("the failure leaked %q: %v", leak, err)
		}
	}
}

// TestBillProofAttack_SpendMethodsAreGatedToo: possession gates every bill method,
// not only the read.
//
// Worth pinning separately because cash_redeem moves money: a gate enforced on
// cash_status alone would leave the expensive method open, and the symptom would be
// a payout rather than a disclosure.
func TestBillProofAttack_SpendMethodsAreGatedToo(t *testing.T) {
	admin, hub := plainClientHub(t)

	// Minted to a keypair this test controls both halves of, so the slice credential
	// below is genuinely the recipient's — leaving possession as the only thing wrong.
	privA, pubA := newIdentity(t)
	const amountMillis = uint64(150_000)
	token := mintPubkeyTokenFromHub(t, hub, pubA, amountMillis)
	tok, err := nipcash.Decode(token)
	if err != nil {
		t.Fatalf("decode token: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A real invoice, so a successful attack would visibly move money rather than
	// failing on a malformed payout target.
	inv, err := dialNWC(t, ctx, hub.PairingUri).MakeInvoice(ctx, nip47.MakeInvoiceParams{
		Amount: int64(amountMillis / 2),
	})
	if err != nil {
		t.Fatalf("make_invoice: %v", err)
	}

	c, err := nipcashclient.Connect(ctx, retoken(t, tok, fakeHex32(t)))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	_, redeemErr := c.CashRedeem(ctx, nipcash.CashRedeemParams{
		Invoice:    inv.Invoice,
		Credential: nipcash.BySigning(privA),
	})
	if redeemErr == nil {
		t.Fatal("a redeem with a forged possession proof SUCCEEDED — this moves money")
	}
	if !strings.Contains(redeemErr.Error(), "no answer") {
		t.Errorf("want an information-free omission, got %v", redeemErr)
	}

	// The assertion that actually matters: the slice is still there, unspent.
	walletID := walletAppIDForPubkey(t, admin, hub.ID, tok.WalletPubkey)
	claims := liveClaimsForWallet(t, admin, hub.ID, walletID)
	claim, ok := claims[pubA]
	if !ok {
		t.Fatalf("the slice vanished after a refused redeem: %+v", claims)
	}
	if claim.Claimed {
		t.Error("the slice was marked claimed by a redeem the hub refused to serve")
	}
	if uint64(claim.AmountMloki) != amountMillis {
		t.Errorf("slice amount = %d, want %d untouched", claim.AmountMloki, amountMillis)
	}
}
