package testmint

import (
	"testing"

	"github.com/flokiorg/go-flokicoin/chainutil/bech32"
)

// WithUnknownTLV is bill with an unknown TLV 9 appended: another spelling of
// the same bill, which still decodes and verifies.
func WithUnknownTLV(t testing.TB, bill string) string {
	t.Helper()
	hrp, data := tlvBytes(t, bill)
	return tlvString(t, hrp, append(data, 9, 1, 0))
}

// Reordered is bill with its last TLV entry (the signature scheme) moved to
// the front: another spelling of the same bill, which still decodes and
// verifies.
func Reordered(t testing.TB, bill string) string {
	t.Helper()
	hrp, data := tlvBytes(t, bill)
	last := 0
	for i := 0; i < len(data); i += 2 + int(data[i+1]) {
		last = i
	}
	out := append(append([]byte(nil), data[last:]...), data[:last]...)
	return tlvString(t, hrp, out)
}

func tlvBytes(t testing.TB, s string) (string, []byte) {
	t.Helper()
	hrp, words, err := bech32.DecodeNoLimit(s)
	if err != nil {
		t.Fatal(err)
	}
	data, err := bech32.ConvertBits(words, 5, 8, false)
	if err != nil {
		t.Fatal(err)
	}
	return hrp, data
}

func tlvString(t testing.TB, hrp string, data []byte) string {
	t.Helper()
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
