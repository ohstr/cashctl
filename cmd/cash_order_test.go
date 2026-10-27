package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nipCO"
	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
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
}

func (f *fakeOrderHub) Terms(context.Context) (*nipCO.Terms, error) { return &f.terms, nil }
func (f *fakeOrderHub) Network() string                             { return "mainnet" }
func (f *fakeOrderHub) Close()                                      {}

func (f *fakeOrderHub) Place(ctx context.Context, p nipCO.CashOrderParams, j nipcashclient.Journal, device string) (*nipCO.Order, error) {
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
	return f.open(p.AmountMillis, time.Now().Unix()+600), nil
}

func (f *fakeOrderHub) open(amount uint64, payBy int64) *nipCO.Order {
	return &nipCO.Order{State: nipCO.OrderOpen, Payment: nipCO.PaymentLightning, AmountMillis: amount, FeeMillis: 1000,
		PayMillis: amount + 1000, Invoice: "lnfc1invoice", PayBy: payBy}
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
	if doc["invoice"] != "lnfc1invoice" || doc["state"] != "open" || len(doc["warnings"].([]any)) == 0 {
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
