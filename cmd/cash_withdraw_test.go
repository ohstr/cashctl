package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nipCO"
	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/ohstr/nmilat/utils"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
	"github.com/ohstr/cashctl/internal/testmint"
)

const testAddress = "fc1qw508d6qejxtdg4y5r3zarvary0c5xw7kygt080"

// fakeWithdrawHub is one held bill's Hub: quotes signed by the Hub identity,
// withdrawals written ahead as nmilat does, then answered as told.
type fakeWithdrawHub struct {
	t                 *testing.T
	hubPriv, hubXOnly string
	wallet            string
	answer            func(pc nipcash.PendingCall) (*nipcash.Payout, error)
	lookup            func(pc nipcash.PendingCall) (nipcashclient.LookupOutcome, *nipcash.Payout, error)
	cancel            func() (*nipcash.Payout, error)
	sent              []nipcash.PendingCall
	quotes            int
}

func (f *fakeWithdrawHub) Close() {}

func (f *fakeWithdrawHub) QuoteWithdraw(_ context.Context, p nipcash.CashWithdrawQuoteParams) (*nipcash.WithdrawQuote, error) {
	f.quotes++
	fields := nipcash.QuoteFields{Wallet: f.wallet, IdentityValue: strings.Repeat("ee", 32), Address: p.Address,
		AmountMillis: 21000, NetworkFeeMillis: 1200, HubFeeMillis: 542, PayoutMillis: 19258,
		SentBy: time.Now().Unix() + 3600, HoldsUntil: 870413}
	ev, err := nipcash.NewWithdrawQuoteEvent(fields, time.Now().Unix(), func(ev *nip01.Event) error { return ev.Sign(f.hubPriv) })
	if err != nil {
		f.t.Fatal(err)
	}
	raw, _ := json.Marshal(ev)
	return nipcash.ParseWithdrawQuote(string(raw))
}

func (f *fakeWithdrawHub) Withdraw(ctx context.Context, p nipcash.CashWithdrawParams, j nipcashclient.Journal, device string) (*nipcash.Payout, error) {
	if p.RequestID == "" {
		p.RequestID, _ = nipcash.NewRequestID()
	}
	if p.Notify == "" {
		p.Notify, _ = nipCO.NewNotify()
	}
	pc, err := p.Pending(f.wallet)
	if errors.Is(err, nipcash.ErrRemintRequired) {
		p.Remint, _ = nipcash.NewCashTarget()
		pc, err = p.Pending(f.wallet)
	}
	if err != nil {
		return nil, err
	}
	pc.Deadline, pc.HubIdentity, pc.MaxRequestAge, pc.Device, pc.Chain = time.Now().Unix()+60, f.hubXOnly, 60, device, "flokicoin"
	if err := j.WriteAhead(ctx, []nipcash.PendingCall{pc}); err != nil {
		return nil, errors.Join(nipcashclient.ErrNotSent, err)
	}
	f.sent = append(f.sent, pc)
	if f.answer != nil {
		return f.answer(pc)
	}
	return f.payout(nipcash.PayoutQueued, p.Quote), nil
}

func (f *fakeWithdrawHub) payout(state nipcash.PayoutState, q *nipcash.WithdrawQuote) *nipcash.Payout {
	return &nipcash.Payout{State: state, Address: q.Address, AmountMillis: q.AmountMillis, FeeMillis: q.FeeMillis(),
		PayoutMillis: q.PayoutMillis, SentBy: q.SentBy, FinalDepth: 6, Receipt: "{}"}
}

func (f *fakeWithdrawHub) LookupWithdraw(_ context.Context, pc nipcash.PendingCall, _ nipcash.Credential) (nipcashclient.LookupOutcome, *nipcash.Payout, error) {
	if f.lookup == nil {
		return nipcashclient.LookupOutcome{NotServed: true}, nil, nil
	}
	return f.lookup(pc)
}

func (f *fakeWithdrawHub) CancelWithdraw(context.Context, nipcash.PendingCall, nipcash.Credential) (*nipcash.Payout, error) {
	return f.cancel()
}

// setUpWithdraw holds one protected cash note minted by the fake Hub.
func setUpWithdraw(t *testing.T) (*fakeWithdrawHub, string) {
	t.Helper()
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	priv, xonly := testmint.Key(t)
	wallet := testmint.Hex32(t)
	tok := testmint.Bill(t, nipcash.Token{HRP: "lokicash", WalletPubkey: wallet, Secret: testmint.Hex32(t),
		RelayURLs: []string{"wss://hub.example"}, IdentityRequired: ptrTo(false)}, priv)
	minter, _ := utils.GetPublicKey(priv)
	minter = "02" + minter
	l, _ := ledger.Load()
	e, err := l.Add(ledger.Entry{Token: tok, WalletPubkey: wallet, AmountMillis: ptrTo(uint64(21000)), Verified: true,
		MinterPubkey: &minter, IdentityRequired: ptrTo(false), CashSecret: testmint.Hex32(t), CashProtection: ledger.CashProtected})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	f := &fakeWithdrawHub{t: t, hubPriv: priv, hubXOnly: xonly, wallet: wallet}
	prev := dialWithdrawFn
	dialWithdrawFn = func(context.Context, string) (withdrawBill, error) { return f, nil }
	t.Cleanup(func() { dialWithdrawFn = prev })
	return f, e.ID
}

func withdrawCmd(c *cobra.Command, flags map[string]string) *cobra.Command {
	c.Flags().Bool("json", true, "")
	c.Flags().Bool("yes", false, "")
	c.Flags().String("connection", "", "")
	for k, v := range flags {
		if err := c.Flags().Set(k, v); err != nil {
			panic(err)
		}
	}
	return c
}

func runWithdrawQuote(t *testing.T) map[string]any {
	t.Helper()
	c := withdrawCmd(newCashWithdrawQuoteCmd(), nil)
	c.SetArgs([]string{testAddress})
	var err error
	stdout, _ := captureStreams(t, func() { err = c.Execute() })
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	var doc map[string]any
	_ = json.Unmarshal([]byte(stdout), &doc)
	return doc
}

func runWithdrawSend(t *testing.T, args []string, flags map[string]string) (map[string]any, error) {
	t.Helper()
	c := withdrawCmd(newCashWithdrawSendCmd(), flags)
	var err error
	stdout, _ := captureStreams(t, func() { err = runCashWithdrawSend(c, args) })
	var doc map[string]any
	_ = json.Unmarshal([]byte(stdout), &doc)
	return doc, err
}

// A quote moves nothing; send needs --quote and --yes without a terminal, and
// then writes the call (with the re-mint secret) and the withdrawal ahead; a
// queued payout spends the bill and leaves the call following the payout.
func TestWithdraw_QuoteThenSend(t *testing.T) {
	f, entryID := setUpWithdraw(t)
	doc := runWithdrawQuote(t)
	handle, _ := doc["quote"].(string)
	if !strings.HasPrefix(handle, "wq-") || doc["payout_millis"] != float64(19258) || len(f.sent) != 0 {
		t.Fatalf("quote %v", doc)
	}
	if _, err := runWithdrawSend(t, []string{testAddress}, nil); output.ExitCode(err) != 2 {
		t.Fatalf("send without a terminal or --quote: %v", err)
	}
	if _, err := runWithdrawSend(t, nil, map[string]string{"quote": handle}); output.ExitCode(err) != 2 {
		t.Fatalf("--quote without --yes: %v", err)
	}
	doc, err := runWithdrawSend(t, nil, map[string]string{"quote": handle, "yes": "true"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if doc["state"] != "queued" || len(f.sent) != 1 {
		t.Fatalf("send %v", doc)
	}
	l, _ := ledger.Load()
	e, _ := l.Find(entryID)
	if e.Status != ledger.StatusWithdrawn {
		t.Errorf("source %s, want withdrawn", e.Status)
	}
	c, _ := l.FindCall(ledger.CallIDFor(f.sent[0].RequestID))
	if c.State != ledger.CallOpenWithdraw || callSecretOf(c, ledger.RoleRemint) == "" {
		t.Errorf("call %s, remint kept %v", c.State, callSecretOf(c, ledger.RoleRemint) != "")
	}
	raw, _ := json.Marshal(doc)
	w, _, _ := ledger.FindWithdrawal(doc["withdrawal"].(string))
	if strings.Contains(string(raw), w.Notify) {
		t.Error("notify was printed")
	}
	// The quote is single-use.
	if _, err := runWithdrawSend(t, nil, map[string]string{"quote": handle, "yes": "true"}); output.ExitCode(err) != 4 {
		t.Errorf("reused quote: %v", err)
	}
}

// Interactively, anything but "yes" sends nothing.
func TestWithdraw_InteractiveNeedsYes(t *testing.T) {
	f, _ := setUpWithdraw(t)
	prevI, prevA := interactiveWithdraw, askYesFn
	interactiveWithdraw = func(*cobra.Command) bool { return true }
	answer := false
	askYesFn = func() bool { return answer }
	t.Cleanup(func() { interactiveWithdraw, askYesFn = prevI, prevA })
	if _, err := runWithdrawSend(t, []string{testAddress}, nil); err != nil || len(f.sent) != 0 {
		t.Fatalf("declined: %v, sent %d", err, len(f.sent))
	}
	answer = true
	if _, err := runWithdrawSend(t, []string{testAddress}, nil); err != nil || len(f.sent) != 1 {
		t.Fatalf("confirmed: %v, sent %d", err, len(f.sent))
	}
}

// Local refusals: a mixed-case address, and a shared cash note.
func TestWithdraw_LocalRefusals(t *testing.T) {
	_, entryID := setUpWithdraw(t)
	c := withdrawCmd(newCashWithdrawQuoteCmd(), nil)
	c.SetArgs([]string{"fc1QW508d6qejxtdg4y5r3zarvary0c5xw7kygt080"})
	var err error
	captureStreams(t, func() { err = c.Execute() })
	if ce := output.AsCLIError(err); ce == nil || ce.Reason != reasonAddressInvalid {
		t.Errorf("mixed case: %v", err)
	}
	l, _ := ledger.Load()
	e, _ := l.Find(entryID)
	e.CashProtection = ledger.CashShared
	_ = l.Save()
	c = withdrawCmd(newCashWithdrawQuoteCmd(), nil)
	c.SetArgs([]string{testAddress})
	captureStreams(t, func() { err = c.Execute() })
	if ce := output.AsCLIError(err); ce == nil || ce.Reason != reasonProtectFirst || output.ExitCode(err) != 2 {
		t.Errorf("shared note: %v", err)
	}
}

// A lost reply keeps the bill locked (exit 8); the lookup's payout then spends
// it; a final payout closes the call; a cancelled one brings the amount back
// as a new bill with the re-mint secret.
func TestWithdraw_FollowToFinalAndCancelled(t *testing.T) {
	for _, end := range []nipcash.PayoutState{nipcash.PayoutFinal, nipcash.PayoutCancelled} {
		t.Run(string(end), func(t *testing.T) {
			f, entryID := setUpWithdraw(t)
			handle := runWithdrawQuote(t)["quote"].(string)
			f.answer = func(pc nipcash.PendingCall) (*nipcash.Payout, error) {
				return nil, &nipcashclient.IndeterminateError{Call: pc, Cause: errors.New("no reply")}
			}
			if _, err := runWithdrawSend(t, nil, map[string]string{"quote": handle, "yes": "true"}); output.ExitCode(err) != 8 {
				t.Fatalf("lost reply: %v", err)
			}
			l, _ := ledger.Load()
			if e, _ := l.Find(entryID); e.Status != ledger.StatusHeld {
				t.Fatal("the bill was spent before any answer")
			}
			c, _ := l.FindCall(ledger.CallIDFor(f.sent[0].RequestID))
			c.Deadline = time.Now().Unix() - 1
			_ = l.Save()
			w, _, _ := ledger.WithdrawalForCall(c.CallID)
			q, _ := nipcash.ParseWithdrawQuote(w.QuoteJSON)
			p := f.payout(end, q)
			if end == nipcash.PayoutCancelled {
				remint, _ := nipcash.CashTargetFromSecret(callSecretOf(c, ledger.RoleRemint))
				_ = remint
				wallet := testmint.Hex32(t)
				p.NewWalletPubkey = wallet
				p.NewWalletToken = nipcash.TokenOrTombstone{Token: testmint.Bill(t, nipcash.Token{HRP: "lokicash", WalletPubkey: wallet,
					Secret: testmint.Hex32(t), RelayURLs: []string{"wss://hub.example"}, IdentityRequired: ptrTo(false)}, f.hubPriv)}
			}
			f.lookup = func(nipcash.PendingCall) (nipcashclient.LookupOutcome, *nipcash.Payout, error) {
				return nipcashclient.LookupOutcome{Status: nipcash.LookupDone}, p, nil
			}
			l = settleNow(t)
			c, _ = l.FindCall(c.CallID)
			w, _, _ = ledger.FindWithdrawal(w.WithdrawalID)
			if c.State != ledger.CallDone || w.FinishedAt == 0 || w.State != string(end) {
				t.Fatalf("call %s/%s, withdrawal %+v", c.State, c.Detail, w)
			}
			if e, _ := l.Find(entryID); e.Status != ledger.StatusWithdrawn {
				t.Errorf("source %s", e.Status)
			}
			if end == nipcash.PayoutCancelled {
				re, ok := l.Find(w.RemintEntryID)
				if !ok || re.Status != ledger.StatusHeld || re.CashSecret == "" {
					t.Errorf("re-minted entry %+v", re)
				}
			}
		})
	}
}

// A cancel is refused locally before 7 days after sent_by.
func TestWithdraw_CancelNotDue(t *testing.T) {
	f, _ := setUpWithdraw(t)
	handle := runWithdrawQuote(t)["quote"].(string)
	doc, _ := runWithdrawSend(t, nil, map[string]string{"quote": handle, "yes": "true"})
	id := doc["withdrawal"].(string)
	w, _, _ := ledger.FindWithdrawal(id)
	w.State = string(nipcash.PayoutBroadcast)
	_ = ledger.PutWithdrawal(w)
	f.cancel = func() (*nipcash.Payout, error) { t.Fatal("a cancel was sent"); return nil, nil }
	c := withdrawCmd(newCashWithdrawCancelCmd(), map[string]string{"yes": "true"})
	var err error
	captureStreams(t, func() { err = runCashWithdrawCancel(c, []string{id}) })
	if ce := output.AsCLIError(err); ce == nil || ce.Reason != reasonCancelNotDue {
		t.Errorf("cancel: %v", err)
	}
}

// W-1: `cash settle --close` never closes a withdrawal (its payout is still
// followed and its re-mint secret still needed), even with --json --yes; and
// --close of any other call under --json needs --yes.
func TestSettleClose_RefusesWithdrawalAndNeedsYes(t *testing.T) {
	f, _ := setUpWithdraw(t)
	handle := runWithdrawQuote(t)["quote"].(string)
	if _, err := runWithdrawSend(t, nil, map[string]string{"quote": handle, "yes": "true"}); err != nil {
		t.Fatal(err)
	}
	l, _ := ledger.Load()
	c, _ := l.FindCall(ledger.CallIDFor(f.sent[0].RequestID))
	var err error
	captureStreams(t, func() { err = closeCallByUser(testCmdWithFlags(true, true), l, c.CallID, "dev", true) })
	if output.ExitCode(err) != 2 || !strings.Contains(err.Error(), "cash withdraw show") {
		t.Fatalf("close of an open withdrawal: %v", err)
	}
	l, _ = ledger.Load()
	c, _ = l.FindCall(c.CallID)
	if c.State != ledger.CallOpenWithdraw || callSecretOf(c, ledger.RoleRemint) == "" {
		t.Fatalf("withdrawal call changed: %s, remint kept %v", c.State, callSecretOf(c, ledger.RoleRemint) != "")
	}

	// A plain open call: --json alone is not consent.
	c2, err := l.AddCall(ledger.Call{RequestID: strings.Repeat("ab", 32), Method: nipcash.MethodCashTransfer,
		State: ledger.CallLookup, DeviceID: "dev", Deadline: time.Now().Unix() + 60})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	l, _ = ledger.Load()
	captureStreams(t, func() { err = closeCallByUser(testCmdWithFlags(true, false), l, c2.CallID, "dev", true) })
	if output.ExitCode(err) != 2 {
		t.Fatalf("--close under --json without --yes: %v", err)
	}
	captureStreams(t, func() { err = closeCallByUser(testCmdWithFlags(true, true), l, c2.CallID, "dev", true) })
	if err != nil {
		t.Fatalf("--close --json --yes: %v", err)
	}
}

// W-2: a withdraw call whose withdrawal row was never written (a stop between
// the two writes) never went out: settle closes it and frees the bill.
func TestWithdraw_CallWithoutRowIsNotSent(t *testing.T) {
	_, entryID := setUpWithdraw(t)
	l, _ := ledger.Load()
	device, _ := deviceIDFn()
	c, err := l.AddCall(ledger.Call{RequestID: strings.Repeat("cd", 32), Method: nipcash.MethodCashWithdraw,
		State: ledger.CallLookup, DeviceID: device, Deadline: time.Now().Unix() - 1, SourceEntryIDs: []string{entryID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	l = settleNow(t)
	got, _ := l.FindCall(c.CallID)
	if got.State != ledger.CallNotApplied {
		t.Fatalf("call %s/%s, want not_applied", got.State, got.Detail)
	}
	if _, open := l.OpenCallFor(entryID); open {
		t.Error("the bill is still locked")
	}
}

// W-3: a re-placement the Hub refuses ends the withdrawal row, not only the
// call.
func TestWithdraw_RefusedReplacementEndsRow(t *testing.T) {
	f, entryID := setUpWithdraw(t)
	handle := runWithdrawQuote(t)["quote"].(string)
	f.answer = func(pc nipcash.PendingCall) (*nipcash.Payout, error) {
		return nil, &nipcashclient.IndeterminateError{Call: pc, Cause: errors.New("no reply")}
	}
	if _, err := runWithdrawSend(t, nil, map[string]string{"quote": handle, "yes": "true"}); output.ExitCode(err) != 8 {
		t.Fatalf("lost reply: %v", err)
	}
	l, _ := ledger.Load()
	c, _ := l.FindCall(ledger.CallIDFor(f.sent[0].RequestID))
	c.Deadline = time.Now().Unix() - 1
	_ = l.Save()
	f.lookup = func(nipcash.PendingCall) (nipcashclient.LookupOutcome, *nipcash.Payout, error) {
		return nipcashclient.LookupOutcome{Status: nipcash.LookupUnknown}, nil, nil
	}
	f.answer = func(nipcash.PendingCall) (*nipcash.Payout, error) {
		return nil, &nipcashclient.FinalError{Method: nipcash.MethodCashWithdraw, Code: "BAD_REQUEST", Message: "no"}
	}
	l = settleNow(t)
	if len(f.sent) != 2 {
		pc, _ := l.FindCall(c.CallID)
		t.Fatalf("sent %d, want a re-placement; parent %s/%s", len(f.sent), pc.State, pc.Detail)
	}
	child, _ := l.FindCall(ledger.CallIDFor(f.sent[1].RequestID))
	w, ok, _ := ledger.WithdrawalForCall(child.CallID)
	if child.State != ledger.CallNotApplied || !ok || w.State != ledger.WithdrawNotApplied || w.FinishedAt == 0 {
		t.Errorf("child %s, withdrawal %+v", child.State, w)
	}
	if e, _ := l.Find(entryID); e.Status != ledger.StatusHeld {
		t.Errorf("source %s, want held", e.Status)
	}
}

// W-4: a tombstone answer spends the bill and shows the withdrawal as
// bill_gone, never "your cash is still yours".
func TestWithdraw_TombstoneIsBillGone(t *testing.T) {
	f, entryID := setUpWithdraw(t)
	handle := runWithdrawQuote(t)["quote"].(string)
	f.answer = func(nipcash.PendingCall) (*nipcash.Payout, error) {
		return nil, &nipcash.BillGone{Method: nipcash.MethodCashWithdraw, Tombstone: nipcash.Tombstone{Reason: "transferred"}}
	}
	if _, err := runWithdrawSend(t, nil, map[string]string{"quote": handle, "yes": "true"}); output.ExitCode(err) != 10 {
		t.Fatalf("tombstone: %v", err)
	}
	l, _ := ledger.Load()
	if e, _ := l.Find(entryID); e.Status == ledger.StatusHeld {
		t.Error("the bill is still held")
	}
	w, ok, _ := ledger.WithdrawalForCall(ledger.CallIDFor(f.sent[0].RequestID))
	if !ok || w.State != ledger.WithdrawBillGone || strings.Contains(withdrawalText(w), "still yours") {
		t.Errorf("withdrawal %s: %q", w.State, withdrawalText(w))
	}
}

// A bill whose redeem may still settle is never withdrawn (redeem_in_flight),
// named or picked.
func TestWithdraw_RefusesARedeemingBill(t *testing.T) {
	_, entryID := setUpWithdraw(t)
	l, _ := ledger.Load()
	e, _ := l.Find(entryID)
	e.Status = ledger.StatusRedeeming
	_ = l.Save()
	for _, flags := range []map[string]string{nil, {"token": entryID}} {
		c := withdrawCmd(newCashWithdrawQuoteCmd(), flags)
		c.SetArgs([]string{testAddress})
		var err error
		captureStreams(t, func() { err = c.Execute() })
		if output.AsCLIError(err) == nil || output.ExitCode(err) == 0 {
			t.Errorf("quote of a redeeming bill (%v): %v, want refused", flags, err)
		}
		if flags != nil && output.AsCLIError(err).Reason != reasonRedeemInFlight {
			t.Errorf("named redeeming bill: %v, want redeem_in_flight", err)
		}
	}
}
