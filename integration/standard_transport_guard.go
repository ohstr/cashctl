// standard_transport_guard.go holds the spec classification the attack-surface
// test uses to tell a forbidden bill method apart from a method a bill
// connection legitimately serves on the shared (kind-23194) transport. The NWC
// request decoder that reads a captured method lives in the integration-tagged
// standard_transport_decode.go, since only the live attack test uses it.
//
// No build tag: this file and its unit test compile and run without the
// live stack, so the classification can be pinned to the NIP-CASH table on
// its own.
package integration

import (
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
