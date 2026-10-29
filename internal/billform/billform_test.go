package billform

import (
	"strings"
	"testing"

	"github.com/flokiorg/go-flokicoin/chainutil/bech32"
	"github.com/ohstr/nmilat/nipcash"
)

// signedBill is a scheme-1 bill signed with a local key, over relays.
func signedBill(t *testing.T, relays ...string) string {
	t.Helper()
	amt := uint64(21000)
	tok := nipcash.Token{
		HRP:                  "satscash",
		WalletPubkey:         "c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5",
		Secret:               "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
		RelayURLs:            relays,
		AttestedAmountMillis: &amt,
		HubGroup:             nipcash.HubGroupFor(strings.Repeat("ab", 32)),
	}
	sign, err := nipcash.LocalMintSigner(strings.Repeat("01", 32), "satscash")
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

// tlvs splits s into its raw entries.
func tlvs(t *testing.T, s string) (string, []entry) {
	t.Helper()
	hrp, words, err := bech32.DecodeNoLimit(strings.ToLower(s))
	if err != nil {
		t.Fatal(err)
	}
	data, err := bech32.ConvertBits(words, 5, 8, false)
	if err != nil {
		t.Fatal(err)
	}
	var out []entry
	for i := 0; i < len(data); i += 2 + int(data[i+1]) {
		out = append(out, entry{data[i], data[i+2 : i+2+int(data[i+1])]})
	}
	return hrp, out
}

// respell writes entries back as a bill string, in the order given.
func respell(t *testing.T, hrp string, entries []entry) string {
	t.Helper()
	var data []byte
	for _, e := range entries {
		data = append(data, e.typ, byte(len(e.value)))
		data = append(data, e.value...)
	}
	w, err := bech32.ConvertBits(data, 8, 5, true)
	if err != nil {
		t.Fatal(err)
	}
	s, err := bech32.Encode(hrp, w)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCanonical_EverySpellingOfOneBillIsOneString(t *testing.T) {
	bill := signedBill(t, "wss://relay.example.com", "wss://two.example.com")
	hrp, entries := tlvs(t, bill)

	reversed := make([]entry, len(entries))
	for i, e := range entries {
		reversed[len(entries)-1-i] = e
	}
	// The two relay hints keep their order, as relay_hash needs: TLV 1 entries
	// moved together, everything else around them.
	var relaysFirst []entry
	for _, e := range entries {
		if e.typ == 1 {
			relaysFirst = append(relaysFirst, e)
		}
	}
	for _, e := range entries {
		if e.typ != 1 {
			relaysFirst = append(relaysFirst, e)
		}
	}
	reversedRelays := reversed
	for i, j := 0, 0; i < len(reversedRelays); i++ {
		if reversedRelays[i].typ == 1 {
			reversedRelays[i] = relaysFirst[j]
			j++
		}
	}
	spellings := map[string]string{
		"as encoded":                  bill,
		"uppercase":                   strings.ToUpper(bill),
		"an unknown TLV 9 appended":   respell(t, hrp, append(append([]entry(nil), entries...), entry{9, []byte{1}})),
		"an unknown TLV 4 inserted":   respell(t, hrp, append([]entry{{4, []byte("x")}}, entries...)),
		"entries in reverse order":    respell(t, hrp, reversedRelays),
		"relays first, then the rest": respell(t, hrp, relaysFirst),
	}
	want, err := nipcash.Encode(mustDecode(t, bill))
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range spellings {
		got, err := Canonical(s)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != want {
			t.Errorf("%s: Canonical = %s, want %s", name, got, want)
		}
		if _, ok := nipcash.VerifyProvenance(mustDecode(t, got)); !ok {
			t.Errorf("%s: the canonical spelling no longer verifies", name)
		}
		if again, _ := Canonical(got); again != got {
			t.Errorf("%s: Canonical is not idempotent", name)
		}
	}
}

func TestCanonical_KeepsWhatTheSignatureCovers(t *testing.T) {
	// A blank hint is part of relay_hash, so it stays, byte for byte.
	bill := signedBill(t, "wss://relay.example.com")
	hrp, entries := tlvs(t, bill)
	var withBlank []entry
	for _, e := range entries {
		withBlank = append(withBlank, e)
		if e.typ == 1 {
			withBlank = append(withBlank, entry{1, []byte(" ")})
		}
	}
	s := respell(t, hrp, withBlank)
	got, err := Canonical(s)
	if err != nil {
		t.Fatal(err)
	}
	if got != s {
		t.Errorf("a canonical string with a blank hint changed: %s, want %s", got, s)
	}
	if tok := mustDecode(t, got); len(tok.RelayURLs) != 2 || tok.RelayURLs[1] != " " {
		t.Errorf("relays = %q, want the blank hint kept", tok.RelayURLs)
	}

	// Two different bills stay two strings.
	other := signedBill(t, "wss://other.example.com")
	a, _ := Canonical(bill)
	b, _ := Canonical(other)
	if a == b {
		t.Error("two bills on different relays gave one canonical string")
	}
}

func TestCanonical_RefusesWhatADecoderRefuses(t *testing.T) {
	bill := signedBill(t, "wss://relay.example.com")
	hrp, entries := tlvs(t, bill)
	mixed := bill[:10] + strings.ToUpper(bill[10:12]) + bill[12:]
	var noSecret []entry
	for _, e := range entries {
		if e.typ != 2 {
			noSecret = append(noSecret, e)
		}
	}
	for name, s := range map[string]string{
		"mixed case":        mixed,
		"no TLV 2":          respell(t, hrp, noSecret),
		"a second TLV 0":    respell(t, hrp, append(append([]entry(nil), entries...), entries[0])),
		"not bech32":        "satscash1notabill",
		"a broken checksum": bill[:len(bill)-1] + map[bool]string{true: "q", false: "p"}[bill[len(bill)-1] != 'q'],
	} {
		if got, err := Canonical(s); err == nil {
			t.Errorf("%s: Canonical = %s, want a refusal", name, got)
		}
	}
}

func TestWalletPubkey_IsTheSameForEverySpelling(t *testing.T) {
	bill := signedBill(t, "wss://relay.example.com")
	hrp, entries := tlvs(t, bill)
	for _, s := range []string{bill, strings.ToUpper(bill), respell(t, hrp, append(entries, entry{9, nil}))} {
		got, err := WalletPubkey(s)
		if err != nil || got != "c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5" {
			t.Errorf("WalletPubkey = %q, %v", got, err)
		}
	}
	if _, err := WalletPubkey("satscash1notabill"); err == nil {
		t.Error("WalletPubkey of a non-bill succeeded")
	}
}

func mustDecode(t *testing.T, s string) nipcash.Token {
	t.Helper()
	tok, err := nipcash.Decode(s)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}
