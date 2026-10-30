//go:build integration

// standard_transport_decode.go holds the NWC request decoder the attack-surface
// integration test uses to read a captured kind-23194 request's method. It is
// integration-tagged because only the live attack test uses it; the tag-free
// standard_transport_guard.go keeps the spec classification its own unit test pins.
package integration

import (
	"encoding/json"
	"fmt"

	"github.com/ohstr/nmilat/nip44"
)

// decodeNWCMethod NIP-44-v2-decrypts a captured NWC request's content with the
// bill's connection secret and wallet pubkey, then returns its "method".
//
// A kind-23194 request is encrypted client→wallet, so the conversation key is
// derived from the bill's Secret (the connection private key) and its
// WalletPubkey — exactly what nip44.DecryptHex takes.
func decodeNWCMethod(content, secretHex, walletPubkeyHex string) (string, error) {
	plaintext, err := nip44.DecryptHex(content, secretHex, walletPubkeyHex)
	if err != nil {
		return "", fmt.Errorf("nip-44 v2 decrypt: %w", err)
	}
	var req struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal([]byte(plaintext), &req); err != nil {
		return "", fmt.Errorf("parse NWC request %q: %w", plaintext, err)
	}
	return req.Method, nil
}
