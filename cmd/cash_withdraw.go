package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ohstr/nmilat/nipCO"
	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/ohstr/cashctl/internal/crashpoint"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// On-chain withdrawals (NIP-CASH §Redeeming On-chain). A quote moves nothing; a
// withdrawal is irreversible, so it is sent only after the user typed "yes" to
// the exact quote, or, non-interactively, named a quote it was shown with
// --quote and --yes. The call is written ahead with the quote, address, notify
// and any re-mint secret; the payout is then followed until final or
// cancelled (a cancelled payout comes back as a new bill).

// Reasons of a refused withdrawal (--json "reason").
const (
	reasonWithdrawNotOffered = "withdraw_not_offered"
	reasonAddressInvalid     = "address_invalid"
	reasonOwnOrderAddress    = "own_order_address"
	reasonQuoteMismatch      = "quote_mismatch"
	reasonReceiptInvalid     = "receipt_invalid"
	reasonExpiring           = "expiring"
	reasonCancelNotDue       = "cancel_not_due"
	reasonProtectFirst       = "protect_first"
)

// purposeWithdraw is a cash_withdraw call's purpose.
const purposeWithdraw = "withdraw"

// withdrawBill is a dialled bill's withdrawal methods: nipcash/client.Client,
// behind an interface so tests can stand in for it.
type withdrawBill interface {
	QuoteWithdraw(ctx context.Context, p nipcash.CashWithdrawQuoteParams) (*nipcash.WithdrawQuote, error)
	Withdraw(ctx context.Context, p nipcash.CashWithdrawParams, j nipcashclient.Journal, device string) (*nipcash.Payout, error)
	LookupWithdraw(ctx context.Context, pc nipcash.PendingCall, cred nipcash.Credential) (nipcashclient.LookupOutcome, *nipcash.Payout, error)
	CancelWithdraw(ctx context.Context, pc nipcash.PendingCall, cred nipcash.Credential) (*nipcash.Payout, error)
	Close()
}

// dialWithdrawFn dials a bill for a withdrawal, through the provenance gate.
// Swapped in tests.
var dialWithdrawFn = func(ctx context.Context, token string) (withdrawBill, error) {
	c, err := dialBill(ctx, token)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func newCashWithdrawCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "withdraw",
		Short: "Pay a bill out on-chain",
		RunE:  groupRunE,
	}
	cmd.AddCommand(newCashWithdrawQuoteCmd(), newCashWithdrawSendCmd(), newCashWithdrawShowCmd(),
		newCashWithdrawCancelCmd(), newCashWithdrawSettleCmd())
	return cmd
}

func addWithdrawBillFlags(cmd *cobra.Command) {
	cmd.Flags().String("token", "", "the held bill to withdraw (required when you hold several)")
	cmd.Flags().String("send-before", "", "latest time the payout must be sent: unix seconds, RFC 3339 or +<duration>")
	cmd.Flags().String("as", "", "the credential the bill needs (as redeem --as)")
}

func newCashWithdrawQuoteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "quote <address|->",
		Short: "Ask what withdrawing a bill to an address would pay (moves nothing)",
		Args:  output.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _, _, err := quoteWithdrawal(cmd, args[0], true)
			return err
		},
	}
	addWithdrawBillFlags(cmd)
	return cmd
}

func newCashWithdrawSendCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "send [<address|->]",
		Short: "Withdraw a bill on-chain, after confirming its quote — irreversible",
		Long: `Withdraws one whole bill to an on-chain address. Interactively it quotes first
and sends only after you type "yes" to the exact amounts shown. Without a
terminal (or under --json), quote first (` + "`cash withdraw quote`" + `), show the quote
to the user, then ` + "`cash withdraw send --quote wq-... --yes`" + `.`,
		Args: output.MaximumNArgs(1),
		RunE: runCashWithdrawSend,
	}
	addWithdrawBillFlags(cmd)
	cmd.Flags().String("quote", "", "send exactly this quote (from cash withdraw quote); needs --yes")
	return cmd
}

// parseSendBefore reads --send-before.
func parseSendBefore(cmd *cobra.Command) (*int64, error) {
	v, _ := cmd.Flags().GetString("send-before")
	if v == "" {
		return nil, nil
	}
	if strings.HasPrefix(v, "+") {
		d, err := time.ParseDuration(v[1:])
		if err != nil {
			return nil, output.InvalidInputError(cmd, v, err)
		}
		at := time.Now().Add(d).Unix()
		return &at, nil
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return &n, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, output.InvalidInputError(cmd, v, errors.New("not unix seconds, RFC 3339 or +<duration>"))
	}
	at := t.Unix()
	return &at, nil
}

// checkWithdrawAddress is the local part of the address check (nmilat checks
// the network and script type against the terms when quoting): normalized,
// never one of this wallet's own order deposit addresses.
func checkWithdrawAddress(cmd *cobra.Command, raw string) (string, []string, error) {
	addr, err := nipcash.NormalizeAddress(raw)
	if err != nil {
		return "", nil, output.WithReason(output.InvalidInputError(cmd, raw, fmt.Errorf("not a valid address: %w", err)), reasonAddressInvalid)
	}
	orders, _ := ledger.Orders()
	for _, o := range orders {
		var ord nipCO.Order
		if o.OrderJSON != "" && json.Unmarshal([]byte(o.OrderJSON), &ord) == nil && strings.EqualFold(ord.Address, addr) {
			return "", nil, output.WithReason(output.InvalidInputError(cmd, addr, fmt.Errorf(
				"this is the deposit address of order %s; paying it would not reach you", o.OrderID)), reasonOwnOrderAddress)
		}
	}
	var warnings []string
	ws, _ := ledger.Withdrawals()
	for _, w := range ws {
		switch {
		case w.Address == addr:
			warnings = append(warnings, "you used this address before; reusing addresses links your payments")
		case len(w.Address) > 12 && len(addr) > 12 && w.Address[:6] == addr[:6] && w.Address[len(w.Address)-6:] == addr[len(addr)-6:]:
			warnings = append(warnings, "this address looks like one you used before but is different: check every character")
		}
	}
	return addr, warnings, nil
}

// classifyQuoteErr maps a quote's failure: nothing was sent to withdraw.
func classifyQuoteErr(cmd *cobra.Command, err error) error {
	var final *nipcashclient.FinalError
	var gone *nipcash.BillGone
	switch {
	case errors.Is(err, nipcashclient.ErrWithdrawNotOffered):
		return output.WithReason(output.RuntimeError(cmd, errors.New("this Hub doesn't pay out on-chain; redeem over Lightning (`cashctl redeem`)")), reasonWithdrawNotOffered)
	case errors.Is(err, nipcash.ErrQuoteMismatch):
		return output.WithReason(output.RuntimeError(cmd, fmt.Errorf("the Hub's quote doesn't match what was asked; nothing was sent: %w", err)), reasonQuoteMismatch)
	case errors.Is(err, nipcash.ErrAddressMalformed), errors.Is(err, nipcash.ErrAddressNetwork),
		errors.Is(err, nipcash.ErrAddressNoWitness), errors.Is(err, nipcash.ErrScriptTypeNotPaid):
		return output.WithReason(output.InvalidInputError(cmd, "", addressRefusal(err)), reasonAddressInvalid)
	case errors.As(err, &gone):
		return output.SpentError(cmd, "", errBillSpent)
	case errors.As(err, &final):
		msg := fmt.Errorf("the Hub refused the quote: %s %s", final.Code, output.Sanitize(final.Detail))
		switch final.Code {
		case "RESTRICTED", "UNAUTHORIZED":
			return output.AuthError(cmd, msg)
		case "RATE_LIMITED", "QUOTA_EXCEEDED":
			return output.ConflictError(cmd, "", msg)
		case "NOT_FOUND":
			return output.NotFoundError(cmd, "", msg)
		case "BAD_REQUEST":
			return output.InvalidInputError(cmd, "", msg)
		}
		return output.RuntimeError(cmd, msg)
	}
	return classifyCashTokenNWCErr(cmd, err)
}

func addressRefusal(err error) error {
	switch {
	case errors.Is(err, nipcash.ErrAddressNetwork):
		return errors.New("this address is for another network")
	case errors.Is(err, nipcash.ErrAddressNoWitness):
		return errors.New("this Hub pays only bech32/bech32m (segwit) addresses")
	case errors.Is(err, nipcash.ErrScriptTypeNotPaid):
		return errors.New("this Hub doesn't pay this kind of address")
	}
	return errors.New("not a valid address (checksum, version or length)")
}

// quoteWithdrawal quotes withdrawing the selected bill to raw. print shows the
// quote and how to send it.
func quoteWithdrawal(cmd *cobra.Command, raw string, print bool) (*ledger.Entry, *nipcash.WithdrawQuote, string, error) {
	if err := rejectConnectionFlag(cmd); err != nil {
		return nil, nil, "", err
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	raw, err := argOrStdin(cmd, raw)
	if err != nil {
		return nil, nil, "", err
	}
	l, err := ledger.Load()
	if err != nil {
		return nil, nil, "", output.RuntimeError(cmd, err)
	}
	settleCalls(cmd, l, settleOptions{})
	e, err := resolveHeldToken(cmd, l)
	if err != nil {
		return nil, nil, "", err
	}
	if isCashNote(e) && e.CashProtection == ledger.CashShared {
		return nil, nil, "", output.WithReason(output.UsageError(cmd, errors.New(
			"run `cashctl wallet protect` first: whoever gave you this bill can still spend it")), reasonProtectFirst)
	}
	addr, warnings, err := checkWithdrawAddress(cmd, raw)
	if err != nil {
		return nil, nil, "", err
	}
	sendBefore, err := parseSendBefore(cmd)
	if err != nil {
		return nil, nil, "", err
	}
	cred, err := resolveCredential(cmd, e)
	if err != nil {
		return nil, nil, "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var q *nipcash.WithdrawQuote
	err = WithSpinner(jsonMode, "Asking the Hub for a quote...", func() error {
		c, dErr := dialWithdrawFn(ctx, e.Token)
		if dErr != nil {
			return dErr
		}
		defer c.Close()
		var qErr error
		q, qErr = c.QuoteWithdraw(ctx, nipcash.CashWithdrawQuoteParams{Address: addr, SendBefore: sendBefore, Credential: cred})
		return qErr
	})
	if err != nil {
		return nil, nil, "", classifyQuoteErr(cmd, err)
	}
	if e.ExpiresAt != nil && *e.ExpiresAt > 0 && time.Until(time.Unix(*e.ExpiresAt, 0)) <= 120*time.Second {
		warnings = append(warnings, "this bill expires too soon to send")
	}
	rawQuote, _ := q.Raw()
	handle := "wq-" + q.ID[:10]
	var sb int64
	if sendBefore != nil {
		sb = *sendBefore
	}
	if err := ledger.PutWithdrawQuote(ledger.WithdrawQuote{Handle: handle, EntryID: e.ID, WalletPubkey: e.WalletPubkey,
		HubIdentity: xOnlyMinter(e), Address: addr, QuoteJSON: rawQuote, QuoteID: q.ID, SendBefore: sb}); err != nil {
		return nil, nil, "", output.RuntimeError(cmd, err)
	}
	if print {
		if jsonMode {
			v := quoteView(e, q)
			v["quote"], v["warnings"] = handle, output.NonNil(warnings)
			v["next_step"] = "show these numbers to the user; to send exactly this: `cashctl cash withdraw send --quote " + handle + " --yes`"
			output.PrintJSON(v)
		} else {
			printWithdrawScreen(e, q, warnings)
			output.Printf("To send exactly this: cashctl cash withdraw send --quote %s --yes\n", handle)
		}
	}
	return e, q, handle, nil
}

// xOnlyMinter is a bill's minter as an x-only key, "" when unknown.
func xOnlyMinter(e *ledger.Entry) string {
	if e.MinterPubkey == nil {
		return ""
	}
	x, err := nipcash.MinterXOnly(*e.MinterPubkey)
	if err != nil {
		return ""
	}
	return x
}

func quoteView(e *ledger.Entry, q *nipcash.WithdrawQuote) map[string]any {
	return map[string]any{"token": e.ID, "address": q.Address, "amount_millis": q.AmountMillis,
		"network_fee_millis": q.NetworkFeeMillis, "hub_fee_millis": q.HubFeeMillis, "fee_millis": q.FeeMillis(),
		"payout_millis": q.PayoutMillis, "sent_by": q.SentBy, "holds_until": q.HoldsUntil}
}

// groupsOf4 is an address in groups of four, for comparing by eye.
func groupsOf4(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i += 4 {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(s[i:min(i+4, len(s))])
	}
	return b.String()
}

// printWithdrawScreen is the confirmation screen, on stderr, every number from
// the verified quote; the address in groups and whole.
func printWithdrawScreen(e *ledger.Entry, q *nipcash.WithdrawQuote, warnings []string) {
	f := func(v uint64) string { return output.FormatAmount(int64(v)) } //nolint:gosec // amounts fit
	output.Fprintf(os.Stderr, "Withdraw on-chain — this cannot be reversed\n\n")
	output.Fprintf(os.Stderr, "  From bill        %s  (%s)\n", e.ID, f(q.AmountMillis))
	output.Fprintf(os.Stderr, "  To address       %s\n                   %s\n", groupsOf4(q.Address), q.Address)
	output.Fprintf(os.Stderr, "  Network fee      %s\n  Hub fee          %s\n", f(q.NetworkFeeMillis), f(q.HubFeeMillis))
	output.Fprintf(os.Stderr, "  Recipient gets   %s (exact)\n", f(q.PayoutMillis))
	output.Fprintf(os.Stderr, "  Sent by          %s\n", time.Unix(q.SentBy, 0).UTC().Format(time.RFC3339))
	if 4*q.FeeMillis() > q.AmountMillis {
		output.Fprintf(os.Stderr, "  On-chain costs %s of %s here.\n", f(q.FeeMillis()), f(q.AmountMillis))
	}
	if e.ExpiresAt != nil && *e.ExpiresAt > 0 {
		output.Fprintf(os.Stderr, "  This bill expires %s\n", time.Unix(*e.ExpiresAt, 0).UTC().Format("2006-01-02"))
	}
	if isCashNote(e) {
		output.Fprintf(os.Stderr, "  This withdrawal can't be recovered if this device loses its data; keep cashctl.db until it is final.\n")
	}
	for _, w := range warnings {
		output.Fprintf(os.Stderr, "Warning: %s.\n", w)
	}
	output.Fprintf(os.Stderr, "\nCompare the WHOLE address with where you got it. On-chain payments can't be undone.\n")
}

// confirmIrreversible asks for "yes", only on a terminal and never under --json.
func confirmIrreversible() bool {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stderr.Fd())) {
		return false
	}
	output.Fprintf(os.Stderr, `Type "yes" to send (anything else: not sent): `)
	line, _ := stdin.ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(line), "yes")
}

// interactiveWithdraw reports whether this run may ask for "yes"; swapped in tests.
var interactiveWithdraw = func(cmd *cobra.Command) bool {
	jsonMode, _ := cmd.Flags().GetBool("json")
	return !jsonMode && term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}

// askYesFn is confirmIrreversible, swapped in tests.
var askYesFn = confirmIrreversible

func runCashWithdrawSend(cmd *cobra.Command, args []string) error {
	if err := rejectConnectionFlag(cmd); err != nil {
		return err
	}
	quoteHandle, _ := cmd.Flags().GetString("quote")
	yes, _ := cmd.Flags().GetBool("yes")
	var e *ledger.Entry
	var q *nipcash.WithdrawQuote
	switch {
	case quoteHandle != "":
		if !yes {
			return output.UsageError(cmd, errors.New("--quote sends that exact quote and needs --yes, which stands for the user's confirmation of it"))
		}
		stored, ok, err := ledger.FindWithdrawQuote(quoteHandle)
		if err != nil {
			return output.RuntimeError(cmd, err)
		}
		if !ok || stored.Used || time.Now().Unix()-stored.CreatedAt > ledger.QuoteMaxAge {
			return output.NotFoundError(cmd, quoteHandle, errors.New("no usable quote by that name (used, superseded or older than a day): quote again"))
		}
		l, err := ledger.Load()
		if err != nil {
			return output.RuntimeError(cmd, err)
		}
		e, ok = l.Find(stored.EntryID)
		if !ok || e.Status != ledger.StatusHeld {
			return output.NotFoundError(cmd, stored.EntryID, errors.New("the quoted bill is no longer held"))
		}
		if _, _, err := checkWithdrawAddress(cmd, stored.Address); err != nil {
			return err
		}
		if q, err = nipcash.ParseWithdrawQuote(stored.QuoteJSON); err != nil {
			return output.RuntimeError(cmd, err)
		}
		printWithdrawScreen(e, q, nil)
	case len(args) == 0:
		return output.InvocationError(cmd, errors.New("an address (or --quote <quote> --yes) is required"))
	case !interactiveWithdraw(cmd):
		return output.UsageError(cmd, errors.New(
			"quote first (`cashctl cash withdraw quote`), show the quote to the user, then `cashctl cash withdraw send --quote <quote> --yes`"))
	default:
		var err error
		e, q, quoteHandle, err = quoteWithdrawal(cmd, args[0], false)
		if err != nil {
			return err
		}
		printWithdrawScreen(e, q, nil)
		if !askYesFn() {
			output.Println("Not sent. Your cash is still yours.")
			return nil
		}
	}
	return sendWithdrawal(cmd, e, q, quoteHandle)
}

// withdrawJournal writes a withdrawal's call and its withdrawals row ahead
// together, and keeps the relays that accepted it.
type withdrawJournal struct {
	ledgerJournal
	w ledger.Withdrawal
}

func (j *withdrawJournal) WriteAhead(ctx context.Context, calls []nipcash.PendingCall) error {
	if err := j.ledgerJournal.WriteAhead(ctx, calls); err != nil {
		return err
	}
	c := j.call()
	if c == nil || len(calls) == 0 {
		return errors.New("the withdrawal's call was not written")
	}
	pc := calls[len(calls)-1]
	w := j.w
	if existing, ok, _ := ledger.WithdrawalForCall(c.CallID); ok {
		w = existing
	} else if w.WithdrawalID == "" {
		w.WithdrawalID = c.CallID
	}
	w.CallID = c.CallID
	if w.Notify == "" {
		w.Notify = pc.Notify
	}
	if w.State == "" {
		w.State = ledger.WithdrawSending
	}
	if err := ledger.PutWithdrawal(w); err != nil {
		j.l.RemoveCall(c.CallID)
		_ = j.l.Save()
		return err
	}
	if got, ok, err := ledger.FindWithdrawal(w.WithdrawalID); err != nil || !ok || got.Notify != w.Notify || got.QuoteID != w.QuoteID {
		j.l.RemoveCall(c.CallID)
		_ = j.l.Save()
		return errors.New("cashctl.db: the withdrawal's write-ahead did not read back as written")
	}
	j.w = w
	return nil
}

// RecordAccepted keeps the relays that accepted the withdrawal, for its notices.
func (j *withdrawJournal) RecordAccepted(_ context.Context, _ string, relays []string) error {
	w, ok, err := ledger.FindWithdrawal(j.w.WithdrawalID)
	if err != nil || !ok {
		return err
	}
	w.AcceptedRelays = relays
	return ledger.PutWithdrawal(w)
}

var _ nipcashclient.AcceptedRecorder = (*withdrawJournal)(nil)

// sendWithdrawal places cash_withdraw for the confirmed quote q of bill e.
func sendWithdrawal(cmd *cobra.Command, e *ledger.Entry, q *nipcash.WithdrawQuote, handle string) error {
	l, err := ledger.Load()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	e, ok := l.Find(e.ID)
	if ok && e.Status == ledger.StatusRedeeming {
		return errRedeemInFlight(cmd, e.ID)
	}
	if !ok || e.Status != ledger.StatusHeld {
		return output.NotFoundError(cmd, "", errors.New("the bill is no longer held"))
	}
	if err := refuseLocked(cmd, l, []string{e.ID}); err != nil {
		return err
	}
	if e.ExpiresAt != nil && *e.ExpiresAt > 0 && time.Until(time.Unix(*e.ExpiresAt, 0)) <= 120*time.Second {
		return output.WithReason(output.InvalidInputError(cmd, e.ID, errors.New("this bill expires too soon to withdraw safely; redeem it now")), reasonExpiring)
	}
	cred, err := resolveCredential(cmd, e)
	if err != nil {
		return err
	}
	device, err := deviceIDFn()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	kind, pub, secret := authorizerOf(cmd, e, cred)
	rawQuote, _ := q.Raw()
	j := &withdrawJournal{ledgerJournal: ledgerJournal{l: l, spec: callSpec{Purpose: purposeWithdraw, SourceEntryIDs: []string{e.ID},
		AuthorizerKind: kind, AuthorizerPubkey: pub, AuthorizerSecret: secret, TargetCredential: e.Token}},
		w: ledger.Withdrawal{SourceEntryID: e.ID, WalletPubkey: e.WalletPubkey, HubIdentity: xOnlyMinter(e),
			Address: q.Address, QuoteJSON: rawQuote, QuoteID: q.ID, AmountMillis: q.AmountMillis, NetworkFeeMillis: q.NetworkFeeMillis,
			HubFeeMillis: q.HubFeeMillis, FeeMillis: q.FeeMillis(), PayoutMillis: q.PayoutMillis, SentBy: q.SentBy,
			HoldsUntil: q.HoldsUntil, ConfirmedAt: time.Now().Unix()}}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	jsonMode, _ := cmd.Flags().GetBool("json")
	var payout *nipcash.Payout
	err = WithSpinner(jsonMode, "Sending the withdrawal...", func() error {
		c, dErr := dialWithdrawFn(ctx, e.Token)
		if dErr != nil {
			return &notSentError{dErr}
		}
		defer c.Close()
		var wErr error
		payout, wErr = c.Withdraw(callContext(ctx), nipcash.CashWithdrawParams{Quote: q, Credential: cred}, j, device)
		return wErr
	})
	if handle != "" {
		_ = ledger.UseWithdrawQuote(handle)
	}
	if err != nil {
		if c := j.call(); c != nil && errors.Is(err, nipcash.ErrReceiptInvalid) {
			pErr := placeErr(cmd, l, &j.ledgerJournal, err, "withdrawal", e.ID)
			return output.WithReason(pErr, reasonReceiptInvalid)
		}
		pErr := placeErr(cmd, l, &j.ledgerJournal, err, "withdrawal", e.ID)
		if c := j.call(); c != nil && c.State == ledger.CallNotApplied {
			w := j.w
			w.State, w.FinishedAt = refusedWithdrawState(c), time.Now().Unix()
			_ = ledger.PutWithdrawal(w)
		}
		return pErr
	}
	crashpoint.Hit("calls.before_apply")
	c := j.call()
	w := j.w
	if err := applyPayout(cmd, l, c, &w, payout); err != nil {
		return err
	}
	return printWithdrawal(cmd, w, payout)
}

// applyPayout records a payout as the Hub stated it: the source is spent from
// the first accepted answer; a final payout closes the call; a cancelled one
// brings the amount back as a new bill.
func applyPayout(cmd *cobra.Command, l *ledger.Ledger, c *ledger.Call, w *ledger.Withdrawal, p *nipcash.Payout) error {
	now := time.Now().Unix()
	if p == nil {
		return output.RuntimeError(cmd, errors.New("no payout"))
	}
	if p.Address != "" && w.Address != "" && p.Address != w.Address || p.PayoutMillis != w.PayoutMillis {
		c.State, c.Detail, c.UpdatedAt = ledger.CallStuck, "invalid_result", now
		_ = l.Save()
		return output.WithReason(output.IndeterminateError(cmd, "", errors.New("the Hub's payout doesn't match the quote you confirmed; the call is kept")), reasonReceiptInvalid)
	}
	if e, ok := l.Find(w.SourceEntryID); ok && e.Status == ledger.StatusHeld {
		_ = l.SetStatus(e.ID, ledger.StatusWithdrawn)
		l.AppendHistory("withdraw", fmt.Sprintf("withdrew %s on-chain (withdrawal %s)", output.FormatAmount(int64(w.AmountMillis)), w.WithdrawalID)) //nolint:gosec // amounts fit
	}
	raw, _ := json.Marshal(p)
	w.State, w.PayoutJSON, w.CheckedAt = string(p.State), string(raw), now
	switch p.State {
	case nipcash.PayoutFinal:
		w.FinishedAt = now
		closeCall(l, c, ledger.CallDone, "final")
	case nipcash.PayoutCancelled:
		id, err := importRemint(l, c, w, p)
		if err != nil {
			c.State, c.Detail, c.UpdatedAt = ledger.CallStuck, "invalid_result", now
			_ = l.Save()
			return output.IndeterminateError(cmd, "", fmt.Errorf("the payout was cancelled, but the bill that should come back doesn't check out (%v); its secret is kept", err))
		}
		w.RemintEntryID, w.FinishedAt = id, now
		closeCall(l, c, ledger.CallDone, "cancelled")
	default:
		retry := now + 600
		c.State, c.Detail, c.RetryAfter, c.UpdatedAt = ledger.CallOpenWithdraw, string(p.State), &retry, now
	}
	if err := l.Save(); err != nil {
		return output.RuntimeError(cmd, err)
	}
	return ledger.PutWithdrawal(*w)
}

// importRemint keeps the bill a cancelled payout re-minted: bound as the
// source was (a cash note with the call's re-mint secret).
func importRemint(l *ledger.Ledger, c *ledger.Call, w *ledger.Withdrawal, p *nipcash.Payout) (string, error) {
	if p.NewWalletToken.Tombstone != nil {
		return "", nil
	}
	token := strings.ToLower(p.NewWalletToken.Token)
	tok, err := nipcash.Decode(token)
	if err != nil {
		return "", err
	}
	if tok.WalletPubkey != p.NewWalletPubkey {
		return "", errors.New("the re-minted token is not the wallet named")
	}
	minter, ok := nipcash.VerifyProvenance(tok)
	if !ok || !nipcash.HubIdentityEqual(w.HubIdentity, minter) {
		return "", errors.New("the re-minted bill is not signed by the Hub")
	}
	amount := w.AmountMillis
	entry := ledger.Entry{Token: token, WalletPubkey: tok.WalletPubkey, Secret: tok.Secret, RelayURLs: tok.RelayURLs,
		AmountMillis: &amount, Verified: true, MinterPubkey: &minter, IdentityRequired: ptrTo(true)}
	if secret := callSecretOf(c, ledger.RoleRemint); secret != "" {
		entry.IdentityRequired, entry.CashSecret, entry.CashProtection = ptrTo(false), secret, ledger.CashProtected
	} else if src, ok := l.Find(w.SourceEntryID); ok {
		mode := credentialModeOf(*src)
		mode.Token, mode.WalletPubkey, mode.Secret, mode.RelayURLs = token, tok.WalletPubkey, tok.Secret, tok.RelayURLs
		mode.AmountMillis, mode.Verified, mode.MinterPubkey = &amount, true, &minter
		entry = mode
	}
	added, err := l.Add(entry)
	if err != nil {
		return "", err
	}
	l.AppendHistory("withdraw", fmt.Sprintf("withdrawal %s cancelled by the Hub; %s back as %s", w.WithdrawalID, output.FormatAmount(int64(amount)), added.ID)) //nolint:gosec // amounts fit
	return added.ID, nil
}

// withdrawalView is a withdrawal for --json and the screen. The address is
// shown only while the withdrawal is not final.
func withdrawalView(w ledger.Withdrawal) map[string]any {
	v := map[string]any{"withdrawal": w.WithdrawalID, "state": w.State, "token": w.SourceEntryID,
		"amount_millis": w.AmountMillis, "network_fee_millis": w.NetworkFeeMillis, "hub_fee_millis": w.HubFeeMillis,
		"fee_millis": w.FeeMillis, "payout_millis": w.PayoutMillis, "sent_by": w.SentBy, "finished": w.FinishedAt != 0}
	if w.Address != "" {
		v["address"] = w.Address
	}
	var p nipcash.Payout
	if w.PayoutJSON != "" && json.Unmarshal([]byte(w.PayoutJSON), &p) == nil {
		v["signed"], v["final_depth"] = p.Signed, p.FinalDepth
		if p.Txid != "" {
			v["txid"] = p.Txid
		}
		if p.Vout != nil {
			v["vout"] = *p.Vout
		}
		if p.Depth != nil {
			v["depth"] = *p.Depth
		}
		now := time.Now().Unix()
		if p.State == nipcash.PayoutQueued && now > w.SentBy {
			v["late"] = true
			v["tx_available"] = p.Signed && p.Tx != ""
		}
		v["cancel_available"] = p.State == nipcash.PayoutBroadcast && now >= w.SentBy+604800
	}
	if w.RemintEntryID != "" {
		v["remint_entry"] = w.RemintEntryID
	}
	v["next_step"] = withdrawalNextStep(w)
	return v
}

func withdrawalNextStep(w ledger.Withdrawal) string {
	switch w.State {
	case string(nipcash.PayoutFinal):
		return "nothing: received"
	case string(nipcash.PayoutCancelled):
		return "nothing: the amount is back as cash"
	case ledger.WithdrawNotApplied:
		return "nothing: not sent, your cash is still yours"
	case ledger.WithdrawBillGone:
		return "nothing: the bill was already spent, so nothing was withdrawn"
	case ledger.WithdrawSending:
		return "never resend it: `cashctl cash withdraw settle` asks the Hub what happened"
	}
	return "`cashctl cash withdraw show " + w.WithdrawalID + "` follows the payout"
}

func withdrawalText(w ledger.Withdrawal) string {
	f := func(v uint64) string { return output.FormatAmount(int64(v)) } //nolint:gosec // amounts fit
	switch w.State {
	case ledger.WithdrawSending:
		return "Sending your withdrawal… your bill is locked; never resend it"
	case ledger.WithdrawNotApplied:
		return "Not sent. Your cash is still yours."
	case ledger.WithdrawBillGone:
		return "Nothing withdrawn: the Hub says this bill was already spent."
	case string(nipcash.PayoutQueued):
		if time.Now().Unix() > w.SentBy {
			return fmt.Sprintf("Late. If it isn't sent by %s, your %s comes back as cash.",
				time.Unix(w.SentBy+86400, 0).UTC().Format(time.RFC3339), f(w.AmountMillis))
		}
		return fmt.Sprintf("Accepted: %s will be sent by %s", f(w.PayoutMillis), time.Unix(w.SentBy, 0).UTC().Format(time.RFC3339))
	case string(nipcash.PayoutBroadcast):
		return "Sent on-chain, waiting for confirmations"
	case string(nipcash.PayoutConfirmed):
		return "Confirmed"
	case string(nipcash.PayoutFinal):
		return "Received at " + w.Address
	case string(nipcash.PayoutCancelled):
		return fmt.Sprintf("The Hub did not send it. Your %s is back as cash: %s.", f(w.AmountMillis), w.RemintEntryID)
	}
	return w.State
}

func printWithdrawal(cmd *cobra.Command, w ledger.Withdrawal, _ *nipcash.Payout) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	if jsonMode {
		output.PrintJSON(withdrawalView(w))
		return nil
	}
	output.Printf("Withdrawal %s: %s\n", w.WithdrawalID, withdrawalText(w))
	if w.State == string(nipcash.PayoutQueued) {
		output.Println("Your bill is spent. Follow it: cashctl cash withdraw show " + w.WithdrawalID)
	}
	return nil
}
