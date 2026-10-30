// standard_transport_guard.go holds the spec classification and the NWC
// request decoder the attack-surface test uses to tell a forbidden bill
// method apart from a method a bill connection legitimately serves on the
// shared (kind-23194) transport.
//
// No build tag: this file and its unit test compile and run without the
// live stack, so the classification can be pinned to the NIP-CASH table on
// its own.
package integration

import (
	"encoding/json"
	"fmt"

	"github.com/ohstr/nmilat/nip44"
	"github.com/ohstr/nmilat/nipcash"
)

// isForbiddenOnStandardTransport reports whether method is one of the bill
// methods NIP-CASH §"It is the ONLY transport for the bill methods"
// (:2011-2024) keeps to the private transport (kind 23190) and refuses with
// NOT_IMPLEMENTED on the standard transport (kind 23194).
//
// Only those seven are forbidden here. Everything else — get_info (the
// spec-required receive-time network read, NIP-CASH §Network and Chain
// :3163-3165), get_balance, pay_invoice, mint_cash, … — rides the standard
// transport unchanged.
func isForbiddenOnStandardTransport(method string) bool {
	switch method {
	case nipcash.MethodCashStatus,
		nipcash.MethodCashRedeem,
		nipcash.MethodCashWithdrawQuote,
		nipcash.MethodCashWithdraw,
		nipcash.MethodCashTransfer,
		nipcash.MethodCashConsolidate,
		nipcash.MethodCashLookup:
		return true
	default:
		return false
	}
}

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
