package cmd

import (
	"errors"
	"testing"

	"github.com/ohstr/nmilat/nipcash"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// A call that failed definitely — the Hub refused it, or it was never sent —
// created no bill, so its parked destination secret is dropped and saved: a
// refused merge used to leave `wallet show` reporting an interrupted send
// forever. An indeterminate failure keeps it, as the only copy of a bill's
// secret that may exist.
func TestForgetIfNothingCreated(t *testing.T) {
	cmd := &cobra.Command{}
	cases := []struct {
		name    string
		failure error
		keep    bool
	}{
		{"hub refused", output.NWCErrorForCashToken(cmd, &relayclient.WalletError{Code: "BAD_REQUEST", Message: "all sources must belong to the same Cash Hub"}), false},
		{"never sent", output.NetworkError(cmd, errors.New("dial failed")), false},
		{"no answer", output.IndeterminateError(cmd, "", errors.New("timed out")), true},
		{"unreadable reply", errors.New("failed to decrypt delivery"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			auditDTempLedger(t)
			l, ids := auditDSeedTwoHeldCash(t)
			target := nipcash.NewCashTarget()
			parked, err := parkDestinationCashSecret(l, ids, target)
			if err != nil {
				t.Fatal(err)
			}
			parked.forgetIfNothingCreated(c.failure)

			fresh, err := ledger.Load()
			if err != nil {
				t.Fatal(err)
			}
			kept := len(unfinishedCashSends(fresh)) > 0
			if kept != c.keep {
				t.Errorf("parked secret kept = %v on disk, want %v", kept, c.keep)
			}
		})
	}
}
