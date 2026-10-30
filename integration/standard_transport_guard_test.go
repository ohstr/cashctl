// No build tag: a pure unit test, no live stack. It locks
// isForbiddenOnStandardTransport to the two lists in NIP-CASH's transport
// table so the attack-surface test can never drift from the spec.
package integration

import "testing"

// TestStandardTransportGuard_MatchesSpecTable pins the classification to
// NIP-CASH §"It is the ONLY transport for the bill methods" (:2011-2024):
// the seven bill methods are private-ONLY (forbidden on kind 23194), while
// the standard-transport methods a bill connection serves — get_info first
// among them — are allowed.
func TestStandardTransportGuard_MatchesSpecTable(t *testing.T) {
	forbidden := []string{
		"cash_status",
		"cash_redeem",
		"cash_withdraw_quote",
		"cash_withdraw",
		"cash_transfer",
		"cash_consolidate",
		"cash_lookup",
	}
	allowed := []string{
		"get_info", // the spec-required receive-time network read (:3163-3165)
		"get_balance",
		"pay_invoice",
		"mint_cash",
	}

	for _, m := range forbidden {
		if !isForbiddenOnStandardTransport(m) {
			t.Errorf("%q must be forbidden on the standard transport (NIP-CASH bill method, private ONLY)", m)
		}
	}
	for _, m := range allowed {
		if isForbiddenOnStandardTransport(m) {
			t.Errorf("%q must be allowed on the standard transport", m)
		}
	}
}
