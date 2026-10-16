package cmd

import (
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nipcash"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// F-43: a tombstone marks the entry spent, so it leaves everything that offers held
// bills (wallet balance, redeem --all), and the error is the distinct spent code.
func TestRecordSpent_MarksAHeldEntryAndDropsItFromHeld(t *testing.T) {
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	l, err := ledger.Load()
	if err != nil {
		t.Fatal(err)
	}
	amt := uint64(1000)
	e, err := l.Add(ledger.Entry{Token: "tok", WalletPubkey: "pub", AmountMillis: &amt})
	if err != nil {
		t.Fatal(err)
	}
	err = recordSpent(&cobra.Command{}, l, e)
	if ce := output.AsCLIError(err); ce.Code != output.CodeSpent {
		t.Errorf("code = %q, want spent", ce.Code)
	}
	fresh, _ := ledger.Load()
	if got, _ := fresh.Find(e.ID); got.Status != ledger.StatusSpent {
		t.Errorf("saved status = %q, want spent", got.Status)
	}
	if len(fresh.Held()) != 0 {
		t.Errorf("a spent bill is still held: %+v", fresh.Held())
	}
}

// `cash status` (and redeem/consolidate) on a bill you redeemed or merged
// away answers spent, as the Hub's tombstone would; one you transferred away
// still exists under its new holder, so it is not_found.
func TestAlreadySpentError_SpentForEveryBillWhoseValueMovedOnTheHub(t *testing.T) {
	cmd := &cobra.Command{}
	for status, want := range map[string]output.ErrorCode{
		ledger.StatusSpent:        output.CodeSpent,
		ledger.StatusRedeemed:     output.CodeSpent,
		ledger.StatusConsolidated: output.CodeSpent,
		ledger.StatusTransferred:  output.CodeNotFound,
	} {
		if c := output.AsCLIError(alreadySpentError(cmd, "tok", status)).Code; c != want {
			t.Errorf("%s entry: code = %q, want %q", status, c, want)
		}
	}
}

// End to end through cash status's own resolver: `cash status --token`
// on a bill you just redeemed is spent (exit 10), with no network call.
func TestCashStatus_ATokenYouRedeemedIsSpent(t *testing.T) {
	l := heldLedger(2)
	l.Entries[1].Status = ledger.StatusRedeemed
	cmd := testCmdWithFlags(true, false)
	cmd.Flags().String("token", l.Entries[1].ID, "")
	_, err := resolveHeldTokensOrAll(cmd, l)
	ce := output.AsCLIError(err)
	if ce.Code != output.CodeSpent || output.ExitCode(err) != 10 {
		t.Fatalf("code = %q (exit %d), want spent (exit 10): %v", ce.Code, output.ExitCode(err), err)
	}
}

func TestCashStatusRowStatus_SpentIsNotOK(t *testing.T) {
	if got := cashStatusRowStatus(&nipcash.CashStatusResult{Error: nipcash.ErrorSpent}); got != "spent" {
		t.Errorf("tombstone row status = %q, want spent", got)
	}
	if got := cashStatusRowStatus(&nipcash.CashStatusResult{}); got != "ok" {
		t.Errorf("live row status = %q, want ok", got)
	}
}

// I-22: a Hub refusing to answer about a cash-mode bill without its secret (or
// with the wrong one) is explained as that, not as a generic bad request.
func TestClassifyCashTokenNWCErr_ExplainsAMissingOrWrongCashSecret(t *testing.T) {
	for msg, want := range map[string]string{
		"this item carries neither a slice proof nor a cash secret": "<token>#<secret>",
		"no slice of this bill matches this cash secret":            "does not match",
	} {
		err := classifyCashTokenNWCErr(&cobra.Command{}, &relayclient.WalletError{Code: "BAD_REQUEST", Message: msg})
		ce := output.AsCLIError(err)
		if ce.Code != output.CodeInvalidInput {
			t.Errorf("%q: code = %q, want invalid_input (not retryable)", msg, ce.Code)
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v does not say %q", msg, err, want)
		}
	}
	other := &relayclient.WalletError{Code: "BAD_REQUEST", Message: "something else"}
	if _, ok := explainCashSecretRefusal(other); ok {
		t.Error("an unrelated BAD_REQUEST was explained as a cash-secret refusal")
	}
}

// The first `cash status` on a bill whose Hub has just tombstoned it is the
// same spent error (exit 10, nothing on stdout) every later call gets from
// the stored status — never the raw tombstone printed as a result.
func TestRunCashStatus_AFreshTombstoneIsSpentOnTheFirstCall(t *testing.T) {
	for _, jsonMode := range []bool{true, false} {
		appdir.SetOverride(t.TempDir())
		t.Cleanup(func() { appdir.SetOverride("") })
		l, err := ledger.Load()
		if err != nil {
			t.Fatal(err)
		}
		amt := uint64(1000)
		e, err := l.Add(ledger.Entry{Token: "tok", WalletPubkey: "pub", AmountMillis: &amt})
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Save(); err != nil {
			t.Fatal(err)
		}
		real := fetchCashStatusFn
		t.Cleanup(func() { fetchCashStatusFn = real })
		fetchCashStatusFn = func(*cobra.Command, bool, *ledger.Entry) (*nipcash.CashStatusResult, error) {
			return &nipcash.CashStatusResult{Error: nipcash.ErrorSpent}, nil
		}
		cmd := newTestStatusCmd(jsonMode, false)
		_ = cmd.Flags().Set("token", e.ID)

		var runErr error
		stdout, _ := captureStreams(t, func() { runErr = runCashStatus(cmd, nil) })
		if ce := output.AsCLIError(runErr); ce.Code != output.CodeSpent || output.ExitCode(runErr) != 10 {
			t.Errorf("json=%v: code = %q (exit %d), want spent (exit 10): %v", jsonMode, ce.Code, output.ExitCode(runErr), runErr)
		}
		if strings.TrimSpace(stdout) != "" {
			t.Errorf("json=%v: stdout = %q, want empty", jsonMode, stdout)
		}
		fresh, _ := ledger.Load()
		if got, _ := fresh.Find(e.ID); got.Status != ledger.StatusSpent {
			t.Errorf("json=%v: saved status = %q, want spent", jsonMode, got.Status)
		}
	}
}

// Several bills: a tombstone stays a "spent" row in the full report, so the
// other rows are not hidden behind it.
func TestRunCashStatus_ATombstoneAmongSeveralStaysARow(t *testing.T) {
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	l, err := ledger.Load()
	if err != nil {
		t.Fatal(err)
	}
	amt := uint64(1000)
	spent, _ := l.Add(ledger.Entry{Token: "tok-a", WalletPubkey: "pub", AmountMillis: &amt})
	if _, err := l.Add(ledger.Entry{Token: "tok-b", WalletPubkey: "pub", AmountMillis: &amt}); err != nil {
		t.Fatal(err)
	}
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	real := fetchCashStatusFn
	t.Cleanup(func() { fetchCashStatusFn = real })
	fetchCashStatusFn = func(_ *cobra.Command, _ bool, e *ledger.Entry) (*nipcash.CashStatusResult, error) {
		if e.ID == spent.ID {
			return &nipcash.CashStatusResult{Error: nipcash.ErrorSpent}, nil
		}
		return &nipcash.CashStatusResult{}, nil
	}
	stdout, _ := captureStreams(t, func() { _ = runCashStatus(newTestStatusCmd(true, false), nil) })
	if !strings.Contains(stdout, `"spent"`) || !strings.Contains(stdout, `"ok"`) {
		t.Errorf("multi-token report lost a row: %s", stdout)
	}
}
