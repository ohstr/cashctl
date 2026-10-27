package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ohstr/nmilat/nipCO"
	orderclient "github.com/ohstr/nmilat/nipCO/client"
	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/crashpoint"
	"github.com/ohstr/cashctl/internal/credential"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// Cash Orders (NIP-CO): buy cash over an Order connection (cashorder1). The
// order is placed on the private transport, written ahead with its buyer key or
// cash secret and its notify; the reply is the invoice or address to pay; the
// order is then read (cash_lookup) until it is finished, and each bill it mints
// is kept (a cash note or a bill for the local key) or handed out.

// Reasons of a refused order (--json "reason").
const reasonNotSelling = "not_selling"

// purposeOrder is a cash_order call's purpose.
const purposeOrder = "order"

// orderHub is an Order connection: nipCO/client's OrderClient, behind an
// interface so tests can stand in for it.
type orderHub interface {
	Terms(ctx context.Context) (*nipCO.Terms, error)
	Place(ctx context.Context, p nipCO.CashOrderParams, j nipcashclient.Journal, device string) (*nipCO.Order, error)
	Lookup(ctx context.Context, pc nipcash.PendingCall) (nipcashclient.LookupOutcome, *nipCO.Order, error)
	Network() string
	Close()
}

// dialOrderFn connects to a cashorder1 string: get_info and the online check,
// the terms, and the Hub's announcement. Swapped in tests.
var dialOrderFn = func(ctx context.Context, cashorder string) (orderHub, error) {
	return orderclient.Connect(ctx, cashorder, orderclient.Options{Session: sessionOptions()})
}

func newCashOrderCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "order",
		Short: "Buy cash over an Order connection (cashorder1)",
		RunE:  groupRunE,
	}
	cmd.AddCommand(newCashOrderPlaceCmd(), newCashOrderShowCmd(), newCashOrderWatchCmd(), newCashOrderSettleCmd())
	return cmd
}

func newCashOrderPlaceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "place <cashorder1...|-> <amount>",
		Short: "Order a bill: get an invoice (or an on-chain address) to pay",
		Long: `Places an order over an Order connection. The Hub must answer as the
identity and chain the string names; a string whose signature can't be
confirmed is used only after you agree once (or --allow-unverified). The terms
are shown first (fee, amount to pay, payment window, the bill's redeem fee).

Pay exactly the amount shown, before pay_by: a different amount mints a
different bill, never a refund. Then ` + "`cash order watch <order>`" + ` or ` + "`cash order show`" + `
collects the bill.`,
		Example: `  cashctl cash order place cashorder1... 100
  cashctl cash order place - 50000 --onchain --to self < order.txt`,
		Args: output.ExactArgs(2),
		RunE: runCashOrderPlace,
	}
	cmd.Flags().Bool("onchain", false, "pay on-chain instead of over Lightning")
	cmd.Flags().String("to", "cash", "who the bill is for: cash, self, a pubkey/npub/name@domain, or connection:<platform>:<id>:<ia>")
	cmd.Flags().Bool("allow-unverified", false, "use a string whose signature can't be confirmed (asked once per string otherwise)")
	cmd.Flags().Bool("wait", false, "after placing, watch the order until it is finished")
	return cmd
}

// orderRecipient is --to as an order recipient: its kind for the record
// (cash, self, other) and what the record shows.
func orderRecipient(cmd *cobra.Command, to string) (nipcash.Recipient, string, string, error) {
	switch to {
	case "", "cash":
		t, err := nipCO.NewCashOrderTarget()
		if err != nil {
			return nil, "", "", output.RuntimeError(cmd, err)
		}
		return t, "cash", "cash", nil
	case "self":
		pub, err := localPubKeyHex(cmd)
		if err != nil {
			return nil, "", "", output.RuntimeError(cmd, err)
		}
		return nipcash.Pubkey(pub), "self", pub, nil
	}
	rt, err := credential.ParseTarget(to)
	if err != nil {
		return nil, "", "", output.InvalidInputError(cmd, to, err)
	}
	r, ok := rt.Target.(nipcash.Recipient)
	if !ok {
		return nil, "", "", output.InvalidInputError(cmd, to, errors.New("not a recipient a bill can be ordered for"))
	}
	if localPubIs(cmd, strings.TrimPrefix(to, "pubkey:")) {
		return r, "self", strings.TrimPrefix(to, "pubkey:"), nil
	}
	return r, "other", rt.Input, nil
}

func runCashOrderPlace(cmd *cobra.Command, args []string) error {
	if err := rejectConnectionFlag(cmd); err != nil {
		return err
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	raw, err := argOrStdin(cmd, args[0])
	if err != nil {
		return err
	}
	amount, err := output.ParseAmount(args[1])
	if err != nil || amount == 0 {
		if err == nil {
			err = errors.New("an order needs an amount above zero")
		}
		return output.InvalidInputError(cmd, args[1], err)
	}
	c, err := readConnection(cmd, raw, nipCO.OrderConnectionHRP)
	if err != nil {
		return err
	}
	var warnings []string
	if !c.Verified {
		warnings = append(warnings, warnUnverified(jsonMode, c))
	}
	payment := nipCO.PaymentLightning
	if on, _ := cmd.Flags().GetBool("onchain"); on {
		payment = nipCO.PaymentOnchain
	}
	to, _ := cmd.Flags().GetString("to")
	recipient, recipientKind, recipientShown, err := orderRecipient(cmd, to)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	var hub orderHub
	var terms *nipCO.Terms
	err = WithSpinner(jsonMode, "Checking the Hub...", func() error {
		var dErr error
		if hub, dErr = dialOrderFn(ctx, c.Raw); dErr != nil {
			return dErr
		}
		terms, dErr = hub.Terms(ctx)
		return dErr
	})
	if hub != nil {
		defer hub.Close()
	}
	if err != nil {
		switch {
		case isConnectionMismatch(err):
			return connectionMismatchErr(cmd, err)
		case errors.Is(err, nipcashclient.ErrHubUnusable), errors.Is(err, nipCO.ErrOrderTerms):
			return output.RuntimeError(cmd, fmt.Errorf("this Order connection can't be used safely; nothing was sent: %w", err))
		}
		return output.NetworkError(cmd, fmt.Errorf("couldn't reach the Hub; nothing was sent: %w", err))
	}
	if proceed, err := confirmUnverified(cmd, c); err != nil || !proceed {
		if err == nil {
			output.Println("Not sent.")
		}
		return err
	}
	network := hub.Network()
	isTest := testNetwork(jsonMode, network)
	if terms.SellsNothing() {
		return output.WithReason(output.AuthError(cmd, errors.New("this order link is retired: it sells nothing now")), reasonNotSelling)
	}
	k := terms.Kind(payment)
	if k == nil {
		var offered []string
		if terms.Lightning != nil {
			offered = append(offered, "lightning")
		}
		if terms.Onchain != nil {
			offered = append(offered, "on-chain (--onchain)")
		}
		return output.InvalidInputError(cmd, "", fmt.Errorf("this Hub doesn't sell for %s payment; it offers %s", payment, strings.Join(offered, ", ")))
	}
	switch {
	case payment == nipCO.PaymentOnchain && amount%1000 != 0:
		return output.InvalidInputError(cmd, args[1], errors.New("an on-chain order is a whole number of base units"))
	case amount < k.MinMillis:
		return output.InvalidInputError(cmd, args[1], fmt.Errorf("below min_millis %s", output.FormatAmount(int64(k.MinMillis)))) //nolint:gosec // amounts fit
	case k.MaxMillis > 0 && amount > k.MaxMillis:
		return output.InvalidInputError(cmd, args[1], fmt.Errorf("above max_millis %s", output.FormatAmount(int64(k.MaxMillis)))) //nolint:gosec // amounts fit
	}
	fee, pay, err := nipCO.OrderFee(payment, *k, amount)
	if err != nil {
		return output.InvalidInputError(cmd, args[1], err)
	}
	if !jsonMode {
		label, has := c.label()
		hubName := "Hub " + shortFingerprint(c.hubIdentity())
		if has {
			hubName = fmt.Sprintf("%q (not confirmed), %s", output.Sanitize(label), hubName)
		}
		if !c.Verified {
			hubName += " — Hub not confirmed"
		}
		output.Notef(false, "Order from %s:", hubName)
		output.Notef(false, "  amount %s, order fee %s, you pay %s (%s), within %s", output.FormatAmount(int64(amount)), //nolint:gosec // amounts fit
			output.FormatAmount(int64(fee)), output.FormatAmount(int64(pay)), payment, (time.Duration(k.Window) * time.Second).String()) //nolint:gosec // amounts fit
		output.Notef(false, "  redeeming this bill later costs %s + %d ppm", output.FormatAmount(int64(terms.Bill.RedeemFeeBase)), terms.Bill.RedeemFeePpm) //nolint:gosec // amounts fit
		output.Notef(false, "  for: %s", output.Sanitize(recipientShown))
	}
	if !Confirm(cmd, true, "Place this order?") {
		output.Println("Not sent.")
		return nil
	}

	l, err := ledger.Load()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	device, err := deviceIDFn()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	notify, err := nipCO.NewNotify()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	authKind := ledger.AuthBuyerKey
	j := &orderJournal{ledgerJournal: ledgerJournal{l: l, spec: callSpec{Purpose: purposeOrder, AuthorizerKind: authKind,
		TargetCredential: c.Raw, Chain: c.chain()}},
		order: ledger.Order{Notify: notify, Payment: payment, AmountMillis: amount, RecipientKind: recipientKind,
			Recipient: recipientShown, HubIdentity: c.hubIdentity(), Chain: c.chain(), Network: network, ConnVerified: c.Verified}}
	var order *nipCO.Order
	err = WithSpinner(jsonMode, "Placing the order...", func() error {
		var pErr error
		order, pErr = hub.Place(callContext(ctx), nipCO.CashOrderParams{Recipient: recipient, AmountMillis: amount, Payment: payment, Notify: notify}, j, device)
		return pErr
	})
	if err != nil {
		if errors.Is(err, orderclient.ErrOrderNotSold) {
			return output.WithReason(output.AuthError(cmd, errors.New("the Hub stopped selling this kind of order; nothing was sent")), reasonNotSelling)
		}
		return placeErr(cmd, l, &j.ledgerJournal, err, "order")
	}
	crashpoint.Hit("calls.before_apply")
	call := j.call()
	o, _, _ := ledger.OrderForCall(call.CallID)
	if _, err := recordOrder(cmd, l, call, &o, order, time.Now().Unix()); err != nil {
		return err
	}
	if wait, _ := cmd.Flags().GetBool("wait"); wait {
		return watchOrder(cmd, o.OrderID, 0)
	}
	return printOrder(cmd, o, order, warnings, isTest)
}

// orderJournal writes an order's call and its orders row ahead together, and
// keeps the relays that accepted it (nipcash/client.AcceptedRecorder).
type orderJournal struct {
	ledgerJournal
	order ledger.Order
}

// WriteAhead stages the order row so the ledger writes it in the same
// transaction as its call, then reads both back.
func (j *orderJournal) WriteAhead(ctx context.Context, calls []nipcash.PendingCall) error {
	if len(calls) == 0 {
		return errors.New("no order call to write")
	}
	callID := ledger.CallIDFor(calls[len(calls)-1].RequestID)
	o := j.order
	if existing, ok, _ := ledger.OrderForCall(callID); ok {
		o = existing
	} else if o.OrderID == "" {
		o.OrderID = callID
	}
	o.CallID = callID
	if o.State == "" {
		o.State = ledger.OrderPlacing
	}
	j.l.StageOrder(o)
	if err := j.ledgerJournal.WriteAhead(ctx, calls); err != nil {
		return err
	}
	if got, ok, err := ledger.FindOrder(o.OrderID); err != nil || !ok || got.Notify != o.Notify || got.CallID != callID {
		j.l.RemoveCall(callID)
		_ = j.l.Save()
		_ = ledger.DeleteOrder(o.OrderID)
		return errors.New("cashctl.db: the order's write-ahead did not read back as written")
	}
	j.order = o
	return nil
}

// RecordAccepted keeps the relays that accepted the order, for its notices.
func (j *orderJournal) RecordAccepted(_ context.Context, _ string, relays []string) error {
	o, ok, err := ledger.FindOrder(j.order.OrderID)
	if err != nil || !ok {
		return err
	}
	o.AcceptedRelays = relays
	return ledger.PutOrder(o)
}

var _ nipcashclient.AcceptedRecorder = (*orderJournal)(nil)

// recordOrder checks an order as the Hub stated it (a reply or a lookup's done)
// and records it: the order row, every new bill (kept or handed out), and the
// call, closed once the order is finished or ended unpaid. It reports whether
// the order is finished.
func recordOrder(cmd *cobra.Command, l *ledger.Ledger, c *ledger.Call, o *ledger.Order, order *nipCO.Order, now int64) (bool, error) {
	if err := validateOrder(o, order, now); err != nil {
		c.State, c.Detail, c.UpdatedAt = ledger.CallStuck, "invalid_result", now
		_ = l.Save()
		return false, output.WithCall(output.IndeterminateError(cmd, "", fmt.Errorf(
			"the Hub's order doesn't check out (%v); nothing is shown to pay, and the order's keys are kept", err)),
			map[string]any{"call": c.CallID, "method": c.Method, "state": c.State, "detail": c.Detail})
	}
	if err := applyOrderBills(cmd, l, c, o, order); err != nil {
		return false, output.RuntimeError(cmd, err)
	}
	raw, _ := json.Marshal(order)
	o.OrderJSON, o.State, o.PayBy, o.CheckedAt = string(raw), string(order.State), order.PayBy, now
	o.TermsChanged = o.TermsChanged || order.TermsChanged
	if order.State == nipCO.OrderPaid || order.State == nipCO.OrderMinted || order.State == nipCO.OrderClosed || len(order.Bills) > 0 {
		o.SawProgress = true
	}
	finished := false
	switch {
	case orderclient.Finished(order):
		finished = true
	case orderclient.EndedUnpaid(order, o.SawProgress, now):
		o.State, finished = ledger.OrderEndedUnpaid, true
	}
	if finished {
		o.FinishedAt = now
		closeCall(l, c, ledger.CallDone, o.State)
	} else {
		c.State, c.Detail, c.UpdatedAt = ledger.CallOpenOrder, string(order.State), now
	}
	if err := l.Save(); err != nil {
		return false, output.RuntimeError(cmd, err)
	}
	if err := ledger.PutOrder(*o); err != nil {
		return false, output.RuntimeError(cmd, err)
	}
	return finished, nil
}

// validateOrder is what cashctl checks of an order beyond nmilat's parse: the
// kind and amount as sent, a payment to show, and pay_by within reach.
func validateOrder(o *ledger.Order, order *nipCO.Order, now int64) error {
	switch {
	case order == nil:
		return errors.New("no order")
	case order.Payment != o.Payment || order.AmountMillis != o.AmountMillis:
		return errors.New("the order is not the one placed")
	case order.Payment == nipCO.PaymentLightning && order.Invoice == "":
		return errors.New("no invoice")
	case order.Payment == nipCO.PaymentOnchain && (order.Address == "" || order.Confirmations == 0):
		return errors.New("no address or confirmations")
	case order.PayMillis != order.AmountMillis+order.FeeMillis:
		return errors.New("pay_millis is not amount plus fee")
	}
	return nil
}

// applyOrderBills records each bill of order not yet held here: a cash note
// (the order's cash secret) or a bill for the local key is kept; one for
// someone else is handed out. A tombstone in place of a token is skipped.
func applyOrderBills(cmd *cobra.Command, l *ledger.Ledger, c *ledger.Call, o *ledger.Order, order *nipCO.Order) error {
	for _, b := range order.Bills {
		if b.Token.Tombstone != nil || b.Token.Token == "" {
			continue
		}
		token := strings.ToLower(b.Token.Token)
		if _, ok := l.FindByToken(token); ok || handedOut(l, token) {
			continue
		}
		tok, err := nipcash.Decode(token)
		if err != nil {
			return err
		}
		minter, ok := nipcash.VerifyProvenance(tok)
		if !ok || !nipcash.HubIdentityEqual(o.HubIdentity, minter) {
			return fmt.Errorf("bill %s is not signed by the Hub", b.WalletPubkey)
		}
		amount := b.AmountMillis
		switch o.RecipientKind {
		case "cash":
			secret := callSecretOf(c, ledger.RoleNewIdentity)
			if _, err := l.Add(ledger.Entry{Token: token, WalletPubkey: b.WalletPubkey, Secret: tok.Secret, RelayURLs: tok.RelayURLs,
				AmountMillis: &amount, Verified: true, MinterPubkey: &minter, IdentityRequired: ptrTo(false),
				CashSecret: secret, CashProtection: ledger.CashProtected}); err != nil {
				return err
			}
		case "self":
			if _, err := l.Add(ledger.Entry{Token: token, WalletPubkey: b.WalletPubkey, Secret: tok.Secret, RelayURLs: tok.RelayURLs,
				AmountMillis: &amount, Verified: true, MinterPubkey: &minter, IdentityRequired: ptrTo(true)}); err != nil {
				return err
			}
		default:
			l.AddHandoff(ledger.Handoff{Kind: ledger.HandoffGift, Handoff: token, AmountMillis: amount, Target: "identity", WalletPubkey: b.WalletPubkey})
		}
		l.AppendHistory("order", fmt.Sprintf("order %s minted %s", o.OrderID, output.FormatAmount(int64(amount)))) //nolint:gosec // amounts fit
	}
	return nil
}

func handedOut(l *ledger.Ledger, token string) bool {
	for _, h := range l.Handoffs {
		if strings.EqualFold(h.Handoff, token) {
			return true
		}
	}
	return false
}

// orderView is an order for --json and the screen.
func orderView(o ledger.Order, order *nipCO.Order) map[string]any {
	v := map[string]any{"order": o.OrderID, "state": o.State, "payment": o.Payment, "amount_millis": o.AmountMillis,
		"recipient": o.Recipient, "network": o.Network, "chain": o.Chain, "test_network": o.Network != "mainnet",
		"hub_verified": o.ConnVerified, "finished": o.FinishedAt != 0}
	if order == nil && o.OrderJSON != "" {
		order = &nipCO.Order{}
		_ = json.Unmarshal([]byte(o.OrderJSON), order)
	}
	bills := []map[string]any{}
	if order != nil {
		v["fee_millis"], v["pay_millis"], v["pay_by"] = order.FeeMillis, order.PayMillis, order.PayBy
		if order.Invoice != "" {
			v["invoice"] = order.Invoice
		}
		if order.Address != "" {
			v["address"], v["confirmations"] = order.Address, order.Confirmations
		}
		for _, b := range order.Bills {
			row := map[string]any{"wallet_pubkey": b.WalletPubkey, "amount_millis": b.AmountMillis, "paid_millis": b.PaidMillis}
			if b.Outpoint != "" {
				row["outpoint"] = b.Outpoint
			}
			if b.Token.Tombstone != nil {
				row["state"] = "gone"
			}
			bills = append(bills, row)
		}
	}
	v["bills"] = bills
	if o.TermsChanged {
		v["terms_changed"] = true
	}
	return v
}

func orderWarnings(o ledger.Order, order *nipCO.Order) []string {
	if order == nil || o.FinishedAt != 0 {
		return nil
	}
	by := time.Unix(order.PayBy, 0).UTC().Format(time.RFC3339)
	w := []string{"pay exactly this amount: a different amount mints a different bill, never a refund",
		"a payment the Hub has not seen by " + by + " is not minted"}
	if order.Payment == nipCO.PaymentOnchain {
		w = append(w, "a first payment after "+by+" is kept by the Hub",
			fmt.Sprintf("each output to this address is a separate payment and pays the order fee of %s once; pay it in one output", output.FormatAmount(int64(order.FeeMillis))), //nolint:gosec // amounts fit
			fmt.Sprintf("an output of %s or less mints nothing", output.FormatAmount(int64(order.FeeMillis))))                                                                     //nolint:gosec // amounts fit
	} else {
		w = append(w, "the invoice expires then")
	}
	if o.TermsChanged {
		w = append(w, "the Hub's terms changed since cashctl read them")
	}
	return w
}

func printOrder(cmd *cobra.Command, o ledger.Order, order *nipCO.Order, warnings []string, isTest bool) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	warnings = append(warnings, orderWarnings(o, order)...)
	if jsonMode {
		v := orderView(o, order)
		v["warnings"], v["test_network"] = output.NonNil(warnings), isTest
		v["next_step"] = orderNextStep(o)
		output.PrintJSON(v)
		return nil
	}
	v := orderView(o, order)
	output.Printf("Order %s: pay %s by %s\n", o.OrderID, output.FormatAmount(int64(order.PayMillis)), //nolint:gosec // amounts fit
		time.Unix(order.PayBy, 0).UTC().Format(time.RFC3339))
	if inv, ok := v["invoice"]; ok {
		output.Printf("%s\n", inv)
		output.Println("Pay it from any Lightning wallet, e.g. `cashctl pay <invoice>`.")
	}
	if addr, ok := v["address"]; ok {
		output.Printf("%s (needs %d confirmation(s))\n", addr, order.Confirmations)
	}
	for _, w := range warnings {
		output.Notef(false, "Warning: %s.", w)
	}
	output.Printf("Next: %s\n", orderNextStep(o))
	return nil
}

func orderNextStep(o ledger.Order) string {
	switch {
	case o.FinishedAt != 0:
		return "nothing: the order is finished"
	case o.Payment == nipCO.PaymentOnchain:
		return "pay, then `cashctl cash order show " + o.OrderID + "` collects the bills"
	}
	return "pay, then `cashctl cash order watch " + o.OrderID + "` collects the bill"
}
