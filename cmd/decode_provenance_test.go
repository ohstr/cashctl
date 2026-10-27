package cmd

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ohstr/nmilat/nipcash"
	"github.com/ohstr/nmilat/utils"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/output"
	"github.com/ohstr/cashctl/internal/testmint"
)

// What cashctl shows and does about a bill's mint provenance (NIP-CASH §Mint
// Provenance): the scheme-1 signature covers the wallet key, the amount and
// relay_hash, a hash of this copy's relay hints and secret. A copy whose relays
// or secret were changed no longer verifies, and is never dialled.
//
// The bills here are signed in the test with the key 0x11 x 32, to exercise
// cashctl's behaviour; the known answers (K-TOK-1, K-PAY-*, K-RH-*) come from
// nmilat's vector files in vectors_test.go.
const (
	provMinter = "034f355bdcb7cc0af728ef3cceb9615d90684bb5b2ca5f859ab0f0b704075871aa"
	provAmount = 21000
)

var (
	provPriv   = hex.EncodeToString(n(0x11, 32))
	provWallet = n(0x22, 32)
)

func amountTLV(millis uint64) tlv {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], millis)
	return tlv{6, b[:]}
}

// provSig signs the scheme-1 payload for hrp/wallet/amount over relays and secret.
func provSig(t *testing.T, hrp string, wallet []byte, amount uint64, relays []string, secret []byte) []byte {
	t.Helper()
	relayHash, err := nipcash.RelayHash(relays, hex.EncodeToString(secret))
	if err != nil {
		t.Fatal(err)
	}
	signer, err := nipcash.LocalMintSigner(provPriv, hrp)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := signer(nipcash.MintPayloadV1(hrp, hex.EncodeToString(wallet), amount, relayHash))
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

// signedTLVs is a scheme-1 bill as raw TLVs, with extra entries placed after.
func signedTLVs(t *testing.T, relay string, extra ...tlv) []tlv {
	t.Helper()
	sig := provSig(t, "lokicash", provWallet, provAmount, []string{relay}, tlvSecret.val)
	tlvs := []tlv{{0, provWallet}, {1, []byte(relay)}, tlvSecret, {5, sig}, amountTLV(provAmount), {8, []byte{1}}}
	return append(tlvs, extra...)
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

// The control: a well-formed scheme-1 bill verifies and names its minter, and
// decode shows its scheme and relay_hash. Without it every "never valid" case
// below could pass because nothing ever verifies.
func TestDecode_SchemeOneProvenanceVerifies(t *testing.T) {
	if pub, _ := utils.GetPublicKey(provPriv); !strings.HasSuffix(provMinter, pub) {
		t.Fatalf("the test key's public key %s is not %s", pub, provMinter)
	}
	relay := string(tlvRelay.val)
	out := runDecodeJSON(t, rawBech32(t, "lokicash", signedTLVs(t, relay)))
	wantHash, _ := nipcash.RelayHash([]string{relay}, hex.EncodeToString(tlvSecret.val))
	for k, want := range map[string]any{
		"signature_scheme": "mint_v1", "provenance": "valid", "mint_signature_valid": true,
		"minter_pubkey": provMinter, "relay_hash": wantHash, "attested_amount_millis": float64(provAmount),
	} {
		if out[k] != want {
			t.Errorf("%s = %v, want %v", k, out[k], want)
		}
	}
	text, err := runDecode(t, rawBech32(t, "lokicash", signedTLVs(t, relay)))
	if err != nil || !strings.Contains(text, "minter: "+provMinter) || !strings.Contains(text, "signature covers this copy's relays") {
		t.Errorf("text decode (%v) does not name the minter as covering this copy's relays:\n%s", err, text)
	}
}

// A decoder ignores a TLV of a type above 8: placed after the known entries,
// unknown ones change nothing (NIP-CASH §Wire Format).
func TestDecode_UnknownTLVsAreIgnored(t *testing.T) {
	relay := string(tlvRelay.val)
	tok := rawBech32(t, "lokicash", signedTLVs(t, relay, tlv{200, []byte("future")}, tlv{99, nil}, tlv{255, n(0xee, 40)}))
	out := runDecodeJSON(t, tok)
	if out["wallet_pubkey"] != hex.EncodeToString(provWallet) {
		t.Errorf("wallet_pubkey = %v, want %x", out["wallet_pubkey"], provWallet)
	}
	if relays, _ := out["relays"].([]any); len(relays) != 1 || relays[0] != relay {
		t.Errorf("relays = %v, want [%s]: an unknown TLV leaked into a known field", out["relays"], relay)
	}
	if out["provenance"] != "valid" || out["minter_pubkey"] != provMinter {
		t.Errorf("provenance=%v minter_pubkey=%v, want valid and %s", out["provenance"], out["minter_pubkey"], provMinter)
	}
}

// A signature that cannot be recovered, a provenance pair without TLV 8 or with
// an unknown scheme, and no provenance at all are never reported valid.
func TestDecode_AnUnverifiableSignatureIsNeverValid(t *testing.T) {
	relay := string(tlvRelay.val)
	good := provSig(t, "lokicash", provWallet, provAmount, []string{relay}, tlvSecret.val)
	mutate := func(f func([]byte)) []byte { s := bytes.Clone(good); f(s); return s }
	pair := func(sig []byte, scheme ...byte) []tlv {
		tlvs := []tlv{{0, provWallet}, tlvRelay, tlvSecret, {5, sig}, amountTLV(provAmount)}
		if len(scheme) > 0 {
			tlvs = append(tlvs, tlv{8, scheme})
		}
		return tlvs
	}
	cases := map[string]struct {
		hrp    string
		tlvs   []tlv
		scheme string
	}{
		"recovery byte out of range": {"lokicash", pair(mutate(func(s []byte) { s[0] = 4 }), 1), "mint_v1"},
		"r is zero":                  {"lokicash", pair(mutate(func(s []byte) { clear(s[1:33]) }), 1), "mint_v1"},
		"s is zero":                  {"lokicash", pair(mutate(func(s []byte) { clear(s[33:]) }), 1), "mint_v1"},
		"unknown HRP, no prefix":     {"xyzcash", pair(good, 1), "mint_v1"},
		"no scheme (TLV 8)":          {"lokicash", pair(good), "malformed"},
		"unknown scheme":             {"lokicash", pair(good, 9), "malformed"},
		"no provenance at all":       {"lokicash", []tlv{{0, provWallet}, tlvRelay, tlvSecret}, "none"},
	}
	for name, c := range cases {
		tok := rawBech32(t, c.hrp, c.tlvs)
		out := runDecodeJSON(t, tok)
		if out["signature_scheme"] != c.scheme {
			t.Errorf("%s: signature_scheme = %v, want %s", name, out["signature_scheme"], c.scheme)
		}
		if out["provenance"] == "valid" || out["mint_signature_valid"] == true {
			t.Errorf("%s: reported valid", name)
		}
		if _, ok := out["minter_pubkey"]; ok {
			t.Errorf("%s: minter_pubkey present (%v) for a signature that does not verify", name, out["minter_pubkey"])
		}
		text, err := runDecode(t, tok)
		if err != nil {
			t.Errorf("%s: text decode failed: %v", name, err)
			continue
		}
		if strings.Contains(text, provMinter) || !strings.Contains(text, "won't contact") {
			t.Errorf("%s: text output does not refuse the copy:\n%s", name, text)
		}
	}
}

// A copy whose amount, wallet, relays or secret differ from what was signed
// still recovers SOME key — recovery cannot tell — but never the real minter's,
// which is what a session compares with the Hub's announcement.
func TestDecode_ATamperedCopyNeverNamesTheRealMinter(t *testing.T) {
	relay := string(tlvRelay.val)
	good := provSig(t, "lokicash", provWallet, provAmount, []string{relay}, tlvSecret.val)
	flip := bytes.Clone(good)
	flip[64] ^= 1
	cases := map[string][]tlv{
		"signature flipped": {{0, provWallet}, tlvRelay, tlvSecret, {5, flip}, amountTLV(provAmount), {8, []byte{1}}},
		"amount raised":     {{0, provWallet}, tlvRelay, tlvSecret, {5, good}, amountTLV(provAmount + 1), {8, []byte{1}}},
		"other wallet":      {{0, n(0x23, 32)}, tlvRelay, tlvSecret, {5, good}, amountTLV(provAmount), {8, []byte{1}}},
		"relay swapped":     {{0, provWallet}, {1, []byte("wss://attacker.example")}, tlvSecret, {5, good}, amountTLV(provAmount), {8, []byte{1}}},
		"relay added":       {{0, provWallet}, tlvRelay, {1, []byte("wss://attacker.example")}, tlvSecret, {5, good}, amountTLV(provAmount), {8, []byte{1}}},
		"secret changed":    {{0, provWallet}, tlvRelay, {2, n(0xbc, 32)}, {5, good}, amountTLV(provAmount), {8, []byte{1}}},
	}
	for name, tlvs := range cases {
		tok := rawBech32(t, "lokicash", tlvs)
		if out := runDecodeJSON(t, tok); out["minter_pubkey"] == provMinter {
			t.Errorf("%s: a tampered copy still names the real minter %s", name, provMinter)
		}
		if text, _ := runDecode(t, tok); strings.Contains(text, provMinter) {
			t.Errorf("%s: text output names the real minter for a tampered copy:\n%s", name, text)
		}
	}
}

// receive checks provenance before anything else: a copy that fails it is
// invalid_input (exit 3) and no relay it names is contacted, not even to read
// the Hub's announcement.
func TestReceive_RefusesAFailingCopyBeforeDialling(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var dialled atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			dialled.Add(1)
			_ = c.Close()
		}
	}()
	relay := "ws://" + ln.Addr().String()
	// A copy whose signature cannot be recovered, pointed at this relay.
	sig := provSig(t, "lokicash", provWallet, provAmount, []string{relay}, tlvSecret.val)
	sig[0] = 4 // no recovery header
	tok := rawBech32(t, "lokicash", []tlv{{0, provWallet}, {1, []byte(relay)}, tlvSecret, {3, []byte{0}}, {5, sig}, amountTLV(provAmount), {8, []byte{1}}})

	cmd := &cobra.Command{}
	cmd.Flags().Bool("json", true, "")
	cmd.Flags().Bool("yes", true, "")
	var runErr error
	withCapturedOutput(func() { runErr = runCashReceive(cmd, []string{tok + "#" + strings.Repeat("ab", 32)}) })
	if output.ExitCode(runErr) != 3 || !strings.Contains(runErr.Error(), "ask the sender for the original") {
		t.Fatalf("receive of a copy its minter did not sign: %v (exit %d); want invalid_input naming the original", runErr, output.ExitCode(runErr))
	}
	if n := dialled.Load(); n != 0 {
		t.Errorf("the copy's relay was contacted %d time(s) before the refusal", n)
	}
}

// A cashhub1 decodes to its public fields and signature verdict, never its
// secret, and says to keep it private.
func TestDecode_CashHub(t *testing.T) {
	hubPriv, hubXOnly := testmint.Key(t)
	s := testmint.CashHub(t, hubPriv, "flokicoin", "wss://hub.example")
	hub, err := nipcash.DecodeCashHubConnection(s)
	if err != nil {
		t.Fatal(err)
	}
	out := runDecodeJSON(t, s)
	for k, want := range map[string]any{"type": "cashhub", "hub_identity": hubXOnly, "chain": "flokicoin", "signature": "verified"} {
		if out[k] != want {
			t.Errorf("%s = %v, want %v", k, out[k], want)
		}
	}
	printed, _ := json.Marshal(out)
	if strings.Contains(string(printed), hub.Secret) {
		t.Error("decode --json printed the cashhub1 secret")
	}
	if !strings.Contains(string(printed), "keep it private") {
		t.Errorf("no warning to keep the string private: %s", printed)
	}
	text, err := runDecode(t, s)
	if err != nil || strings.Contains(text, hub.Secret) || !strings.Contains(text, "signature: verified") {
		t.Errorf("text decode (%v) shows the secret or no verdict:\n%s", err, text)
	}

	// Signed by another key than the identity it names: decodes, never confirmed.
	otherPriv, _ := testmint.Key(t)
	signer, _ := nipcash.LocalChainSigner(otherPriv, "flokicoin")
	text2, err := nipcash.ConnSignedText(hub.Fields())
	if err != nil {
		t.Fatal(err)
	}
	sig, err := signer(text2)
	if err != nil {
		t.Fatal(err)
	}
	hub.Signature = sig
	forged, err := nipcash.EncodeCashHubConnection(hub)
	if err != nil {
		t.Fatal(err)
	}
	out = runDecodeJSON(t, forged)
	if out["signature"] != "unverified" || out["signature_reason"] != "signer_mismatch" {
		t.Errorf("a string signed by another key: signature=%v reason=%v, want unverified/signer_mismatch", out["signature"], out["signature_reason"])
	}

	// Mixed case is refused (BIP-173); all-uppercase, as a QR code carries it, reads.
	if _, err := runDecode(t, strings.ToUpper(s[:10])+s[10:]); output.ExitCode(err) != 3 {
		t.Errorf("mixed-case cashhub1: %v, want invalid_input", err)
	}
	if out := runDecodeJSON(t, strings.ToUpper(s)); out["hub_identity"] != hubXOnly {
		t.Errorf("all-uppercase cashhub1 did not decode: %v", out)
	}
}
