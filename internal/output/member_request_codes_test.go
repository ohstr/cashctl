package output

import (
	"testing"

	"github.com/ohstr/nmilat/nipcw"
	relayclient "github.com/ohstr/nmilat/relay/client"
)

// NIP-CW's member-request codes: FROZEN is auth (exit 7), ROTATION_PENDING a
// conflict that retrying as is never fixes (exit 5, not retryable).
func TestNWCError_MemberRequestCodes(t *testing.T) {
	for code, want := range map[string]int{nipcw.ErrFrozen: 7, nipcw.ErrRotationPending: 5} {
		err := NWCError(nil, &relayclient.WalletError{Method: "pay_invoice", Code: code, Message: "x"})
		ce := AsCLIError(err)
		if ce == nil || ExitCode(err) != want || ce.NWCCode != code {
			t.Errorf("%s: exit %d, want %d (%+v)", code, ExitCode(err), want, ce)
		}
		if code == nipcw.ErrRotationPending && !ce.Final {
			t.Errorf("%s is retryable", code)
		}
	}
}
