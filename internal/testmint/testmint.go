// Package testmint signs what a test needs as a Hub would: bills (scheme 1,
// cash-mint:v1) and cashhub1 and circlehub1 connection strings. Every encoder now refuses an
// unsigned bill or string, so a test fixture has to be signed like a real one.
// For tests and test tooling only; it is never imported by the shipped binary
// (cmd/dial_lint_test.go).
package testmint

import (
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/ohstr/nmilat/nipcash"
	"github.com/ohstr/nmilat/nipcw"
	"github.com/ohstr/nmilat/utils"
)

// Key is a fresh private key and its x-only public key, both hex.
func Key(t testing.TB) (priv, xonly string) {
	t.Helper()
	priv = Hex32(t)
	xonly, err := utils.GetPublicKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return priv, xonly
}

// Hex32 is 32 random bytes, hex.
func Hex32(t testing.TB) string {
	t.Helper()
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b[:])
}

// Token signs tok under scheme 1 with minterPriv and returns it, filling what a
// fixture usually leaves out: HRP lokicash, a wallet key and secret, a relay
// hint, an attested amount of 1000. minterPriv "" uses a fresh key.
func Token(t testing.TB, tok nipcash.Token, minterPriv string) nipcash.Token {
	t.Helper()
	signed, err := sign(tok, minterPriv)
	if err != nil {
		t.Fatalf("testmint: %v", err)
	}
	return signed
}

// MustBill is Bill for a fixture built outside a test function; it panics.
func MustBill(tok nipcash.Token, minterPriv string) string {
	signed, err := sign(tok, minterPriv)
	if err != nil {
		panic(err)
	}
	s, err := nipcash.Encode(signed)
	if err != nil {
		panic(err)
	}
	return s
}

func sign(tok nipcash.Token, minterPriv string) (nipcash.Token, error) {
	if minterPriv == "" {
		minterPriv = randomHex()
	}
	if tok.HRP == "" {
		tok.HRP = "lokicash"
	}
	if tok.WalletPubkey == "" {
		tok.WalletPubkey = randomHex()
	}
	if tok.Secret == "" {
		tok.Secret = randomHex()
	}
	if len(tok.RelayURLs) == 0 {
		tok.RelayURLs = []string{"wss://relay.example"}
	}
	if tok.AttestedAmountMillis == nil {
		amount := uint64(1000)
		tok.AttestedAmountMillis = &amount
	}
	tok.MintSignature, tok.Scheme = nil, nipcash.SchemeNone
	signer, err := nipcash.LocalMintSigner(minterPriv, tok.HRP)
	if err != nil {
		return tok, err
	}
	minterXOnly, err := utils.GetPublicKey(minterPriv)
	if err != nil {
		return tok, err
	}
	return nipcash.SignToken(tok, nipcash.SchemeMintV1, nil, minterXOnly, signer)
}

func randomHex() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// Bill is Token, encoded.
func Bill(t testing.TB, tok nipcash.Token, minterPriv string) string {
	t.Helper()
	s, err := nipcash.Encode(Token(t, tok, minterPriv))
	if err != nil {
		t.Fatalf("testmint: encode token: %v", err)
	}
	return s
}

// CashHub is a cashhub1 string signed by hubPriv for chain (default
// "flokicoin"), naming relays (default one example relay).
func CashHub(t testing.TB, hubPriv, chain string, relays ...string) string {
	t.Helper()
	s, err := cashHub(hubPriv, chain, relays...)
	if err != nil {
		t.Fatalf("testmint: %v", err)
	}
	return s
}

// MustCashHub is CashHub outside a test function; it panics.
func MustCashHub(hubPriv, chain string, relays ...string) string {
	s, err := cashHub(hubPriv, chain, relays...)
	if err != nil {
		panic(err)
	}
	return s
}

func cashHub(hubPriv, chain string, relays ...string) (string, error) {
	if chain == "" {
		chain = "flokicoin"
	}
	if len(relays) == 0 {
		relays = []string{"wss://relay.example"}
	}
	identity, err := utils.GetPublicKey(hubPriv)
	if err != nil {
		return "", err
	}
	signer, err := nipcash.LocalChainSigner(hubPriv, chain)
	if err != nil {
		return "", err
	}
	conn, err := nipcash.SignCashHubConnection(nipcash.CashHubConnection{
		Pubkey: randomHex(), Relays: relays, Secret: randomHex(), HubIdentity: identity, Chain: chain,
	}, signer, identity)
	if err != nil {
		return "", err
	}
	return nipcash.EncodeCashHubConnection(conn)
}

// Encode is Bill for a call site shaped like nipcash.Encode: the token is
// signed by a fresh minter key (any signature it carried is replaced), then
// encoded.
func Encode(t testing.TB, tok nipcash.Token) (string, error) {
	t.Helper()
	signed, err := sign(tok, "")
	if err != nil {
		return "", err
	}
	return nipcash.Encode(signed)
}

// CircleHub is a circlehub1 string signed by hubPriv's x-only identity on
// chain "flokicoin": a random connection key and secret, label (when not
// empty) and relays (wss://relay.example when none).
func CircleHub(t testing.TB, hubPriv, label string, relays ...string) string {
	t.Helper()
	s, err := circleHub(hubPriv, nipcw.CircleHubConnection{Label: label, HasLabel: label != "", Relays: relays})
	if err != nil {
		t.Fatalf("testmint: %v", err)
	}
	return s
}

// MustCircleHub is CircleHub outside a test function; it panics.
func MustCircleHub(hubPriv, label string, relays ...string) string {
	s, err := circleHub(hubPriv, nipcw.CircleHubConnection{Label: label, HasLabel: label != "", Relays: relays})
	if err != nil {
		panic(err)
	}
	return s
}

// SignCircleHub signs c (its Pubkey and Secret random when empty) as hubPriv's
// identity, on c.Chain or "flokicoin".
func SignCircleHub(t testing.TB, hubPriv string, c nipcw.CircleHubConnection) string {
	t.Helper()
	s, err := circleHub(hubPriv, c)
	if err != nil {
		t.Fatalf("testmint: %v", err)
	}
	return s
}

func circleHub(hubPriv string, c nipcw.CircleHubConnection) (string, error) {
	if c.Chain == "" {
		c.Chain = "flokicoin"
	}
	if len(c.Relays) == 0 {
		c.Relays = []string{"wss://relay.example"}
	}
	if c.Pubkey == "" {
		c.Pubkey = randomHex()
	}
	if c.Secret == "" {
		c.Secret = randomHex()
	}
	identity, err := utils.GetPublicKey(hubPriv)
	if err != nil {
		return "", err
	}
	c.HubIdentity = identity
	signer, err := nipcash.LocalChainSigner(hubPriv, c.Chain)
	if err != nil {
		return "", err
	}
	signed, err := nipcw.SignCircleHubConnection(c, signer, identity)
	if err != nil {
		return "", err
	}
	return nipcw.EncodeCircleHubConnection(signed)
}
