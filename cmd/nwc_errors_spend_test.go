package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/output"
)

// A timeout alone MUST NOT be reported as spent or expired (NIP-CASH §Cash Status).
func TestClassifyCashTokenNWCErr_TimeoutIsNeutralAndRetryable(t *testing.T) {
	err := classifyCashTokenNWCErr(&cobra.Command{}, fmt.Errorf("wait: %w", context.DeadlineExceeded))
	ce := output.AsCLIError(err)
	if ce.Code != output.CodeNetwork {
		t.Errorf("code = %q, want network: a read is safe to repeat", ce.Code)
	}
	if msg := strings.ToLower(err.Error()); strings.Contains(msg, "spent") || strings.Contains(msg, "expired") {
		t.Errorf("a timeout is reported as spent/expired: %v", err)
	}
}

func TestClassifyCashSpendErr(t *testing.T) {
	cmd := &cobra.Command{}
	tests := []struct {
		name string
		err  error
		want output.ErrorCode
	}{
		{"timeout", context.DeadlineExceeded, output.CodeIndeterminate},
		{"omission", &nipcashclient.NotServedError{Method: "cash_transfer"}, output.CodeIndeterminate},
		{"incomplete reply", nipcashclient.ErrIncompleteReply, output.CodeIndeterminate},
		{"dial failed", &notSentError{errors.New("dial: refused")}, output.CodeNetwork},
		{"hub decline", &relayclient.WalletError{Code: "EXPIRED", Message: "gone"}, output.CodeAuth},
		{"refused before send", fmt.Errorf("consolidate: %w", nipcash.ErrCashSource), output.CodeInvalidInput},
		{"no private transport", nipcashclient.ErrNoAnnouncement, output.CodeNetwork},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := classifyCashSpendErr(cmd, tt.err, "transfer", "tok-a")
			if got := output.AsCLIError(err).Code; got != tt.want {
				t.Errorf("code = %q, want %q", got, tt.want)
			}
			if tt.want == output.CodeIndeterminate && !strings.Contains(err.Error(), "cash status --token tok-a") {
				t.Errorf("no cash status hint: %v", err)
			}
		})
	}
}
