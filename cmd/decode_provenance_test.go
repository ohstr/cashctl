package cmd

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// F-52: known-answer tests for what cashctl shows about a token's mint
// provenance and its unknown TLVs. The vector is nmilat's frozen KAT
// (nipcash/kat_test.go), computed outside the code under test: private key
// 0x11 x 32 signs MintPayload("lokicash", 0x22 x 32, 21000) under flnd's
// prefix.
const (
	katMinter      = "034f355bdcb7cc0af728ef3cceb9615d90684bb5b2ca5f859ab0f0b704075871aa"
	katLokicashSig = "20c72cfbb8e8566000d6d90904c0c04fd047ce066dd599ede34c84dfb314f3126709ad9a50800f285c90a9683f8282b268a8bb85f8b7a9599f9b4482cbfd83b508"
	katAmount      = 21000
)

var katWallet = n(0x22, 32)

func katSig(t *testing.T) []byte {
	t.Helper()
	sig, err := hex.DecodeString(katLokicashSig)
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

func amountTLV(millis uint64) tlv {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], millis)
	return tlv{6, b[:]}
}

// runDecodeJSON runs `cashctl decode --json <value>` and parses its stdout.
func runDecodeJSON(t *testing.T, value string) map[string]any {
	t.Helper()
	var err error
	printed := withCapturedStdout(func() {
		cmd := newDecodeCmd()
		cmd.Flags().Bool("json", true, "")
		cmd.SetArgs([]string{value})
		cmd.SetOut(io.Discard)
		err = cmd.Execute()
	})
	if err != nil {
		t.Fatalf("decode --json failed: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(printed), &out); err != nil {
		t.Fatalf("decode --json printed no JSON: %v\n%s", err, printed)
	}
	return out
}

// The well-formed vector itself: the test's own control. Without it, every
// "never valid" case below could pass because nothing ever verifies.
func TestDecode_KATProvenanceVerifies(t *testing.T) {
	tok := rawBech32(t, "lokicash", []tlv{{0, katWallet}, tlvRelay, tlvSecret, {5, katSig(t)}, amountTLV(katAmount)})
	out := runDecodeJSON(t, tok)
	if out["mint_signature_valid"] != true || out["minter_pubkey"] != katMinter {
		t.Fatalf("KAT vector: mint_signature_valid=%v minter_pubkey=%v, want true and %s", out["mint_signature_valid"], out["minter_pubkey"], katMinter)
	}
	if out["attested_amount_millis"] != float64(katAmount) {
		t.Fatalf("attested_amount_millis = %v, want %d", out["attested_amount_millis"], katAmount)
	}
}

// NIP-CASH §The Cash Token → Wire Format: a decoder MUST ignore a TLV of an
// unrecognized type (the reserved 4 included) rather than reject the token.
// Placed between and after the known entries, they must change nothing: the
// fields read the same and the signature still names the same minter.
func TestDecode_UnknownTLVsAreIgnored(t *testing.T) {
	tok := rawBech32(t, "lokicash", []tlv{
		{0, katWallet}, {4, []byte{1, 2, 3}}, tlvRelay, {200, []byte("future")},
		tlvSecret, {5, katSig(t)}, {99, nil}, amountTLV(katAmount), {255, n(0xee, 40)},
	})
	out := runDecodeJSON(t, tok)
	if out["wallet_pubkey"] != hex.EncodeToString(katWallet) {
		t.Errorf("wallet_pubkey = %v, want %x", out["wallet_pubkey"], katWallet)
	}
	relays, _ := out["relays"].([]any)
	if len(relays) != 1 || relays[0] != string(tlvRelay.val) {
		t.Errorf("relays = %v, want [%s]: an unknown TLV leaked into a known field", out["relays"], tlvRelay.val)
	}
	if out["mint_signature_valid"] != true || out["minter_pubkey"] != katMinter {
		t.Errorf("mint_signature_valid=%v minter_pubkey=%v, want true and %s", out["mint_signature_valid"], out["minter_pubkey"], katMinter)
	}

	// The same token through receive: accepted as a token (no invalid_input),
	// and its bill report names the same minter. Cash-mode with no embedded
	// secret, so receive stops at its read-only report; the relay refuses at
	// once, so the one live probe it makes fails fast without DNS.
	cashTok := rawBech32(t, "lokicash", []tlv{
		{0, katWallet}, {4, []byte{1}}, {1, []byte("ws://127.0.0.1:1")}, {200, []byte("future")},
		tlvSecret, {3, []byte{0}}, {5, katSig(t)}, amountTLV(katAmount), {77, nil},
	})
	cmd := &cobra.Command{}
	cmd.Flags().Bool("json", false, "")
	cmd.Flags().Bool("yes", false, "")
	cmd.Flags().Bool("check", false, "")
	withStdin(t, "n\n") // decline the live check
	var runErr error
	printed := withCapturedOutput(func() {
		runErr = runCashReceive(cmd, []string{cashTok})
	})
	if runErr != nil {
		t.Fatalf("receive refused a token with unknown TLVs: %v\n%s", runErr, printed)
	}
	if !strings.Contains(printed, "No spending secret") || !strings.Contains(printed, katMinter) {
		t.Fatalf("receive's report lacks the read-only result or the KAT minter:\n%s", printed)
	}
}

// A mint signature that does not verify is never reported as valid, in
// --json (`mint_signature_valid`) or in text, and never names the real
// minter. Recovery-based verification has a limit worth stating: a tampered
// payload (amount, wallet) still recovers SOME key, so the guarantee there
// is only that it is not the real minter's — which is what a caller compares
// against.
func TestDecode_BadMintSignatureIsNeverValid(t *testing.T) {
	good := katSig(t)
	mutate := func(f func(sig []byte)) []byte {
		sig := bytes.Clone(good)
		f(sig)
		return sig
	}
	unrecoverable := map[string]struct {
		hrp string
		sig []byte
	}{
		"recovery byte out of range": {"lokicash", mutate(func(s []byte) { s[0] = 0 })},
		"r is zero":                  {"lokicash", mutate(func(s []byte) { clear(s[1:33]) })},
		"s is zero":                  {"lokicash", mutate(func(s []byte) { clear(s[33:]) })},
		"r beyond the curve order":   {"lokicash", mutate(func(s []byte) { copy(s[1:33], n(0xff, 32)) })},
		"unknown HRP, no prefix":     {"xyzcash", good},
	}
	for name, c := range unrecoverable {
		tok := rawBech32(t, c.hrp, []tlv{{0, katWallet}, tlvRelay, tlvSecret, {5, c.sig}, amountTLV(katAmount)})
		out := runDecodeJSON(t, tok)
		if out["mint_signature_valid"] != false {
			t.Errorf("%s: mint_signature_valid = %v, want false", name, out["mint_signature_valid"])
		}
		if _, ok := out["minter_pubkey"]; ok {
			t.Errorf("%s: minter_pubkey present (%v) for a signature that does not verify", name, out["minter_pubkey"])
		}
		text, err := runDecode(t, tok)
		if err != nil {
			t.Errorf("%s: text decode failed: %v", name, err)
			continue
		}
		if !strings.Contains(text, "INVALID SIGNATURE") || strings.Contains(text, katMinter) {
			t.Errorf("%s: text output does not flag the signature as invalid:\n%s", name, text)
		}
	}

	tampered := map[string][]tlv{
		"amount raised":  {{0, katWallet}, tlvRelay, tlvSecret, {5, good}, amountTLV(katAmount + 1)},
		"other wallet":   {{0, n(0x23, 32)}, tlvRelay, tlvSecret, {5, good}, amountTLV(katAmount)},
		"signature flip": {{0, katWallet}, tlvRelay, tlvSecret, {5, mutate(func(s []byte) { s[64] ^= 1 })}, amountTLV(katAmount)},
	}
	for name, tlvs := range tampered {
		tok := rawBech32(t, "lokicash", tlvs)
		if out := runDecodeJSON(t, tok); out["minter_pubkey"] == katMinter {
			t.Errorf("%s: a tampered token still names the real minter %s", name, katMinter)
		}
		if text, _ := runDecode(t, tok); strings.Contains(text, katMinter) {
			t.Errorf("%s: text output names the real minter for a tampered token:\n%s", name, text)
		}
	}
}
