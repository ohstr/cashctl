package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ohstr/nmilat/nipCO"
	orderclient "github.com/ohstr/nmilat/nipCO/client"
	"github.com/ohstr/nmilat/nipIC"
	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// orderPending is an order call as nmilat reads it: its buyer key (or cash
// secret), notify and the pay_by of a held reply.
func orderPending(c *ledger.Call, o ledger.Order) nipcash.PendingCall {
	pc := pendingOf(c)
	pc.Notify, pc.PayBy = o.Notify, o.PayBy
	return pc
}

// settleOrderCall reads one order and records what the Hub says: an order as it
// stands is checked and applied (bills kept or handed out), finished or ended
// unpaid closes it; a lost placement the Hub never saw waits for --replace.
func settleOrderCall(cmd *cobra.Command, l *ledger.Ledger, c *ledger.Call, device string) string {
	o, ok, err := ledger.OrderForCall(c.CallID)
	if err != nil || !ok {
		return "no order record"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	hub, err := dialOrderFn(ctx, c.TargetCredential)
	if err != nil {
		c.Detail, c.UpdatedAt = "lookup failed: "+output.Sanitize(err.Error()), time.Now().Unix()
		_ = l.Save()
		return ""
	}
	defer hub.Close()
	var got *nipCO.Order
	now := time.Now()
	act, err := nipcashclient.Recover(ctx, orderPending(c, o), func(ctx context.Context, pc nipcash.PendingCall) (nipcashclient.LookupOutcome, error) {
		lo, order, err := hub.Lookup(ctx, pc)
		got = order
		return lo, err
	}, device, now)
	c, _ = l.FindCall(c.CallID)
	if err != nil {
		c.Detail, c.UpdatedAt = "lookup failed: "+output.Sanitize(err.Error()), now.Unix()
		_ = l.Save()
		return ""
	}
	switch {
	case act.Kind == nipcashclient.ActionWait && got != nil:
		finished, rErr := recordOrder(cmd, l, c, &o, got, now.Unix())
		if rErr != nil {
			return "the order doesn't check out; kept"
		}
		if finished {
			return "order " + o.State
		}
		return "order " + string(got.State)
	case act.Kind == nipcashclient.ActionWait && (act.Reason == string(nipcash.LookupUnknown) || act.Reason == string(nipcash.LookupExpired)):
		// Past pay_by with the reply held: never seen paid, it ended unpaid.
		if !o.SawProgress {
			o.State, o.FinishedAt, o.CheckedAt = ledger.OrderEndedUnpaid, now.Unix(), now.Unix()
			closeCall(l, c, ledger.CallDone, ledger.OrderEndedUnpaid)
			_ = l.Save()
			_ = ledger.PutOrder(o)
			return "ended unpaid: nothing was minted"
		}
		o.CheckedAt = now.Unix()
		_ = ledger.PutOrder(o)
		return "no longer answered by the Hub"
	case act.Kind == nipcashclient.ActionShow && act.Reason == "lookup_unknown":
		c.Detail, c.UpdatedAt = "lookup_unknown", now.Unix()
		_ = l.Save()
		return "the Hub doesn't know this order: don't pay it"
	}
	return applyAction(cmd, l, c, act, device)
}

func newCashOrderShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show [<order>...]",
		Short: "Read orders from their Hub and show them",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rejectConnectionFlag(cmd); err != nil {
				return err
			}
			local, _ := cmd.Flags().GetBool("local")
			return showOrders(cmd, args, local)
		},
	}
	cmd.Flags().Bool("local", false, "print what is stored, with no network")
	return cmd
}

// showOrders reads the named orders (or every one not finished, and every
// closed on-chain one) and prints them.
func showOrders(cmd *cobra.Command, ids []string, local bool) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	all, err := ledger.Orders()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	var pick []ledger.Order
	if len(ids) == 0 {
		for _, o := range all {
			if o.FinishedAt == 0 || (o.Payment == nipCO.PaymentOnchain && o.State == string(nipCO.OrderClosed)) {
				pick = append(pick, o)
			}
		}
	} else {
		for _, id := range ids {
			o, ok, err := ledger.FindOrder(id)
			if err != nil {
				return output.RuntimeError(cmd, err)
			}
			if !ok {
				return output.NotFoundError(cmd, id, fmt.Errorf("no order %q", id))
			}
			pick = append(pick, o)
		}
	}
	if !local {
		l, err := ledger.Load()
		if err != nil {
			return output.RuntimeError(cmd, err)
		}
		device, _ := deviceIDFn()
		for _, o := range pick {
			if c, ok := l.FindCall(o.CallID); ok && c.Open() {
				settleOrderCall(cmd, l, c, device)
			}
		}
	}
	var views []map[string]any
	for _, o := range pick {
		fresh, _, _ := ledger.FindOrder(o.OrderID)
		v := orderView(fresh, nil)
		v["next_step"] = orderNextStep(fresh)
		views = append(views, v)
	}
	if jsonMode {
		output.PrintJSON(map[string]any{"orders": output.NonNil(views)})
		return nil
	}
	if len(views) == 0 {
		output.Println("No orders to show.")
	}
	for _, v := range views {
		line := fmt.Sprintf("order %v: %v, %v, pay %v", v["order"], v["state"], v["payment"], v["pay_millis"])
		if v["hub_verified"] == false {
			line += " (Hub not confirmed)"
		}
		if v["terms_changed"] == true {
			line += " (terms changed)"
		}
		output.Println(line)
		if bills, _ := v["bills"].([]map[string]any); len(bills) > 0 {
			output.Printf("  bills: %d\n", len(bills))
		}
	}
	return nil
}

func newCashOrderWatchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "watch <order>",
		Short: "Read an order until it is finished",
		Args:  output.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rejectConnectionFlag(cmd); err != nil {
				return err
			}
			timeout, _ := cmd.Flags().GetDuration("timeout")
			return watchOrder(cmd, args[0], timeout)
		},
	}
	cmd.Flags().Duration("timeout", 0, "stop after this long (default: Lightning pay_by + 60s, on-chain 30m)")
	return cmd
}

// orderPollEvery is how often watch reads a Lightning order until pay_by + 60 s.
var orderPollEvery = 5 * time.Second

// orderNoticeGap is the least time between two reads a notice triggers.
var orderNoticeGap = 2 * time.Second

// watchNoticesFn subscribes to an order's notices (NIP-CO §Order Notice) on
// the relays that accepted it first, then the connection's and the
// announcement's. Each value on the channel is one verified notice; it says
// only that something happened. Swapped in tests.
var watchNoticesFn = func(ctx context.Context, cashorder, notify string, accepted []string) (<-chan struct{}, func(), error) {
	hub, err := orderclient.Connect(ctx, cashorder, orderclient.Options{Session: sessionOptions()})
	if err != nil {
		return nil, func() {}, err
	}
	ch, err := hub.Watch(ctx, notify, accepted)
	if err != nil {
		hub.Close()
		return nil, func() {}, err
	}
	return ch, hub.Close, nil
}

// watchOrder reads an order once, then again on each of its notices (one read
// at a time, at most one per orderNoticeGap) and, for a Lightning order, every
// orderPollEvery until pay_by + 60 s, since notices can be missed. It stops
// when the order is finished or ended unpaid, or at the timeout. Each answer
// is saved before the next; a notice never changes anything by itself.
func watchOrder(cmd *cobra.Command, id string, timeout time.Duration) error {
	o, ok, err := ledger.FindOrder(id)
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	if !ok {
		return output.NotFoundError(cmd, id, fmt.Errorf("no order %q", id))
	}
	if timeout == 0 {
		timeout = 30 * time.Minute
		if o.Payment == nipCO.PaymentLightning && o.PayBy > 0 {
			timeout = time.Until(time.Unix(o.PayBy+60, 0))
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	device, _ := deviceIDFn()
	read := func() {
		l, err := ledger.Load()
		if err != nil {
			return
		}
		if c, ok := l.FindCall(o.CallID); ok && c.Open() {
			settleOrderCall(cmd, l, c, device)
		}
		o, _, _ = ledger.FindOrder(id)
	}
	read()
	var notices <-chan struct{}
	if o.FinishedAt == 0 {
		if c, ok := mustCall(o.CallID); ok {
			ch, closeFn, err := watchNoticesFn(ctx, c.TargetCredential, o.Notify, o.AcceptedRelays)
			if err == nil {
				notices = ch
				defer closeFn()
			} else {
				jsonMode, _ := cmd.Flags().GetBool("json")
				output.Notef(jsonMode, "Couldn't subscribe to the order's notices (%v); reading it on a timer instead.", output.Sanitize(err.Error()))
			}
		}
	}
	var poll <-chan time.Time
	if o.Payment == nipCO.PaymentLightning {
		t := time.NewTicker(orderPollEvery)
		defer t.Stop()
		poll = t.C
	}
	last := time.Now()
	for o.FinishedAt == 0 {
		if o.Payment == nipCO.PaymentLightning && o.PayBy > 0 && time.Now().Unix() > o.PayBy+60 {
			poll = nil
		}
		select {
		case <-ctx.Done():
			goto done
		case _, open := <-notices:
			if !open {
				notices = nil
				continue
			}
			if wait := orderNoticeGap - time.Since(last); wait > 0 {
				select {
				case <-ctx.Done():
					goto done
				case <-time.After(wait):
				}
			}
		case <-poll:
		}
		last = time.Now()
		read()
	}
done:
	if c, ok := mustCall(o.CallID); ok && c.Open() && c.State != ledger.CallOpenOrder {
		return output.IndeterminateError(cmd, id, errors.New("the order's placement has no answer yet; nothing was resent — run `cashctl cash order settle`"))
	}
	return printOrderState(cmd, o)
}

func mustCall(id string) (*ledger.Call, bool) {
	l, err := ledger.Load()
	if err != nil {
		return nil, false
	}
	return l.FindCall(id)
}

func printOrderState(cmd *cobra.Command, o ledger.Order) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	v := orderView(o, nil)
	v["next_step"] = orderNextStep(o)
	if jsonMode {
		output.PrintJSON(v)
		return nil
	}
	output.Printf("order %s: %s\n", o.OrderID, o.State)
	output.Printf("Next: %s\n", orderNextStep(o))
	return nil
}

func newCashOrderSettleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "settle [<order>]",
		Short: "Finish orders whose placement got no answer",
		Args:  output.MaximumNArgs(1),
		RunE:  runCashOrderSettle,
	}
	cmd.Flags().String("replace", "", "place again an order the Hub says was never placed (it asks for a new payment)")
	return cmd
}

func runCashOrderSettle(cmd *cobra.Command, args []string) error {
	if err := rejectConnectionFlag(cmd); err != nil {
		return err
	}
	l, err := ledger.Load()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	device, _ := deviceIDFn()
	if id, _ := cmd.Flags().GetString("replace"); id != "" {
		return replaceOrderByUser(cmd, l, id, device)
	}
	orders, err := ledger.Orders()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	stillOpen := false
	for _, o := range orders {
		if len(args) == 1 && o.OrderID != args[0] {
			continue
		}
		c, ok := l.FindCall(o.CallID)
		if !ok || !c.Open() {
			continue
		}
		settleOrderCall(cmd, l, c, device)
		if c, ok := l.FindCall(o.CallID); ok && c.Open() && c.State != ledger.CallOpenOrder && c.DeviceID == device {
			stillOpen = true
		}
	}
	if err := showOrders(cmd, args, true); err != nil {
		return err
	}
	if stillOpen {
		return output.IndeterminateError(cmd, "", errors.New("an order's placement still has no answer; nothing was resent"))
	}
	return nil
}

// replaceOrderByUser places again an order the Hub says it never saw, under the
// derived id, after the user agreed: same recipient, amount, payment and keys,
// a fresh notify.
func replaceOrderByUser(cmd *cobra.Command, l *ledger.Ledger, id, device string) error {
	o, ok, err := ledger.FindOrder(id)
	if err != nil || !ok {
		return output.NotFoundError(cmd, id, fmt.Errorf("no order %q", id))
	}
	c, ok := l.FindCall(o.CallID)
	if !ok || c.State != ledger.CallStuck || c.Detail != "confirm_replace" {
		return output.ConflictError(cmd, id, errors.New("only an order the Hub says was never placed can be placed again — run `cashctl cash order settle` first"))
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	yes, _ := cmd.Flags().GetBool("yes")
	if jsonMode && !yes {
		return output.UsageError(cmd, errors.New("placing an order again asks for a new payment: pass --yes"))
	}
	if !Confirm(cmd, false, "The Hub says this order was never placed. Place it again? It will ask for a new payment") {
		output.Println("Cancelled.")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	hub, err := dialOrderFn(ctx, c.TargetCredential)
	if err != nil {
		return output.NetworkError(cmd, err)
	}
	defer hub.Close()
	notify, err := nipCO.NewNotify()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	o.Notify = notify
	j := &orderJournal{ledgerJournal: ledgerJournal{l: l, spec: specOf(c)}, order: o}
	var order *nipCO.Order
	parentID := c.CallID
	act, err := nipcashclient.Replace(callContext(ctx), orderPending(c, o), j, nipcashclient.ReplaceOptions{Confirmed: true,
		DerivedEntryExists: func(_ context.Context, rid string) (bool, error) {
			_, ok := l.FindCall(ledger.CallIDFor(rid))
			return ok, nil
		}}, func(ctx context.Context, next nipcash.PendingCall) error {
		recipient, rErr := orderRecipientOf(c)
		if rErr != nil {
			return rErr
		}
		var pErr error
		order, pErr = hub.Place(ctx, nipCO.CashOrderParams{Recipient: recipient, AmountMillis: o.AmountMillis, Payment: o.Payment,
			Notify: notify, RequestID: next.RequestID}, j, device)
		return pErr
	})
	parent, _ := l.FindCall(parentID)
	child := j.call()
	switch {
	case err == nil && act.Kind == nipcashclient.ActionClose:
		parent.State, parent.Detail = ledger.CallReplaced, act.Reason
		_ = l.Save()
		return printOrderState(cmd, o)
	case child == nil || child.CallID == parentID:
		return output.RuntimeError(cmd, fmt.Errorf("not placed again: %v", err))
	}
	parent.State, parent.Detail = ledger.CallReplaced, "replaced by call "+child.CallID
	_ = l.Save()
	if err != nil {
		return placeErr(cmd, l, &j.ledgerJournal, err, "order")
	}
	fresh, _, _ := ledger.FindOrder(o.OrderID)
	if _, err := recordOrder(cmd, l, child, &fresh, order, time.Now().Unix()); err != nil {
		return err
	}
	return printOrder(cmd, fresh, order, nil, fresh.Network != "mainnet")
}

// orderRecipientOf rebuilds an order's recipient from its call: the cash
// secret it kept, or the identity it named.
func orderRecipientOf(c *ledger.Call) (nipcash.Recipient, error) {
	var req struct {
		Recipient nipcash.RecipientParam `json:"recipient"`
	}
	if err := json.Unmarshal([]byte(c.ParamsJSON), &req); err != nil {
		return nil, fmt.Errorf("call %s: unreadable request: %w", c.CallID, err)
	}
	switch req.Recipient.IdentityType {
	case "cash":
		return nipcash.CashTargetFromSecret(callSecretOf(c, ledger.RoleNewIdentity))
	case "pubkey":
		return nipcash.Pubkey(req.Recipient.IdentityValue), nil
	case "connection_key":
		return nipcash.ResolvedConnectionKey(nipIC.ConnectionKey(req.Recipient.IdentityValue), "", req.Recipient.IAPubkey)
	}
	return nil, fmt.Errorf("call %s: unknown recipient %q", c.CallID, req.Recipient.IdentityType)
}
