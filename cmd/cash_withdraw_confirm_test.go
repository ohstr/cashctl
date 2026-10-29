package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// interactive answers the confirmation with yes and the fee's second question
// with fee, counting how often that one is asked.
func interactive(t *testing.T, fee bool) *int {
	t.Helper()
	asked := 0
	prevI, prevA, prevF := interactiveWithdraw, askYesFn, askFeeFn
	interactiveWithdraw = func(*cobra.Command) bool { return true }
	askYesFn = func() bool { return true }
	askFeeFn = func(string) bool { asked++; return fee }
	t.Cleanup(func() { interactiveWithdraw, askYesFn, askFeeFn = prevI, prevA, prevF })
	return &asked
}

// markOwn records the bill as made by this device's own call.
func markOwn(t *testing.T, entryID string) {
	t.Helper()
	l, _ := ledger.Load()
	e, _ := l.Find(entryID)
	e.CreatedByCall = "c-own"
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
}

// T-96, plan Q-19: a fee above 25 % of the bill asks a second time; at 25 %
// it does not. Declining the second question sends nothing.
func TestWithdraw_FeeAboveAQuarterAsksTwice(t *testing.T) {
	for _, c := range []struct {
		name       string
		networkFee uint64 // fee = networkFee + 542 of 21000
		feeAnswer  bool
		wantAsked  int
		wantSent   int
	}{
		{"25 % exactly: one question", 4708, false, 0, 1},
		{"25.005 %: asked, declined", 4709, false, 1, 0},
		{"25.005 %: asked, confirmed", 4709, true, 1, 1},
		{"a small fee: one question", 1200, false, 0, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, _ := setUpWithdraw(t)
			f.networkFee = c.networkFee
			asked := interactive(t, c.feeAnswer)
			if _, err := runWithdrawSend(t, []string{testAddress}, nil); err != nil {
				t.Fatalf("send: %v", err)
			}
			if *asked != c.wantAsked || len(f.sent) != c.wantSent {
				t.Errorf("asked %d, sent %d; want %d, %d", *asked, len(f.sent), c.wantAsked, c.wantSent)
			}
			if c.wantSent == 0 {
				l, _ := ledger.Load()
				if len(l.Calls) != 0 {
					t.Errorf("a declined send wrote %d call(s)", len(l.Calls))
				}
			}
		})
	}
}

// Under --quote --yes the fee is a warning, never a second question.
func TestWithdraw_FeeAboveAQuarterIsAWarningUnderYes(t *testing.T) {
	f, entryID := setUpWithdraw(t)
	markOwn(t, entryID)
	f.networkFee = 4709
	asked := interactive(t, false)
	interactiveWithdraw = func(*cobra.Command) bool { return false }
	doc := runWithdrawQuote(t)
	warnings, _ := doc["warnings"].([]any)
	if len(warnings) != 1 || !strings.Contains(warnings[0].(string), "on-chain costs 5.251 loki of 21 loki (25.0 %)") {
		t.Fatalf("quote warnings %v", warnings)
	}
	if _, err := runWithdrawSend(t, nil, map[string]string{"quote": doc["quote"].(string), "yes": "true"}); err != nil || len(f.sent) != 1 || *asked != 0 {
		t.Fatalf("send: %v, sent %d, asked %d", err, len(f.sent), *asked)
	}
}

// S-R-22: a bill others may know the amount of is warned about; one this
// device made by its own call is not.
func TestWithdraw_PrivacyWarning(t *testing.T) {
	const privacy = "others may know this bill's amount"
	t.Run("a received bill", func(t *testing.T) {
		setUpWithdraw(t)
		if w := strings.Join(warningsOf(runWithdrawQuote(t)), "|"); !strings.Contains(w, privacy) {
			t.Errorf("warnings %q", w)
		}
	})
	t.Run("a zap bill made here", func(t *testing.T) {
		_, entryID := setUpWithdraw(t)
		markOwn(t, entryID)
		l, _ := ledger.Load()
		e, _ := l.Find(entryID)
		e.ZapEvent = `{"kind":9522}`
		_ = l.Save()
		if w := strings.Join(warningsOf(runWithdrawQuote(t)), "|"); !strings.Contains(w, privacy) {
			t.Errorf("warnings %q", w)
		}
	})
	t.Run("a bill this device minted", func(t *testing.T) {
		_, entryID := setUpWithdraw(t)
		markOwn(t, entryID)
		if w := warningsOf(runWithdrawQuote(t)); len(w) != 0 {
			t.Errorf("warnings %q, want none", w)
		}
	})
}

func warningsOf(doc map[string]any) []string {
	var out []string
	for _, w := range doc["warnings"].([]any) {
		out = append(out, w.(string))
	}
	return out
}

// T-96: the screen shows the block the price holds until and, when cash_status
// quotes it, what Lightning would land.
func TestWithdraw_ScreenHoldsUntilAndLightningLine(t *testing.T) {
	for _, c := range []struct {
		name string
		net  uint64
		line string
	}{
		{"Lightning lands more", 20895, "Lightning would land 20.895 loki (1.637 loki more): cashctl redeem"},
		{"Lightning lands less", 19000, "Lightning would land 19 loki\n"},
		{"not quoted", 0, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, entryID := setUpWithdraw(t)
			if c.net > 0 {
				l, _ := ledger.Load()
				e, _ := l.Find(entryID)
				commit, err := nipcash.CashCommitment(e.CashSecret)
				if err != nil {
					t.Fatal(err)
				}
				f.status = &nipcash.CashStatusResult{Recipients: []nipcash.RecipientStatus{{IdentityType: "cash", IdentityValue: commit, AmountMillis: 21000, NetRedeemableMillis: c.net}}}
			}
			cmd := withdrawCmd(newCashWithdrawQuoteCmd(), map[string]string{"json": "false"})
			cmd.SetArgs([]string{testAddress})
			var err error
			_, stderr := captureStreams(t, func() { err = cmd.Execute() })
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(stderr, "Price held until block 870413") {
				t.Errorf("no holds_until line:\n%s", stderr)
			}
			if got := strings.Contains(stderr, "Lightning would land"); got != (c.line != "") || c.line != "" && !strings.Contains(stderr, c.line) {
				t.Errorf("want %q:\n%s", c.line, stderr)
			}
		})
	}
}

// Plan WP-20 step 6: an answer without a valid receipt keeps the call open and
// the bill locked; two in a row make it stuck/receipt_invalid, looked up only
// hourly and shown as such, never asked again at every command. A valid
// answer after that is taken.
func TestWithdraw_InvalidReceiptIsBoundedThenSurfaced(t *testing.T) {
	f, entryID := setUpWithdraw(t)
	handle := runWithdrawQuote(t)["quote"].(string)
	f.answer = func(pc nipcash.PendingCall) (*nipcash.Payout, error) {
		return nil, &nipcashclient.IndeterminateError{Call: pc, Cause: nipcash.ErrReceiptInvalid}
	}
	_, err := runWithdrawSend(t, nil, map[string]string{"quote": handle, "yes": "true"})
	if ce := output.AsCLIError(err); ce == nil || output.ExitCode(err) != 8 || ce.Reason != reasonReceiptInvalid {
		t.Fatalf("an invalid receipt in the reply: %v", err)
	}
	lookups := 0
	var valid *nipcash.Payout
	f.lookup = func(nipcash.PendingCall) (nipcashclient.LookupOutcome, *nipcash.Payout, error) {
		lookups++
		if valid != nil {
			return nipcashclient.LookupOutcome{Status: nipcash.LookupDone}, valid, nil
		}
		return nipcashclient.LookupOutcome{Malformed: true}, nil, nil
	}
	l, _ := ledger.Load()
	c, _ := l.FindCall(ledger.CallIDFor(f.sent[0].RequestID))
	c.Deadline = time.Now().Unix() - 1
	_ = l.Save()

	l = settleNow(t)
	c, _ = l.FindCall(c.CallID)
	w, _, _ := ledger.WithdrawalForCall(c.CallID)
	if lookups != 1 || c.State != ledger.CallStuck || c.Detail != reasonReceiptInvalid || c.RetryAfter == nil || *c.RetryAfter < time.Now().Unix()+3500 {
		t.Fatalf("after two answers without a receipt: lookups %d, call %s/%s retry %v", lookups, c.State, c.Detail, c.RetryAfter)
	}
	if e, _ := l.Find(entryID); e.Status != ledger.StatusHeld {
		t.Errorf("the source is %s; an invalid receipt spends nothing", e.Status)
	}
	if v := withdrawalView(w); v["receipt_invalid"] != true || withdrawalText(w) != receiptInvalidText {
		t.Errorf("shown as %v / %q", v["receipt_invalid"], withdrawalText(w))
	}
	if next := nextStep(c, c.DeviceID); !strings.Contains(next, "never send this bill again") {
		t.Errorf("next step %q", next)
	}
	// Not asked again before its hour.
	settleNow(t)
	if lookups != 1 {
		t.Errorf("looked up %d times within the hour", lookups)
	}
	// An hour on, a valid answer is taken and the count starts again.
	l, _ = ledger.Load()
	c, _ = l.FindCall(c.CallID)
	past := time.Now().Unix() - 1
	c.RetryAfter = &past
	_ = l.Save()
	q, _ := nipcash.ParseWithdrawQuote(w.QuoteJSON)
	valid = f.payout(nipcash.PayoutQueued, q)
	l = settleNow(t)
	c, _ = l.FindCall(c.CallID)
	w, _, _ = ledger.WithdrawalForCall(c.CallID)
	if lookups != 2 || c.State != ledger.CallOpenWithdraw || w.ReceiptInvalidSeen != 0 || w.State != string(nipcash.PayoutQueued) {
		t.Errorf("a valid answer: lookups %d, call %s, withdrawal %s seen %d", lookups, c.State, w.State, w.ReceiptInvalidSeen)
	}
}

// One answer without a valid receipt alone is asked again at once, not stuck.
func TestWithdraw_OneInvalidReceiptIsAskedAgain(t *testing.T) {
	f, _ := setUpWithdraw(t)
	handle := runWithdrawQuote(t)["quote"].(string)
	f.answer = func(pc nipcash.PendingCall) (*nipcash.Payout, error) {
		return nil, &nipcashclient.IndeterminateError{Call: pc, Cause: nipcash.ErrReceiptInvalid}
	}
	_, _ = runWithdrawSend(t, nil, map[string]string{"quote": handle, "yes": "true"})
	l, _ := ledger.Load()
	c, _ := l.FindCall(ledger.CallIDFor(f.sent[0].RequestID))
	w, _, _ := ledger.WithdrawalForCall(c.CallID)
	if c.State == ledger.CallStuck || w.ReceiptInvalidSeen != 1 || withdrawalText(w) == receiptInvalidText {
		t.Errorf("after one: call %s, seen %d, text %q", c.State, w.ReceiptInvalidSeen, withdrawalText(w))
	}
}
