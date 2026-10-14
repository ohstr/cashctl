package cmd

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/flokiorg/go-flokicoin/chainutil/bech32"
)

// F-61: the decoder rules cashctl relies on (through nmilat) for strings it is
// handed, exercised through `cashctl decode` itself with hand-built TLVs whose
// checksum is valid — so a rejection is the TLV rule's doing, not the
// checksum's (NIP-CASH §The Cash Token → Wire Format; NIP-CW §The Circle Wallet
// Hub Connection).

type tlv struct {
	typ byte
	val []byte
}

func rawBech32(t *testing.T, hrp string, tlvs []tlv, trailing ...byte) string {
	t.Helper()
	var buf bytes.Buffer
	for _, e := range tlvs {
		buf.WriteByte(e.typ)
		buf.WriteByte(byte(len(e.val)))
		buf.Write(e.val)
	}
	buf.Write(trailing)
	bits5, err := bech32.ConvertBits(buf.Bytes(), 8, 5, true)
	if err != nil {
		t.Fatal(err)
	}
	s, err := bech32.Encode(hrp, bits5)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func n(b byte, count int) []byte { return bytes.Repeat([]byte{b}, count) }

var (
	tlvPub    = tlv{0, n(0xaa, 32)}
	tlvRelay  = tlv{1, []byte("wss://relay.example")}
	tlvSecret = tlv{2, n(0xbb, 32)}
)

// runDecode runs `cashctl decode <value>` and returns its output and error.
func runDecode(t *testing.T, value string) (string, error) {
	t.Helper()
	var err error
	printed := withCapturedOutput(func() {
		cmd := newDecodeCmd()
		cmd.SetArgs([]string{value})
		cmd.SetOut(io.Discard)
		err = cmd.Execute()
	})
	return printed, err
}

func TestDecode_RejectsMalformedCashTokenTLVs(t *testing.T) {
	if _, err := runDecode(t, rawBech32(t, "lokicash", []tlv{tlvPub, tlvRelay, tlvSecret})); err != nil {
		t.Fatalf("the well-formed control token was rejected, so every case below would pass vacuously: %v", err)
	}
	cases := map[string][]tlv{
		"missing wallet pubkey (0)":      {tlvRelay, tlvSecret},
		"missing secret (2)":             {tlvPub, tlvRelay},
		"short wallet pubkey":            {{0, n(0xaa, 31)}, tlvRelay, tlvSecret},
		"long secret":                    {tlvPub, tlvRelay, {2, n(0xbb, 33)}},
		"two-byte identity required (3)": {tlvPub, tlvRelay, tlvSecret, {3, []byte{0, 1}}},
		"repeated wallet pubkey":         {tlvPub, tlvPub, tlvRelay, tlvSecret},
		"repeated secret":                {tlvPub, tlvRelay, tlvSecret, tlvSecret},
		"repeated identity required":     {tlvPub, tlvRelay, tlvSecret, {3, []byte{0}}, {3, []byte{0}}},
		"no relay hint":                  {tlvPub, tlvSecret},
		"empty relay hint":               {tlvPub, {1, nil}, tlvSecret},
	}
	for name, tlvs := range cases {
		if _, err := runDecode(t, rawBech32(t, "lokicash", tlvs)); err == nil {
			t.Errorf("%s: decoded, want rejected", name)
		}
	}
	// A length byte promising more than is left: truncated, not read out of bounds.
	truncated := rawBech32(t, "lokicash", []tlv{tlvPub, tlvRelay, tlvSecret}, 3, 10, 0)
	if _, err := runDecode(t, truncated); err == nil {
		t.Error("truncated TLV: decoded, want rejected")
	}
}

// An unpaired, wrong-length or repeated mint signature (5) / attested amount (6)
// is "no valid provenance", never a decode failure.
func TestDecode_BadProvenanceIsNoProvenanceNotAFailure(t *testing.T) {
	sig, amount := tlv{5, n(0x01, 65)}, tlv{6, n(0x00, 8)}
	cases := map[string][]tlv{
		"signature without amount": {tlvPub, tlvRelay, tlvSecret, sig},
		"amount without signature": {tlvPub, tlvRelay, tlvSecret, amount},
		"short signature":          {tlvPub, tlvRelay, tlvSecret, {5, n(0x01, 64)}, amount},
		"repeated pair":            {tlvPub, tlvRelay, tlvSecret, sig, amount, sig, amount},
	}
	for name, tlvs := range cases {
		printed, err := runDecode(t, rawBech32(t, "lokicash", tlvs))
		if err != nil {
			t.Errorf("%s: decode failed (%v), want it decoded with no valid provenance", name, err)
			continue
		}
		if !strings.Contains(printed, "minter: unknown") && !strings.Contains(printed, "INVALID SIGNATURE") {
			t.Errorf("%s: provenance not reported as missing or invalid:\n%s", name, printed)
		}
	}
}

func TestDecode_RejectsMalformedCircleHubTLVs(t *testing.T) {
	if _, err := runDecode(t, rawBech32(t, "circlehub", []tlv{tlvPub, tlvRelay, tlvSecret})); err != nil {
		t.Fatalf("the well-formed control string was rejected: %v", err)
	}
	cases := map[string][]tlv{
		"missing wallet pubkey": {tlvRelay, tlvSecret},
		"missing secret":        {tlvPub, tlvRelay},
		"short secret":          {tlvPub, tlvRelay, {2, n(0xbb, 16)}},
		"repeated pubkey":       {tlvPub, tlvPub, tlvRelay, tlvSecret},
	}
	for name, tlvs := range cases {
		if _, err := runDecode(t, rawBech32(t, "circlehub", tlvs)); err == nil {
			t.Errorf("%s: decoded, want rejected", name)
		}
	}
}
