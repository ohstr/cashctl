package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nip57/nip57test"
	"github.com/ohstr/nmilat/nipCO"
	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
	"github.com/ohstr/cashctl/internal/store"
	"github.com/ohstr/cashctl/internal/testmint"
)

// fakeOrderHub stands in for nipCO/client: Place writes the order ahead as
// nmilat does, then answers as told; Lookup answers as told.
type fakeOrderHub struct {
	t                 *testing.T
	hubPriv, hubXOnly string
	terms             nipCO.Terms
	// answer is Place's outcome after the write-ahead: nil returns an open order.
	answer func(pc nipcash.PendingCall) error
	lookup func(pc nipcash.PendingCall) (nipcashclient.LookupOutcome, *nipCO.Order, error)
	placed []nipcash.PendingCall
	// writtenBeforeSend: the call and its order row were on disk when sent.
	writtenBeforeSend bool
	// refuse fails Place before anything is written or sent.
	refuse error
}

func (f *fakeOrderHub) Terms(context.Context) (*nipCO.Terms, error) { return &f.terms, nil }
func (f *fakeOrderHub) Network() string                             { return "mainnet" }
func (f *fakeOrderHub) Close()                                      {}

func (f *fakeOrderHub) Place(ctx context.Context, p nipCO.CashOrderParams, j nipcashclient.Journal, device string) (*nipCO.Order, error) {
	if f.refuse != nil {
		return nil, f.refuse
	}
	if p.RequestID == "" {
		p.RequestID, _ = nipcash.NewRequestID()
	}
	req, err := p.Request()
	if err != nil {
		return nil, err
	}
	params, _ := json.Marshal(req)
	pc := nipcash.PendingCall{RequestID: p.RequestID, Deadline: time.Now().Unix() + 120, Method: nipcash.MethodCashOrder,
		Target: strings.Repeat("ab", 32), HubIdentity: f.hubXOnly, MaxRequestAge: 60, Device: device, Amount: p.AmountMillis,
		Chain: "flokicoin", Params: params, Notify: p.Notify}
	if t, ok := p.Recipient.(*nipcash.CashTarget); ok {
		pc.Secrets = []nipcash.PendingSecret{{Secret: t.Secret()}}
	} else {
		pc.BuyerKey, _, _ = nipCO.NewBuyerKey()
	}
	if err := j.WriteAhead(ctx, []nipcash.PendingCall{pc}); err != nil {
		return nil, errors.Join(nipcashclient.ErrNotSent, err)
	}
	if l, err := ledger.Load(); err == nil {
		if c, ok := l.FindCall(ledger.CallIDFor(pc.RequestID)); ok {
			if o, ok, _ := ledger.OrderForCall(c.CallID); ok && o.Notify == p.Notify {
				f.writtenBeforeSend = true
			}
		}
	}
	f.placed = append(f.placed, pc)
	if f.answer != nil {
		if err := f.answer(pc); err != nil {
			return nil, err
		}
	}
	if p.Payment == nipCO.PaymentOnchain {
		return f.onchain(p.AmountMillis, nipCO.OrderOpen, 0), nil
	}
	return f.open(p.AmountMillis, time.Now().Unix()+600), nil
}

// testOrderAddress is a valid mainnet flokicoin witness address.
const testOrderAddress = "fc1qqypqxpq9qcrsszg2pvxq6rs0zqg3yyc5nuq0zs"

// onchain is an on-chain order in state, with bills minted for it, each
// signed by the Hub.
func (f *fakeOrderHub) onchain(amount uint64, state nipCO.OrderState, bills int) *nipCO.Order {
	o := &nipCO.Order{State: state, Payment: nipCO.PaymentOnchain, AmountMillis: amount, FeeMillis: 1000,
		PayMillis: amount + 1000, Address: testOrderAddress, Confirmations: 1, PayBy: time.Now().Unix() + 3600}
	for i := 0; i < bills; i++ {
		o.Bills = append(o.Bills, f.billFor(amount))
	}
	return o
}

func (f *fakeOrderHub) billFor(amount uint64) nipCO.OrderBill {
	wallet := testmint.Hex32(f.t)
	tok := testmint.Bill(f.t, nipcash.Token{HRP: "lokicash", WalletPubkey: wallet, Secret: testmint.Hex32(f.t),
		RelayURLs: []string{"wss://hub.example"}, IdentityRequired: ptrTo(false)}, f.hubPriv)
	return nipCO.OrderBill{WalletPubkey: wallet, Token: nipcash.TokenOrTombstone{Token: tok}, AmountMillis: amount, PaidMillis: amount + 1000}
}

func (f *fakeOrderHub) open(amount uint64, payBy int64) *nipCO.Order {
	return &nipCO.Order{State: nipCO.OrderOpen, Payment: nipCO.PaymentLightning, AmountMillis: amount, FeeMillis: 1000,
		PayMillis: amount + 1000, Invoice: testInvoice(f.t, f.hubPriv, "fc", amount+1000, payBy), PayBy: payBy}
}

// testInvoice is a signed BOLT11 invoice of millis on currency that expires
// at expiresAt.
func testInvoice(t *testing.T, nodePriv, currency string, millis uint64, expiresAt int64) string {
	t.Helper()
	now := time.Now().Unix()
	inv, err := nip57test.SignedBolt11(nip57test.Invoice{Currency: currency, AmountMillis: millis,
		PaymentHash: mustHex(t, testmint.Hex32(t)), Timestamp: now, Expiry: expiresAt - now, NodePriv: nodePriv})
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

// minted is the order as it stands once paid and minted, its bill signed by
// the Hub.
func (f *fakeOrderHub) minted(amount uint64) *nipCO.Order {
	o := f.open(amount, time.Now().Unix()+600)
	o.State = nipCO.OrderMinted
	wallet := testmint.Hex32(f.t)
	tok := testmint.Bill(f.t, nipcash.Token{HRP: "lokicash", WalletPubkey: wallet, Secret: testmint.Hex32(f.t),
		RelayURLs: []string{"wss://hub.example"}, IdentityRequired: ptrTo(false)}, f.hubPriv)
	o.Bills = []nipCO.OrderBill{{WalletPubkey: wallet, Token: nipcash.TokenOrTombstone{Token: tok}, AmountMillis: amount, PaidMillis: amount + 1000}}
	return o
}

func (f *fakeOrderHub) Lookup(_ context.Context, pc nipcash.PendingCall) (nipcashclient.LookupOutcome, *nipCO.Order, error) {
	if f.lookup == nil {
		return nipcashclient.LookupOutcome{NotServed: true}, nil, nil
	}
	return f.lookup(pc)
}

func setUpOrderHub(t *testing.T) (*fakeOrderHub, string) {
	t.Helper()
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	priv, xonly := testmint.Key(t)
	f := &fakeOrderHub{t: t, hubPriv: priv, hubXOnly: xonly, terms: nipCO.Terms{
		Lightning: &nipCO.KindTerms{MinMillis: 1000, MaxMillis: 1_000_000_000, FeeBase: 1000, Window: 600}}}
	prev := dialOrderFn
	dialOrderFn = func(context.Context, string) (orderHub, error) { return f, nil }
	t.Cleanup(func() { dialOrderFn = prev })
	return f, testmint.CashOrder(t, priv)
}

func orderCmd(c *cobra.Command, flags map[string]string) *cobra.Command {
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

func placeOrder(t *testing.T, conn, amount string) (map[string]any, error) {
	t.Helper()
	c := orderCmd(newCashOrderPlaceCmd(), nil)
	var err error
	stdout, _ := captureStreams(t, func() { err = runCashOrderPlace(c, []string{conn, amount}) })
	var doc map[string]any
	_ = json.Unmarshal([]byte(stdout), &doc)
	return doc, err
}

func settleOrders(t *testing.T, flags map[string]string) error {
	t.Helper()
	c := orderCmd(newCashOrderSettleCmd(), flags)
	var err error
	captureStreams(t, func() { err = runCashOrderSettle(c, nil) })
	return err
}

func onlyOrder(t *testing.T) (ledger.Order, *ledger.Call) {
	t.Helper()
	orders, _ := ledger.Orders()
	if len(orders) != 1 {
		t.Fatalf("orders %+v", orders)
	}
	l, _ := ledger.Load()
	c, _ := l.FindCall(orders[0].CallID)
	return orders[0], c
}

// T-84/T-86: the order is written ahead (call and order row, notify included)
// before it is sent; the reply leaves it open_order with the invoice shown and
// no secret, buyer key or notify printed.
func TestOrderPlace_WritesAheadAndShowsWhatToPay(t *testing.T) {
	f, conn := setUpOrderHub(t)
	doc, err := placeOrder(t, conn, "100")
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if !f.writtenBeforeSend {
		t.Error("the order went out before its call and row were written")
	}
	if inv, _ := doc["invoice"].(string); !strings.HasPrefix(inv, "lnfc") || doc["state"] != "open" || len(doc["warnings"].([]any)) == 0 {
		t.Errorf("--json %v", doc)
	}
	o, c := onlyOrder(t)
	if c.State != ledger.CallOpenOrder || len(c.Secrets) != 1 || o.PayBy == 0 {
		t.Errorf("call %s, secrets %d, order %+v", c.State, len(c.Secrets), o)
	}
	raw, _ := json.Marshal(doc)
	if strings.Contains(string(raw), o.Notify) || strings.Contains(string(raw), c.Secrets[0].Secret) {
		t.Error("notify or the cash secret reached stdout")
	}
}

// A retired connection sells nothing (exit 7 not_selling); an amount under the
// minimum is exit 3; neither writes anything.
func TestOrderPlace_RefusedBeforeWriting(t *testing.T) {
	f, conn := setUpOrderHub(t)
	_, err := placeOrder(t, conn, "0.5")
	if output.ExitCode(err) != 3 {
		t.Errorf("below the minimum: %v", err)
	}
	f.terms = nipCO.Terms{}
	_, err = placeOrder(t, conn, "100")
	if ce := output.AsCLIError(err); ce == nil || output.ExitCode(err) != 7 || ce.Reason != reasonNotSelling {
		t.Errorf("retired: %v", err)
	}
	if orders, _ := ledger.Orders(); len(orders) != 0 || len(f.placed) != 0 {
		t.Error("something was written or sent")
	}
}

// A lost reply is exit 8; the order is then read, and a minted bill for a cash
// recipient is kept with the order's secret; the order finishes.
func TestOrder_LostReplyThenMinted(t *testing.T) {
	f, conn := setUpOrderHub(t)
	f.answer = func(pc nipcash.PendingCall) error {
		return &nipcashclient.IndeterminateError{Call: pc, Cause: errors.New("no reply")}
	}
	if _, err := placeOrder(t, conn, "100"); output.ExitCode(err) != 8 {
		t.Fatalf("lost reply: %v", err)
	}
	_, c := onlyOrder(t)
	secret := c.Secrets[0].Secret
	f.lookup = func(nipcash.PendingCall) (nipcashclient.LookupOutcome, *nipCO.Order, error) {
		return nipcashclient.LookupOutcome{Status: nipcash.LookupDone}, f.minted(100_000), nil
	}
	if err := settleOrders(t, nil); err != nil {
		t.Fatalf("settle: %v", err)
	}
	o, c := onlyOrder(t)
	l, _ := ledger.Load()
	held := l.Held()
	if c.State != ledger.CallDone || o.FinishedAt == 0 || o.State != string(nipCO.OrderMinted) {
		t.Errorf("call %s, order %+v", c.State, o)
	}
	if len(held) != 1 || held[0].CashSecret != secret || held[0].AmountMillis == nil || *held[0].AmountMillis != 100_000 {
		t.Errorf("held %+v", held)
	}
	// Reading it again adds nothing twice.
	_ = settleOrders(t, nil)
	if l, _ := ledger.Load(); len(l.Held()) != 1 {
		t.Error("a bill was recorded twice")
	}
}

// An order the Hub never saw waits for --replace, which places it again under
// the derived id (with a fresh notify) and moves the order to that call.
func TestOrder_NeverSeenNeedsReplace(t *testing.T) {
	f, conn := setUpOrderHub(t)
	f.answer = func(pc nipcash.PendingCall) error {
		return &nipcashclient.IndeterminateError{Call: pc, Cause: errors.New("no reply")}
	}
	_, _ = placeOrder(t, conn, "100")
	o, c := onlyOrder(t)
	l, _ := ledger.Load()
	call, _ := l.FindCall(c.CallID)
	call.Deadline = time.Now().Unix() - 1
	_ = l.Save()
	f.lookup = func(nipcash.PendingCall) (nipcashclient.LookupOutcome, *nipCO.Order, error) {
		return nipcashclient.LookupOutcome{Status: nipcash.LookupUnknown}, nil, nil
	}
	_ = settleOrders(t, nil)
	if _, c := onlyOrder(t); c.State != ledger.CallStuck || c.Detail != "confirm_replace" {
		t.Fatalf("after unknown: %s/%s", c.State, c.Detail)
	}
	if err := settleOrders(t, map[string]string{"replace": o.OrderID}); output.ExitCode(err) != 2 {
		t.Fatalf("--replace under --json without --yes: %v", err)
	}
	f.answer = nil
	if err := settleOrders(t, map[string]string{"replace": o.OrderID, "yes": "true"}); err != nil {
		t.Fatalf("--replace: %v", err)
	}
	moved, child := onlyOrder(t)
	derived, _ := nipcash.DerivedRequestID(c.RequestID)
	if child.RequestID != derived || child.State != ledger.CallOpenOrder || moved.Notify == o.Notify || moved.OrderID != o.OrderID {
		t.Errorf("after --replace: call %+v, order %+v", child, moved)
	}
}

// R-CO-10: an open order past pay_by that the Hub no longer knows, never seen
// paid, ended unpaid.
func TestOrder_EndedUnpaid(t *testing.T) {
	f, conn := setUpOrderHub(t)
	_, _ = placeOrder(t, conn, "100")
	o, _ := onlyOrder(t)
	o.PayBy = time.Now().Unix() - 10
	_ = ledger.PutOrder(o)
	f.lookup = func(nipcash.PendingCall) (nipcashclient.LookupOutcome, *nipCO.Order, error) {
		return nipcashclient.LookupOutcome{Status: nipcash.LookupUnknown}, nil, nil
	}
	if err := settleOrders(t, nil); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if o, c := onlyOrder(t); o.State != ledger.OrderEndedUnpaid || c.State != ledger.CallDone {
		t.Errorf("order %s, call %s", o.State, c.State)
	}
}

// The order row is written in the same transaction as its call: when the row
// can't be written, neither is the call, and nothing is sent.
func TestOrderPlace_RowAndCallAreOneTransaction(t *testing.T) {
	f, conn := setUpOrderHub(t)
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_orders BEFORE INSERT ON orders BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := placeOrder(t, conn, "100"); err == nil {
		t.Fatal("placed with an order row that can't be written")
	}
	l, _ := ledger.Load()
	if len(l.Calls) != 0 || len(f.placed) != 0 {
		t.Errorf("calls %d, placed %d: want neither", len(l.Calls), len(f.placed))
	}
}

// validateOrder decodes the invoice: its amount is pay_millis, it expires by
// pay_by, and it is on the Hub's chain.
func TestValidateOrder_ChecksTheInvoice(t *testing.T) {
	priv, _ := testmint.Key(t)
	payBy := time.Now().Unix() + 600
	o := &ledger.Order{Payment: nipCO.PaymentLightning, AmountMillis: 100_000, Chain: "flokicoin"}
	order := func(inv string) *nipCO.Order {
		return &nipCO.Order{Payment: nipCO.PaymentLightning, AmountMillis: 100_000, FeeMillis: 1000, PayMillis: 101_000, PayBy: payBy, Invoice: inv}
	}
	if err := validateOrder(o, order(testInvoice(t, priv, "fc", 101_000, payBy)), 0); err != nil {
		t.Fatalf("a good invoice: %v", err)
	}
	for name, inv := range map[string]string{
		"another amount": testInvoice(t, priv, "fc", 100_000, payBy),
		"amountless":     testInvoice(t, priv, "fc", 0, payBy),
		"expires late":   testInvoice(t, priv, "fc", 101_000, payBy+60),
		"expires early":  testInvoice(t, priv, "fc", 101_000, payBy-400),
		"unknown prefix": testInvoice(t, priv, "xx", 101_000, payBy),
		"another chain":  testInvoice(t, priv, "bc", 101_000, payBy),
		"not an invoice": "lnfc1invoice",
	} {
		if err := validateOrder(o, order(inv), 0); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// watch reads the order once, then on each notice (never acting on the notice
// itself), and stops when the order is finished.
func TestOrderWatch_ReadsOnNotices(t *testing.T) {
	f, conn := setUpOrderHub(t)
	if _, err := placeOrder(t, conn, "100"); err != nil {
		t.Fatal(err)
	}
	o, _ := onlyOrder(t)
	o.AcceptedRelays = []string{"wss://accepted.example"}
	_ = ledger.PutOrder(o)
	prevPoll, prevGap, prevWatch := orderPollEvery, orderNoticeGap, watchNoticesFn
	orderPollEvery, orderNoticeGap = time.Hour, time.Millisecond
	notices := make(chan struct{}, 2)
	var gotNotify string
	var gotAccepted []string
	watchNoticesFn = func(_ context.Context, _, notify string, accepted []string) (<-chan struct{}, func(), error) {
		gotNotify, gotAccepted = notify, accepted
		return notices, func() {}, nil
	}
	t.Cleanup(func() { orderPollEvery, orderNoticeGap, watchNoticesFn = prevPoll, prevGap, prevWatch })
	lookups := 0
	f.lookup = func(nipcash.PendingCall) (nipcashclient.LookupOutcome, *nipCO.Order, error) {
		lookups++
		if lookups == 1 {
			notices <- struct{}{}
			return nipcashclient.LookupOutcome{Status: nipcash.LookupDone}, f.open(100_000, time.Now().Unix()+600), nil
		}
		return nipcashclient.LookupOutcome{Status: nipcash.LookupDone}, f.minted(100_000), nil
	}
	c := orderCmd(newCashOrderWatchCmd(), nil)
	var err error
	captureStreams(t, func() { err = watchOrder(c, o.OrderID, 10*time.Second) })
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if lookups != 2 || gotNotify != o.Notify || len(gotAccepted) != 1 || gotAccepted[0] != "wss://accepted.example" {
		t.Errorf("lookups %d, notify match %v, accepted %v", lookups, gotNotify == o.Notify, gotAccepted)
	}
	if o, _ := onlyOrder(t); o.FinishedAt == 0 {
		t.Error("the order did not finish")
	}
}

// O-2: watch keeps reading an on-chain order on a timer while subscribed (its
// notices can be missed), and reads every orderPollEvery when it can't
// subscribe.
func TestOrderWatch_PollsOnchainOrders(t *testing.T) {
	for name, subscribe := range map[string]bool{"subscribed": true, "subscription failed": false} {
		t.Run(name, func(t *testing.T) {
			f, conn := setUpOrderHub(t)
			if _, err := placeOrder(t, conn, "100"); err != nil {
				t.Fatal(err)
			}
			o, _ := onlyOrder(t)
			o.Payment = nipCO.PaymentOnchain
			_ = ledger.PutOrder(o)
			prevPoll, prevChain, prevWatch := orderPollEvery, orderOnchainPollEvery, watchNoticesFn
			t.Cleanup(func() { orderPollEvery, orderOnchainPollEvery, watchNoticesFn = prevPoll, prevChain, prevWatch })
			if subscribe {
				orderPollEvery, orderOnchainPollEvery = time.Hour, 20*time.Millisecond
				watchNoticesFn = func(context.Context, string, string, []string) (<-chan struct{}, func(), error) {
					return make(chan struct{}), func() {}, nil
				}
			} else {
				orderPollEvery, orderOnchainPollEvery = 20*time.Millisecond, time.Hour
				watchNoticesFn = func(context.Context, string, string, []string) (<-chan struct{}, func(), error) {
					return nil, nil, errors.New("no relay")
				}
			}
			lookups := 0
			f.lookup = func(nipcash.PendingCall) (nipcashclient.LookupOutcome, *nipCO.Order, error) {
				lookups++
				return nipcashclient.LookupOutcome{}, nil, errors.New("unanswered")
			}
			c := orderCmd(newCashOrderWatchCmd(), nil)
			captureStreams(t, func() { _ = watchOrder(c, o.OrderID, 300*time.Millisecond) })
			if lookups < 3 {
				t.Errorf("read %d times in 300 ms, want a timer", lookups)
			}
		})
	}
}

func placeOnchainOrder(t *testing.T, f *fakeOrderHub, conn string) ledger.Order {
	t.Helper()
	f.terms.Onchain = &nipCO.KindTerms{MinMillis: 1000, MaxMillis: 1_000_000_000, FeeBase: 1000, Window: 3600,
		Confirmations: []nipCO.Tier{{N: 1}}}
	c := orderCmd(newCashOrderPlaceCmd(), map[string]string{"onchain": "true"})
	var err error
	captureStreams(t, func() { err = runCashOrderPlace(c, []string{conn, "100"}) })
	if err != nil {
		t.Fatalf("place on-chain: %v", err)
	}
	o, _ := onlyOrder(t)
	return o
}

func showAllOrders(t *testing.T) (string, string, error) {
	t.Helper()
	c := orderCmd(newCashOrderShowCmd(), nil)
	var err error
	stdout, stderr := captureStreams(t, func() { err = showOrders(c, nil, false) })
	return stdout, stderr, err
}

// O-1: a closed on-chain order keeps its call (and the key that collects its
// bills); a payment minted after it closed is collected by show and settle;
// once the Hub no longer knows the order, the call is closed.
func TestOrder_ClosedOnchainCollectsLaterBills(t *testing.T) {
	f, conn := setUpOrderHub(t)
	placeOnchainOrder(t, f, conn)
	bills := []nipCO.OrderBill{f.billFor(100_000)}
	f.lookup = func(nipcash.PendingCall) (nipcashclient.LookupOutcome, *nipCO.Order, error) {
		o := f.onchain(100_000, nipCO.OrderClosed, 0)
		o.Bills = bills
		return nipcashclient.LookupOutcome{Status: nipcash.LookupDone}, o, nil
	}
	if err := settleOrders(t, nil); err != nil {
		t.Fatalf("settle: %v", err)
	}
	o, c := onlyOrder(t)
	if o.State != string(nipCO.OrderClosed) || o.FinishedAt == 0 || c.State != ledger.CallOpenOrder || len(c.Secrets) == 0 {
		t.Fatalf("after closed: order %s/%d, call %s, secrets %d", o.State, o.FinishedAt, c.State, len(c.Secrets))
	}
	if l, _ := ledger.Load(); len(l.Held()) != 1 {
		t.Fatalf("held %d, want 1", len(l.Held()))
	}
	// A later payment to the closed order's address is minted.
	bills = append(bills, f.billFor(50_000))
	if _, _, err := showAllOrders(t); err != nil {
		t.Fatalf("show: %v", err)
	}
	if l, _ := ledger.Load(); len(l.Held()) != 2 {
		t.Fatalf("held %d after a later bill, want 2", len(l.Held()))
	}
	if err := settleOrders(t, nil); err != nil {
		t.Fatalf("settle again: %v", err)
	}
	if l, _ := ledger.Load(); len(l.Held()) != 2 {
		t.Error("a bill was recorded twice")
	}
	// The Hub forgot the order: nothing more can come; the call closes.
	f.lookup = func(nipcash.PendingCall) (nipcashclient.LookupOutcome, *nipCO.Order, error) {
		return nipcashclient.LookupOutcome{Status: nipcash.LookupUnknown}, nil, nil
	}
	_ = settleOrders(t, nil)
	if o, c := onlyOrder(t); c.State != ledger.CallDone || o.State != string(nipCO.OrderClosed) {
		t.Errorf("after unknown: order %s, call %s", o.State, c.State)
	}
}

// O-4: an on-chain address that arrives through a lookup is checked against
// the Hub's chain and network: one for another chain is never shown to pay.
func TestOrder_LookupAddressChecked(t *testing.T) {
	f, conn := setUpOrderHub(t)
	placeOnchainOrder(t, f, conn)
	f.lookup = func(nipcash.PendingCall) (nipcashclient.LookupOutcome, *nipCO.Order, error) {
		o := f.onchain(100_000, nipCO.OrderOpen, 0)
		o.Address = "bc1qqypqxpq9qcrsszg2pvxq6rs0zqg3yyc5fcj4z3"
		return nipcashclient.LookupOutcome{Status: nipcash.LookupDone}, o, nil
	}
	_ = settleOrders(t, nil)
	o, c := onlyOrder(t)
	if c.State != ledger.CallStuck || c.Detail != "invalid_result" || strings.Contains(o.OrderJSON, "bc1q") {
		t.Errorf("wrong-chain address: call %s/%s, order %s", c.State, c.Detail, o.OrderJSON)
	}
}

// O-3: an order recovered after a lost reply shows what to pay, by when, and
// its warnings, in text and --json.
func TestOrder_RecoveredShowsWhatToPay(t *testing.T) {
	f, conn := setUpOrderHub(t)
	f.answer = func(pc nipcash.PendingCall) error {
		return &nipcashclient.IndeterminateError{Call: pc, Cause: errors.New("no reply")}
	}
	if _, err := placeOrder(t, conn, "100"); output.ExitCode(err) != 8 {
		t.Fatalf("lost reply: %v", err)
	}
	f.lookup = func(nipcash.PendingCall) (nipcashclient.LookupOutcome, *nipCO.Order, error) {
		return nipcashclient.LookupOutcome{Status: nipcash.LookupDone}, f.open(100_000, time.Now().Unix()+600), nil
	}
	stdout, _, err := showAllOrders(t)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	var doc struct {
		Orders []map[string]any `json:"orders"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil || len(doc.Orders) != 1 {
		t.Fatalf("show --json %q: %v", stdout, err)
	}
	v := doc.Orders[0]
	inv, _ := v["invoice"].(string)
	if !strings.HasPrefix(inv, "lnfc1") || v["pay_by"] == nil || len(v["warnings"].([]any)) == 0 {
		t.Errorf("recovered order --json %v", v)
	}
	c := newCashOrderShowCmd()
	c.Flags().Bool("json", false, "")
	c.Flags().Bool("yes", false, "")
	c.Flags().String("connection", "", "")
	text, errText := captureStreams(t, func() { err = showOrders(c, nil, true) })
	if err != nil || !strings.Contains(text, inv) || !strings.Contains(text, " by ") || !strings.Contains(errText, "Warning:") {
		t.Errorf("recovered order text %q / %q (%v)", text, errText, err)
	}
}

// O-8, O-7: --replace is refused from another copy of the wallet; a
// re-placement that fails before anything is sent ends the order as
// not_placed instead of leaving it stuck.
func TestOrder_ReplaceDeviceAndNotSent(t *testing.T) {
	f, conn := setUpOrderHub(t)
	f.answer = func(pc nipcash.PendingCall) error {
		return &nipcashclient.IndeterminateError{Call: pc, Cause: errors.New("no reply")}
	}
	_, _ = placeOrder(t, conn, "100")
	o, c := onlyOrder(t)
	l, _ := ledger.Load()
	call, _ := l.FindCall(c.CallID)
	call.State, call.Detail = ledger.CallStuck, "confirm_replace"
	call.DeviceID = "other-device"
	_ = l.Save()
	if err := settleOrders(t, map[string]string{"replace": o.OrderID, "yes": "true"}); output.ExitCode(err) != 5 || len(f.placed) != 1 {
		t.Fatalf("--replace from another device: %v, placed %d", err, len(f.placed))
	}
	l, _ = ledger.Load()
	call, _ = l.FindCall(c.CallID)
	call.DeviceID, _ = deviceIDFn()
	_ = l.Save()
	f.answer, f.refuse = nil, errors.New("relay down")
	if err := settleOrders(t, map[string]string{"replace": o.OrderID, "yes": "true"}); err == nil {
		t.Fatal("a failed re-placement reported success")
	}
	o, c = onlyOrder(t)
	if o.State != ledger.OrderNotPlaced || o.FinishedAt == 0 || c.State != ledger.CallNotApplied {
		t.Errorf("after a failed re-placement: order %s, call %s/%s", o.State, c.State, c.Detail)
	}
}

// R2-C2: an order whose call closes without placing, after its write-ahead,
// ends not_placed in the same save: never sent, or refused BAD_REQUEST.
func TestOrderPlace_ClosedWithoutPlacingEndsTheRow(t *testing.T) {
	for name, answer := range map[string]error{
		"not sent":    nipcashclient.ErrNotSent,
		"bad request": &nipcashclient.FinalError{Method: nipcash.MethodCashOrder, Code: "BAD_REQUEST"},
	} {
		t.Run(name, func(t *testing.T) {
			f, conn := setUpOrderHub(t)
			f.answer = func(nipcash.PendingCall) error { return answer }
			if _, err := placeOrder(t, conn, "100"); err == nil {
				t.Fatal("placed")
			}
			o, c := onlyOrder(t)
			if c.State != ledger.CallNotApplied || o.State != ledger.OrderNotPlaced || o.FinishedAt == 0 {
				t.Errorf("call %s/%s, order %s", c.State, c.Detail, o.State)
			}
		})
	}
}

// R2-C2: a --replace whose child was written ahead and then never sent ends
// the order not_placed; it is not left placing behind a closed call.
func TestOrder_ReplaceWrittenAheadThenNotSentEndsTheRow(t *testing.T) {
	f, conn := setUpOrderHub(t)
	f.answer = func(pc nipcash.PendingCall) error {
		return &nipcashclient.IndeterminateError{Call: pc, Cause: errors.New("no reply")}
	}
	_, _ = placeOrder(t, conn, "100")
	o, c := onlyOrder(t)
	l, _ := ledger.Load()
	call, _ := l.FindCall(c.CallID)
	call.State, call.Detail = ledger.CallStuck, "confirm_replace"
	_ = l.Save()
	f.answer = func(nipcash.PendingCall) error { return nipcashclient.ErrNotSent }
	if err := settleOrders(t, map[string]string{"replace": o.OrderID, "yes": "true"}); err == nil {
		t.Fatal("a re-placement never sent reported success")
	}
	o, c = onlyOrder(t)
	if o.State != ledger.OrderNotPlaced || o.FinishedAt == 0 || c.State != ledger.CallNotApplied {
		t.Errorf("order %s, call %s/%s", o.State, c.State, c.Detail)
	}
}

// R2-C2: a placing row left behind a call already closed not applied (a crash
// between the two writes) is ended by the next settle.
func TestOrderSettle_EndsAPlacingRowBehindAClosedCall(t *testing.T) {
	f, conn := setUpOrderHub(t)
	f.answer = func(pc nipcash.PendingCall) error {
		return &nipcashclient.IndeterminateError{Call: pc, Cause: errors.New("no reply")}
	}
	_, _ = placeOrder(t, conn, "100")
	o, c := onlyOrder(t)
	l, _ := ledger.Load()
	call, _ := l.FindCall(c.CallID)
	call.State, call.Detail = ledger.CallNotApplied, detailNotSent
	_ = l.Save()
	o.State, o.FinishedAt = ledger.OrderPlacing, 0
	_ = ledger.PutOrder(o)
	_ = settleOrders(t, nil)
	if o, _ := onlyOrder(t); o.State != ledger.OrderNotPlaced || o.FinishedAt == 0 {
		t.Errorf("order %s", o.State)
	}
}

// R2-C8: a closed on-chain order past pay_by that the Hub no longer answers
// closes its call and writes its row in one save: when the row can't be
// written, the call stays open and is looked up again.
func TestOrder_ClosedOnchainForgottenWritesCallAndRowTogether(t *testing.T) {
	f, conn := setUpOrderHub(t)
	placeOnchainOrder(t, f, conn)
	f.lookup = func(nipcash.PendingCall) (nipcashclient.LookupOutcome, *nipCO.Order, error) {
		return nipcashclient.LookupOutcome{Status: nipcash.LookupDone}, f.onchain(100_000, nipCO.OrderClosed, 0), nil
	}
	_ = settleOrders(t, nil)
	o, _ := onlyOrder(t)
	o.PayBy, o.SawProgress = time.Now().Unix()-10, true
	_ = ledger.PutOrder(o)
	f.lookup = func(nipcash.PendingCall) (nipcashclient.LookupOutcome, *nipCO.Order, error) {
		return nipcashclient.LookupOutcome{Status: nipcash.LookupUnknown}, nil, nil
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []string{"INSERT", "UPDATE"} {
		if _, err := db.Exec(`CREATE TRIGGER fail_orders_` + ev + ` BEFORE ` + ev + ` ON orders BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
			t.Fatal(err)
		}
	}
	_ = settleOrders(t, nil)
	if _, c := onlyOrder(t); c.State != ledger.CallOpenOrder {
		t.Fatalf("call %s with its row unwritten: want it still open", c.State)
	}
	for _, ev := range []string{"INSERT", "UPDATE"} {
		if _, err := db.Exec(`DROP TRIGGER fail_orders_` + ev); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	_ = settleOrders(t, nil)
	if o2, c := onlyOrder(t); c.State != ledger.CallDone || o2.CheckedAt == 0 {
		t.Errorf("call %s, order checked %d", c.State, o2.CheckedAt)
	}
}
