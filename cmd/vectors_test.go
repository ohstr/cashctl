package cmd

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The known answers for provenance and the relay hash come from nmilat's
// exported vector files (testdata/vectors, NIP-CASH §Mint Provenance), read
// from the nmilat module this build uses, so cashctl and nmilat can't drift
// apart on them (plan §10.5, T-31). An nmilat that exports no vectors skips.

// nmilatVector reads one of nmilat's vector files.
func nmilatVector(t *testing.T, name string) []byte {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/ohstr/nmilat").Output()
	if err != nil {
		t.Skipf("can't locate the nmilat module: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(out)), "testdata", "vectors", name))
	if err != nil {
		t.Skipf("this nmilat exports no %s: %v", name, err)
	}
	return b
}

type provenanceVectors struct {
	SpecToken struct {
		Token, Minter string
	} `json:"specToken"`
	Verify []struct {
		Name                 string
		HRP                  string `json:"hrp"`
		WalletPubkey         string
		Relays               []string
		Secret               string
		Scheme               int
		Zap                  json.RawMessage
		MintSignature        *string
		AttestedAmountMillis *string
		Minter               *string
	} `json:"verify"`
}

type relayHashVectors struct {
	Secret string
	Hash   []struct {
		ID     string
		Relays []string
		Hash   string
	} `json:"hash"`
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// vectorBill is a bill as raw TLVs from vector fields: TLV 8 only for a
// scheme, TLV 5 and 6 only when given.
func vectorBill(t *testing.T, hrp, wallet string, relays []string, secret string, scheme int, sig, amount *string) string {
	t.Helper()
	tlvs := []tlv{{0, mustHex(t, wallet)}}
	for _, r := range relays {
		tlvs = append(tlvs, tlv{1, []byte(r)})
	}
	tlvs = append(tlvs, tlv{2, mustHex(t, secret)})
	if sig != nil {
		tlvs = append(tlvs, tlv{5, mustHex(t, *sig)})
	}
	if amount != nil {
		n, err := strconv.ParseUint(*amount, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], n)
		tlvs = append(tlvs, tlv{6, b[:]})
	}
	if scheme != 0 {
		tlvs = append(tlvs, tlv{8, []byte{byte(scheme)}})
	}
	return rawBech32(t, hrp, tlvs)
}

// T-31 (K-TOK-1): the spec token decodes with its minter and K-RH-1's relay
// hash.
func TestVectors_SpecTokenDecodes(t *testing.T) {
	var v provenanceVectors
	if err := json.Unmarshal(nmilatVector(t, "provenance.json"), &v); err != nil {
		t.Fatal(err)
	}
	var rh relayHashVectors
	if err := json.Unmarshal(nmilatVector(t, "relay-hash.json"), &rh); err != nil {
		t.Fatal(err)
	}
	out := runDecodeJSON(t, v.SpecToken.Token)
	if out["provenance"] != "valid" || out["minter_pubkey"] != v.SpecToken.Minter || out["signature_scheme"] != "mint_v1" {
		t.Errorf("spec token: %v", out)
	}
	if out["relay_hash"] != rh.Hash[0].Hash {
		t.Errorf("relay_hash %v, want K-RH-1 %s", out["relay_hash"], rh.Hash[0].Hash)
	}
}

// T-31: every provenance verification vector of scheme 1 (or none) decodes to
// the minter the vector names, and to no valid provenance where it names none.
// A scheme-2 bill without its zap never names a minter.
func TestVectors_ProvenanceVerify(t *testing.T) {
	var v provenanceVectors
	if err := json.Unmarshal(nmilatVector(t, "provenance.json"), &v); err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, c := range v.Verify {
		if len(c.Zap) > 0 && string(c.Zap) != "null" {
			continue // a zap context is checked with its Cash Zap (zap_test.go)
		}
		t.Run(c.Name, func(t *testing.T) {
			bill := vectorBill(t, c.HRP, c.WalletPubkey, c.Relays, c.Secret, c.Scheme, c.MintSignature, c.AttestedAmountMillis)
			out := runDecodeJSON(t, bill)
			switch {
			case c.Scheme == 2:
				if out["provenance"] == "valid" || out["minter_pubkey"] != nil {
					t.Errorf("a zap bill without its zap: %v", out)
				}
			case c.Minter == nil:
				if out["provenance"] == "valid" {
					t.Errorf("no minter expected, got %v", out)
				}
			default:
				if out["provenance"] != "valid" || out["minter_pubkey"] != *c.Minter {
					t.Errorf("minter %v (%v), want %s", out["minter_pubkey"], out["provenance"], *c.Minter)
				}
			}
		})
		checked++
	}
	if checked < 10 {
		t.Fatalf("only %d vectors checked", checked)
	}
}

// K-RH-*: decode reports the relay hash of a bill's own hints and secret, every
// hint counted as given (an empty one included). A bill with no usable hint is
// refused.
func TestVectors_RelayHash(t *testing.T) {
	var rh relayHashVectors
	if err := json.Unmarshal(nmilatVector(t, "relay-hash.json"), &rh); err != nil {
		t.Fatal(err)
	}
	var pv provenanceVectors
	if err := json.Unmarshal(nmilatVector(t, "provenance.json"), &pv); err != nil {
		t.Fatal(err)
	}
	sig := pv.Verify[0].MintSignature
	amount := pv.Verify[0].AttestedAmountMillis
	for _, c := range rh.Hash {
		t.Run(c.ID, func(t *testing.T) {
			usable := false
			for _, r := range c.Relays {
				if len(r) > 255 {
					t.Skip("a hint over 255 bytes has no TLV form")
				}
				usable = usable || r != ""
			}
			bill := vectorBill(t, "satscash", pv.Verify[0].WalletPubkey, c.Relays, rh.Secret, 1, sig, amount)
			out, err := runDecode(t, bill)
			refusable := !usable
			if refusable {
				if err == nil {
					t.Errorf("hints %q decoded: %s", c.Relays, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if j := runDecodeJSON(t, bill); j["relay_hash"] != c.Hash {
				t.Errorf("relay_hash %v, want %s", j["relay_hash"], c.Hash)
			}
		})
	}
}
