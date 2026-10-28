package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/flokiorg/go-flokicoin/chainutil/bech32"
	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/dial"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
	"github.com/ohstr/cashctl/internal/testmint"
)

// sendQueued quotes and sends the fixture's bill, leaving a queued payout.
func sendQueued(t *testing.T) (*fakeWithdrawHub, string, ledger.Withdrawal) {
	t.Helper()
	f, entryID := setUpWithdraw(t)
	handle := runWithdrawQuote(t)["quote"].(string)
	doc, err := runWithdrawSend(t, nil, map[string]string{"quote": handle, "yes": "true"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	w, _, _ := ledger.FindWithdrawal(doc["withdrawal"].(string))
	return f, entryID, w
}

func lookupAnswers(f *fakeWithdrawHub, w ledger.Withdrawal, states ...nipcash.PayoutState) {
	q, _ := nipcash.ParseWithdrawQuote(w.QuoteJSON)
	i := 0
	f.lookup = func(nipcash.PendingCall) (nipcashclient.LookupOutcome, *nipcash.Payout, error) {
		st := states[min(i, len(states)-1)]
		i++
		p := f.payout(st, q)
		if st != nipcash.PayoutQueued {
			p.Txid, p.Vout, p.Depth = strings.Repeat("ab", 32), ptrTo(uint32(0)), ptrTo(uint32(1))
		}
		p.Receipt = `{"kind":23188}`
		return nipcashclient.LookupOutcome{Status: nipcash.LookupDone}, p, nil
	}
}

func dueNow(t *testing.T, callID string) {
	t.Helper()
	l, _ := ledger.Load()
	c, _ := l.FindCall(callID)
	c.RetryAfter = nil
	c.Deadline = time.Now().Unix() - 1
	_ = l.Save()
}

// S-CP-R9: the save that records final says where it was received once, then
// keeps neither the address, quote, receipt nor transaction, nor the secrets.
func TestWithdraw_FinalRewrite(t *testing.T) {
	f, entryID, w := sendQueued(t)
	addr := w.Address
	lookupAnswers(f, w, nipcash.PayoutFinal)
	dueNow(t, w.CallID)
	l, _ := ledger.Load()
	c, _ := l.FindCall(w.CallID)
	summary, got := settleWithdrawOne(withdrawCmd(&cobra.Command{}, nil), l, c, "")
	if !strings.Contains(summary, "Received at "+addr) || got.ReceivedAt != addr {
		t.Errorf("summary %q", summary)
	}
	w, _, _ = ledger.FindWithdrawal(w.WithdrawalID)
	if w.Address != "" || w.QuoteJSON != "" || strings.Contains(w.PayoutJSON, "23188") || strings.Contains(w.PayoutJSON, addr) {
		t.Errorf("final row keeps %+v", w)
	}
	l, _ = ledger.Load()
	e, _ := l.Find(entryID)
	c, _ = l.FindCall(w.CallID)
	if e.CashSecret != "" || c.AuthorizerSecret != "" || c.TargetCredential != "" || len(c.Secrets) != 0 {
		t.Errorf("secrets kept: entry %q, call %q %q", e.CashSecret, c.AuthorizerSecret, c.TargetCredential)
	}
}

// --tx prints a transaction only when it pays the address exactly; --receipt
// prints the receipt.
func TestWithdraw_ShowTxAndReceipt(t *testing.T) {
	f, _, w := sendQueued(t)
	q, _ := nipcash.ParseWithdrawQuote(w.QuoteJSON)
	f.lookup = func(nipcash.PendingCall) (nipcashclient.LookupOutcome, *nipcash.Payout, error) {
		p := f.payout(nipcash.PayoutBroadcast, q)
		p.Txid, p.Vout, p.Depth, p.Signed, p.Tx = strings.Repeat("ab", 32), ptrTo(uint32(0)), ptrTo(uint32(0)), true, "00"
		p.Receipt = `{"kind":23188,"note":"receipt"}`
		return nipcashclient.LookupOutcome{Status: nipcash.LookupDone}, p, nil
	}
	dueNow(t, w.CallID)
	c := withdrawCmd(newCashWithdrawShowCmd(), map[string]string{"tx": "true", "receipt": "true"})
	c.SetArgs([]string{w.WithdrawalID})
	var err error
	stdout, _ := captureStreams(t, func() { err = c.Execute() })
	if err != nil {
		t.Fatal(err)
	}
	var doc struct{ Withdrawals []map[string]any }
	_ = json.Unmarshal([]byte(stdout), &doc)
	v := doc.Withdrawals[0]
	if v["tx_check"] != "mismatch" || v["tx"] != nil || !strings.Contains(v["receipt"].(string), "receipt") {
		t.Errorf("view %v", v)
	}
}

// watch reads on each notice and stops at final.
func TestWithdraw_WatchFollowsNotices(t *testing.T) {
	f, _, w := sendQueued(t)
	lookupAnswers(f, w, nipcash.PayoutBroadcast, nipcash.PayoutConfirmed, nipcash.PayoutFinal)
	prev := [3]time.Duration{withdrawPollBeforeBroadcast, withdrawPollAfterBroadcast, withdrawNoticeGap}
	withdrawPollBeforeBroadcast, withdrawPollAfterBroadcast, withdrawNoticeGap = time.Hour, time.Hour, 0
	t.Cleanup(func() {
		withdrawPollBeforeBroadcast, withdrawPollAfterBroadcast, withdrawNoticeGap = prev[0], prev[1], prev[2]
	})
	f.notices = make(chan struct{}, 4)
	f.notices <- struct{}{}
	f.notices <- struct{}{}
	c := withdrawCmd(newCashWithdrawWatchCmd(), map[string]string{"timeout": "5s"})
	var err error
	stdout, _ := captureStreams(t, func() { err = watchWithdrawal(c, w.WithdrawalID, 5*time.Second) })
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if !strings.Contains(stdout, `"final"`) || !strings.Contains(stdout, w.Address) {
		t.Errorf("watch ended %s", stdout)
	}
}

// interactiveSend runs send <address> with a terminal, the prompt answered
// by answers and the clock advanced by advance at each prompt.
func interactiveSend(t *testing.T, answers []bool, advance time.Duration) (int, error) {
	t.Helper()
	prevI, prevA, prevN, prevW := interactiveWithdraw, askYesFn, nowFn, waitFn
	clock := time.Now()
	nowFn = func() time.Time { return clock }
	waitFn = func(time.Duration) {}
	asks := 0
	interactiveWithdraw = func(*cobra.Command) bool { return true }
	askYesFn = func() bool {
		a := answers[min(asks, len(answers)-1)]
		asks++
		clock = clock.Add(advance)
		return a
	}
	t.Cleanup(func() { interactiveWithdraw, askYesFn, nowFn, waitFn = prevI, prevA, prevN, prevW })
	_, err := runWithdrawSend(t, []string{testAddress}, map[string]string{"json": "false"})
	return asks, err
}

// A prompt open over 120 s quotes again: the same numbers are sent without
// asking again; changed numbers are shown for a new yes.
func TestWithdraw_RequoteAfterAnOpenPrompt(t *testing.T) {
	f, _ := setUpWithdraw(t)
	asks, err := interactiveSend(t, []bool{true}, 3*time.Minute)
	if err != nil || asks != 1 || f.quotes != 2 || len(f.sent) != 1 {
		t.Fatalf("same numbers: err %v asks %d quotes %d sent %d", err, asks, f.quotes, len(f.sent))
	}

	// The second quote pays less: shown, marked, and asked again.
	f, _ = setUpWithdraw(t)
	calls := 0
	prevD := dialWithdrawFn
	dialWithdrawFn = func(context.Context, string) (withdrawBill, error) {
		calls++
		if calls > 1 {
			f.payoutMillis = 18000
		}
		return f, nil
	}
	t.Cleanup(func() { dialWithdrawFn = prevD })
	asks, err = interactiveSend(t, []bool{true, true}, 3*time.Minute)
	if err != nil || asks < 2 || len(f.sent) != 1 || f.sent[0].Amount != 21000 {
		t.Fatalf("changed numbers: err %v asks %d sent %d", err, asks, len(f.sent))
	}
}

// quote_expired: confirmed not applied by its lookup; without a terminal exit
// 7 with the detail; with one, a new quote and a new call after a new yes.
// The withdrawal remembers it, so an automatic re-place never sends it again.
func TestWithdraw_QuoteExpired(t *testing.T) {
	expire := func(f *fakeWithdrawHub) {
		n := 0
		f.answer = func(pc nipcash.PendingCall) (*nipcash.Payout, error) {
			n++
			if n == 1 {
				pc.LastCode = "RESTRICTED"
				return nil, &nipcashclient.IndeterminateError{Call: pc, Code: "RESTRICTED", Detail: string(nipcash.DetailQuoteExpired)}
			}
			q := &nipcash.WithdrawQuote{}
			q.Address, q.AmountMillis, q.PayoutMillis = testAddress, 21000, 19258
			return f.payout(nipcash.PayoutQueued, q), nil
		}
		f.lookup = func(nipcash.PendingCall) (nipcashclient.LookupOutcome, *nipcash.Payout, error) {
			return nipcashclient.LookupOutcome{Status: nipcash.LookupUnknown}, nil, nil
		}
	}
	f, _ := setUpWithdraw(t)
	expire(f)
	prevW := waitFn
	waitFn = func(time.Duration) {}
	t.Cleanup(func() { waitFn = prevW })
	handle := runWithdrawQuote(t)["quote"].(string)
	_, err := runWithdrawSend(t, nil, map[string]string{"quote": handle, "yes": "true"})
	if ce := output.AsCLIError(err); ce == nil || output.ExitCode(err) != 7 || ce.Detail != "quote_expired" {
		t.Fatalf("non-interactive: %v", err)
	}
	ws, _ := ledger.Withdrawals()
	if len(ws) != 1 || !ws[0].QuoteExpiredSeen || ws[0].State != ledger.WithdrawNotApplied {
		t.Errorf("withdrawal %+v", ws)
	}

	f, _ = setUpWithdraw(t)
	expire(f)
	asks, err := interactiveSend(t, []bool{true, true}, 0)
	if err != nil || asks != 2 || len(f.sent) != 2 || f.sent[0].RequestID == f.sent[1].RequestID {
		t.Fatalf("interactive: err %v asks %d sent %d", err, asks, len(f.sent))
	}
}

// After a quote_expired, a lost withdrawal the Hub never saw is not re-placed
// on cashctl's own initiative.
func TestWithdraw_NoAutomaticReplaceAfterQuoteExpired(t *testing.T) {
	f, _ := setUpWithdraw(t)
	handle := runWithdrawQuote(t)["quote"].(string)
	f.answer = func(pc nipcash.PendingCall) (*nipcash.Payout, error) {
		return nil, &nipcashclient.IndeterminateError{Call: pc, Cause: errors.New("no reply")}
	}
	_, _ = runWithdrawSend(t, nil, map[string]string{"quote": handle, "yes": "true"})
	ws, _ := ledger.Withdrawals()
	w := ws[0]
	w.QuoteExpiredSeen = true
	_ = ledger.PutWithdrawal(w)
	dueNow(t, w.CallID)
	f.lookup = func(nipcash.PendingCall) (nipcashclient.LookupOutcome, *nipcash.Payout, error) {
		return nipcashclient.LookupOutcome{Status: nipcash.LookupUnknown}, nil, nil
	}
	l := settleNow(t)
	c, _ := l.FindCall(w.CallID)
	if c.State != ledger.CallStuck || c.Detail != "quote_expired" || len(f.sent) != 1 {
		t.Errorf("call %s/%s, sent %d", c.State, c.Detail, len(f.sent))
	}
}

// XQ-29(2): a cash_status that names a withdrawal this device has no record of
// records it (recovered, never re-placed), spends the bill, and follows it.
func TestWithdraw_RecoveredFromWithdrawRequestID(t *testing.T) {
	f, entryID := setUpWithdraw(t)
	rid, _ := nipcash.NewRequestID()
	prev := fetchCashStatusFn
	fetchCashStatusFn = func(*cobra.Command, bool, *ledger.Entry) (*nipcash.CashStatusResult, error) {
		return &nipcash.CashStatusResult{Tombstone: &nipcash.Tombstone{Status: nipcash.StatusSpent, WithdrawRequestID: rid}}, nil
	}
	t.Cleanup(func() { fetchCashStatusFn = prev })
	c := withdrawCmd(newCashStatusCmd(), nil)
	c.SetArgs(nil)
	captureStreams(t, func() { _ = c.Execute() })
	l, _ := ledger.Load()
	e, _ := l.Find(entryID)
	call, ok := l.FindCall(ledger.CallIDFor(rid))
	if e.Status != ledger.StatusWithdrawn || !ok || call.DeviceID != ledger.DeviceRecovered {
		t.Fatalf("entry %s, call %+v", e.Status, call)
	}
	w, _, _ := ledger.WithdrawalForCall(call.CallID)
	if !w.Recovered {
		t.Fatal("not marked recovered")
	}
	q := &nipcash.WithdrawQuote{}
	q.Address, q.AmountMillis, q.PayoutMillis = testAddress, 21000, 19258
	f.lookup = func(nipcash.PendingCall) (nipcashclient.LookupOutcome, *nipcash.Payout, error) {
		return nipcashclient.LookupOutcome{Status: nipcash.LookupDone}, f.payout(nipcash.PayoutQueued, q), nil
	}
	settleWithdrawCall(withdrawCmd(&cobra.Command{}, nil), l, call, "")
	w, _, _ = ledger.FindWithdrawal(w.WithdrawalID)
	if w.State != "queued" || w.PayoutMillis != 19258 || w.Address != testAddress {
		t.Errorf("recovered withdrawal %+v", w)
	}
	f.lookup = func(nipcash.PendingCall) (nipcashclient.LookupOutcome, *nipcash.Payout, error) {
		return nipcashclient.LookupOutcome{Status: nipcash.LookupUnknown}, nil, nil
	}
	if len(f.sent) != 0 {
		t.Error("a recovered withdrawal was sent")
	}
}

// decode reads a quote, a payout and a witness address, and sniffs each.
func TestDecode_WithdrawArtefacts(t *testing.T) {
	f, _ := setUpWithdraw(t)
	q, _ := f.QuoteWithdraw(context.Background(), nipcash.CashWithdrawQuoteParams{Address: testAddress})
	raw, _ := q.Raw()
	if dial.SniffJSON(raw) != dial.KindWithdrawEvent {
		t.Fatal("quote not sniffed")
	}
	doc := runDecodeArgs(t, []string{raw}, map[string]string{"hub": f.hubXOnly})
	if doc["type"] != "withdraw_quote" || doc["payout_millis"] != float64(19258) || doc["hub_verified"] != true {
		t.Errorf("quote %v", doc)
	}
	p, _ := json.Marshal(f.payout(nipcash.PayoutQueued, q))
	if dial.SniffJSON(string(p)) != dial.KindPayout {
		t.Fatal("payout not sniffed")
	}
	if doc := runDecodeArgs(t, []string{string(p)}, nil); doc["type"] != "payout" || doc["state"] != "queued" {
		t.Errorf("payout %v", doc)
	}
	prog, _ := bech32.ConvertBits(make([]byte, 20), 8, 5, true)
	addr, err := bech32.Encode("fc", append([]byte{0}, prog...))
	if err != nil {
		t.Fatal(err)
	}
	if dial.Sniff(addr) != dial.KindWitnessAddress {
		t.Fatal("address not sniffed")
	}
	if doc := runDecodeArgs(t, []string{addr}, nil); doc["type"] != "address" || doc["script_name"] != "P2WPKH" {
		t.Errorf("address %v", doc)
	}
}

func runDecodeArgs(t *testing.T, args []string, flags map[string]string) map[string]any {
	t.Helper()
	c := newDecodeCmd()
	c.Flags().Bool("json", true, "")
	for k, v := range flags {
		_ = c.Flags().Set(k, v)
	}
	c.SetArgs(args)
	var err error
	stdout, _ := captureStreams(t, func() { err = c.Execute() })
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var doc map[string]any
	_ = json.Unmarshal([]byte(stdout), &doc)
	return doc
}

// W-5: a re-quote of another bill is never "the same quote", whatever its
// amounts, so it needs a new yes.
func TestSameQuote_ComparesTheBill(t *testing.T) {
	a := &nipcash.WithdrawQuote{QuoteFields: nipcash.QuoteFields{Wallet: "w1", Address: "addr", AmountMillis: 1000, PayoutMillis: 900}}
	b := *a
	if !sameQuote(a, &b) {
		t.Fatal("identical quotes differ")
	}
	b.Wallet = "w2"
	if sameQuote(a, &b) {
		t.Error("another bill's quote counted as the same")
	}
}

// W-7: a recovered cash-mode withdrawal the Hub cancels is re-minted to a
// commitment this device has no secret for: it is never held with the old one.
func TestImportRemint_RecoveredCashNoteIsNotHeld(t *testing.T) {
	f, entryID := setUpWithdraw(t)
	wallet := testmint.Hex32(t)
	tok := testmint.Bill(t, nipcash.Token{HRP: "lokicash", WalletPubkey: wallet, Secret: testmint.Hex32(t),
		RelayURLs: []string{"wss://hub.example"}, IdentityRequired: ptrTo(false)}, f.hubPriv)
	l, _ := ledger.Load()
	w := &ledger.Withdrawal{WithdrawalID: "wd-1", SourceEntryID: entryID, HubIdentity: f.hubXOnly, AmountMillis: 21000}
	p := &nipcash.Payout{State: nipcash.PayoutCancelled, NewWalletPubkey: wallet, NewWalletToken: nipcash.TokenOrTombstone{Token: tok}}
	id, err := importRemint(l, &ledger.Call{}, w, p)
	if err != nil || id != "" {
		t.Fatalf("importRemint = %q, %v; want nothing held", id, err)
	}
	if _, ok := l.FindByToken(strings.ToLower(tok)); ok {
		t.Error("the re-minted bill was held with the old secret")
	}
}

// W-6: quote_invalid is handled as quote_expired: confirmed not applied, then
// only a new quote goes on.
func TestWithdraw_QuoteInvalidIsLikeExpired(t *testing.T) {
	f, _ := setUpWithdraw(t)
	f.answer = func(pc nipcash.PendingCall) (*nipcash.Payout, error) {
		pc.LastCode = "RESTRICTED"
		return nil, &nipcashclient.IndeterminateError{Call: pc, Code: "RESTRICTED", Detail: string(nipcash.DetailQuoteInvalid)}
	}
	f.lookup = func(nipcash.PendingCall) (nipcashclient.LookupOutcome, *nipcash.Payout, error) {
		return nipcashclient.LookupOutcome{Status: nipcash.LookupUnknown}, nil, nil
	}
	prevW := waitFn
	waitFn = func(time.Duration) {}
	t.Cleanup(func() { waitFn = prevW })
	handle := runWithdrawQuote(t)["quote"].(string)
	_, err := runWithdrawSend(t, nil, map[string]string{"quote": handle, "yes": "true"})
	if output.ExitCode(err) != 7 {
		t.Fatalf("quote_invalid: %v, want the quote refused (exit 7)", err)
	}
	ws, _ := ledger.Withdrawals()
	if len(ws) != 1 || !ws[0].QuoteExpiredSeen || ws[0].State != ledger.WithdrawNotApplied {
		t.Errorf("withdrawal %+v", ws)
	}
}

// R2-C4 (W-6): every coded step-3 refusal, not only quote_expired, is looked
// up once after the deadline in the same run: unknown ends it not applied,
// with its code, instead of exit 8 until a later settle.
func TestWithdraw_CodedRefusalIsSettledInTheSameRun(t *testing.T) {
	for _, tc := range []struct{ code, detail string }{
		{"RATE_LIMITED", ""}, {"INSUFFICIENT_BALANCE", ""}, {"RESTRICTED", string(nipcash.DetailOwnAddress)},
	} {
		t.Run(tc.code+tc.detail, func(t *testing.T) {
			f, _ := setUpWithdraw(t)
			f.answer = func(pc nipcash.PendingCall) (*nipcash.Payout, error) {
				pc.LastCode = tc.code
				return nil, &nipcashclient.IndeterminateError{Call: pc, Code: tc.code, Detail: tc.detail}
			}
			looked := 0
			f.lookup = func(nipcash.PendingCall) (nipcashclient.LookupOutcome, *nipcash.Payout, error) {
				looked++
				return nipcashclient.LookupOutcome{Status: nipcash.LookupUnknown}, nil, nil
			}
			prevW := waitFn
			waitFn = func(time.Duration) {}
			t.Cleanup(func() { waitFn = prevW })
			handle := runWithdrawQuote(t)["quote"].(string)
			_, err := runWithdrawSend(t, nil, map[string]string{"quote": handle, "yes": "true"})
			if ce := output.AsCLIError(err); ce == nil || output.ExitCode(err) == 8 || ce.Detail != tc.detail {
				t.Fatalf("err %v: want a final refusal with detail %q, not exit 8", err, tc.detail)
			}
			ws, _ := ledger.Withdrawals()
			if looked != 1 || len(ws) != 1 || ws[0].State != ledger.WithdrawNotApplied {
				t.Errorf("looked up %d, withdrawals %+v", looked, ws)
			}
		})
	}
}
