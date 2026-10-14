package output

import (
	"testing"

	relayclient "github.com/ohstr/nmilat/relay/client"
)

// NIP-CW §Creating a Circle Wallet: a not-ready Hub and a throttled request are
// each a distinct error the caller can retry.
func TestNWCError_JoinRefusalsThatAreRetryable(t *testing.T) {
	for _, tt := range []struct {
		nwcCode string
		want    ErrorCode
	}{
		{"NOT_READY", CodeNetwork},
		{"RATE_LIMITED", CodeConflict},
	} {
		ce := AsCLIError(NWCError(newTestCmd(), &relayclient.WalletError{Method: "create_circle_wallet", Code: tt.nwcCode, Message: "x"}))
		if ce.Code != tt.want || !retryableCodes[ce.Code] {
			t.Errorf("%s: code = %q (retryable=%v), want %q, retryable", tt.nwcCode, ce.Code, retryableCodes[ce.Code], tt.want)
		}
		if ce.NWCCode != tt.nwcCode {
			t.Errorf("%s: nwc_code = %q, want it kept", tt.nwcCode, ce.NWCCode)
		}
	}
}
