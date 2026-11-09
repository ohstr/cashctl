package cashkitimport

import (
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nipcash"

	"github.com/ohstr/cashctl/internal/cashkitbackup"
)

// signedToken builds a decodable scheme-1 cash token with the given HRP, so a
// test can exercise the currency guard with a real satscash (non-loki) token.
func signedToken(t *testing.T, hrp string) string {
	t.Helper()
	amt := uint64(21000)
	tok := nipcash.Token{
		HRP:                  hrp,
		WalletPubkey:         "c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5",
		Secret:               "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
		RelayURLs:            []string{"wss://bank.test"},
		AttestedAmountMillis: &amt,
		HubGroup:             nipcash.HubGroupFor(strings.Repeat("ab", 32)),
	}
	sign, err := nipcash.LocalMintSigner(strings.Repeat("01", 32), hrp)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := nipcash.SignToken(tok, nipcash.SchemeMintV1, nil,
		"1b84c5567b126440995d3ed5aaba0565d71e1834604819ff9c17f5e9d5dd078f", sign)
	if err != nil {
		t.Fatal(err)
	}
	s, err := nipcash.Encode(signed)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestMapHolding_SkipsNonLokiCurrency pins CASHCTL-0009: a non-loki (satscash)
// cash holding must be skipped, never folded into cashctl's loki-only ledger
// where it would be summed with real loki and spent interchangeably. A lokicash
// holding on the same path still imports.
func TestMapHolding_SkipsNonLokiCurrency(t *testing.T) {
	open := func(cashkitbackup.Sealed) (string, error) { return "a-cash-secret", nil }
	sealed := cashkitbackup.Sealed{}

	sats := cashkitbackup.Holding{
		Mode: "cash", Status: "held", Token: signedToken(t, "satscash"), CashSecret: &sealed,
	}
	if _, reason, ok := mapHolding(sats, open); ok || reason != SkipNonLoki {
		t.Fatalf("satscash holding: ok=%v reason=%q, want skipped %q", ok, reason, SkipNonLoki)
	}

	loki := cashkitbackup.Holding{
		Mode: "cash", Status: "held", Token: signedToken(t, "lokicash"), CashSecret: &sealed,
	}
	e, reason, ok := mapHolding(loki, open)
	if !ok {
		t.Fatalf("lokicash holding: ok=%v reason=%q, want imported", ok, reason)
	}
	if e.Token != loki.Token {
		t.Errorf("imported entry token = %q, want the lokicash token", e.Token)
	}
}
