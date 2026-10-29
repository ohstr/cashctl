// Package billform gives a cash bill string its one stored spelling.
//
// A decoder accepts a bill's TLV entries in any order and ignores unknown
// types (NIP-CASH §Wire Format), and provenance covers only the known ones, so
// one bill has many spellings that all decode and verify. A wallet that keyed
// its holdings by the string would count the same bill once per spelling.
package billform

import (
	"fmt"
	"sort"
	"strings"

	"github.com/flokiorg/go-flokicoin/chainutil/bech32"
	"github.com/ohstr/nmilat/nipcash"
)

// knownTypes are the token TLV types a decoder reads; every other type is
// ignored by every decoder and is dropped here.
var knownTypes = map[byte]bool{0: true, 1: true, 2: true, 3: true, 5: true, 6: true, 7: true, 8: true}

type entry struct {
	typ   byte
	value []byte
}

// Canonical is s in its one spelling: lowercase, the known entries in
// ascending type order (TLV 1 kept in its encoded order), every entry byte
// for byte, unknown types dropped. Provenance verifies on it exactly as on s,
// and for a signed bill with no blank relay hint it equals
// nipcash.Encode(nipcash.Decode(s)). It refuses what nipcash.Decode refuses.
func Canonical(s string) (string, error) {
	if _, err := nipcash.Decode(s); err != nil {
		return "", err
	}
	hrp, words, err := bech32.DecodeNoLimit(strings.ToLower(s))
	if err != nil {
		return "", fmt.Errorf("billform: %w", err)
	}
	data, err := bech32.ConvertBits(words, 5, 8, false)
	if err != nil {
		return "", fmt.Errorf("billform: %w", err)
	}
	var entries []entry
	for i := 0; i < len(data); {
		if i+2 > len(data) || i+2+int(data[i+1]) > len(data) {
			return "", fmt.Errorf("billform: truncated TLV entry at offset %d", i)
		}
		typ, n := data[i], int(data[i+1])
		if knownTypes[typ] {
			entries = append(entries, entry{typ, data[i+2 : i+2+n]})
		}
		i += 2 + n
	}
	sort.SliceStable(entries, func(a, b int) bool { return entries[a].typ < entries[b].typ })
	out := make([]byte, 0, len(data))
	for _, e := range entries {
		out = append(out, e.typ, byte(len(e.value)))
		out = append(out, e.value...)
	}
	w, err := bech32.ConvertBits(out, 8, 5, true)
	if err != nil {
		return "", fmt.Errorf("billform: %w", err)
	}
	return bech32.Encode(hrp, w)
}

// WalletPubkey is the wallet pubkey (TLV 0) of the bill s spells: what one
// holding is keyed by, however the bill is spelled.
func WalletPubkey(s string) (string, error) {
	tok, err := nipcash.Decode(s)
	if err != nil {
		return "", err
	}
	return tok.WalletPubkey, nil
}
