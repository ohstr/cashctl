package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// withdrawPending is a withdraw call as nmilat reads it: its quote, address and
// notify.
func withdrawPending(c *ledger.Call, w ledger.Withdrawal) nipcash.PendingCall {
	pc := pendingOf(c)
	pc.Notify, pc.Address, pc.Quote, pc.HoldsUntil = w.Notify, w.Address, w.QuoteJSON, w.HoldsUntil
	return pc
}

// settleWithdrawCall reads one withdrawal and records what the Hub says.
func settleWithdrawCall(cmd *cobra.Command, l *ledger.Ledger, c *ledger.Call, device string) string {
	summary, _ := settleWithdrawOne(cmd, l, c, device)
	return summary
}

// settleWithdrawOne is settleWithdrawCall, also returning the withdrawal as
// recorded (its ReceivedAt set by the save that made it final).
func settleWithdrawOne(cmd *cobra.Command, l *ledger.Ledger, c *ledger.Call, device string) (string, *ledger.Withdrawal) {
	w, ok, err := ledger.WithdrawalForCall(c.CallID)
	if err != nil || !ok {
		return "no withdrawal record", nil
	}
	summary := settleWithdrawInto(cmd, l, c, device, &w)
	return summary, &w
}

func settleWithdrawInto(cmd *cobra.Command, l *ledger.Ledger, c *ledger.Call, device string, wp *ledger.Withdrawal) string {
	w := *wp
	defer func() { *wp = w }()
	var err error
	cred, err := callCredential(cmd, c)
	if err != nil {
		c.State, c.Detail = ledger.CallStuck, "needs_credential"
		_ = l.Save()
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	bill, err := dialWithdrawFn(ctx, c.TargetCredential)
	if err != nil {
		c.Detail, c.UpdatedAt = "lookup failed: "+output.Sanitize(err.Error()), time.Now().Unix()
		_ = l.Save()
		return ""
	}
	defer bill.Close()
	var payout *nipcash.Payout
	act, err := nipcashclient.Recover(ctx, withdrawPending(c, w), func(ctx context.Context, pc nipcash.PendingCall) (nipcashclient.LookupOutcome, error) {
		lo, p, err := bill.LookupWithdraw(ctx, pc, cred)
		payout = p
		return lo, err
	}, device, time.Now())
	c, _ = l.FindCall(c.CallID)
	if err != nil {
		c.Detail, c.UpdatedAt = "lookup failed: "+output.Sanitize(err.Error()), time.Now().Unix()
		_ = l.Save()
		return ""
	}
	if c.FirstDetail == string(nipcash.DetailQuoteExpired) && !w.QuoteExpiredSeen {
		w.QuoteExpiredSeen = true
		_ = ledger.PutWithdrawal(w)
	}
	if act.Kind == nipcashclient.ActionReplace && w.QuoteExpiredSeen {
		// A call of this withdrawal was refused quote_expired: its quote can't be
		// sent again on cashctl's own initiative.
		c.State, c.Detail, c.UpdatedAt = ledger.CallStuck, "quote_expired", time.Now().Unix()
		_ = l.Save()
		return "the quote expired: quote again (`cashctl cash withdraw quote`) and send the new quote"
	}
	if act.Kind == nipcashclient.ActionWait && payout != nil {
		if w.Recovered && w.PayoutMillis == 0 {
			adoptRecoveredPayout(&w, payout)
		}
		if err := applyPayout(cmd, l, c, &w, payout); err != nil {
			return "the payout doesn't check out; kept"
		}
		return withdrawalText(w)
	}
	summary := applyAction(cmd, l, c, act, device)
	if c, ok := l.FindCall(c.CallID); ok && c.State == ledger.CallNotApplied {
		w.State, w.FinishedAt = ledger.WithdrawNotApplied, time.Now().Unix()
		_ = ledger.PutWithdrawal(w)
	}
	return summary
}

func newCashWithdrawShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show [<withdrawal>...]",
		Short: "Read withdrawals from their Hub and show them",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rejectConnectionFlag(cmd); err != nil {
				return err
			}
			local, _ := cmd.Flags().GetBool("local")
			return showWithdrawals(cmd, args, local)
		},
	}
	cmd.Flags().Bool("local", false, "print what is stored, with no network")
	cmd.Flags().Bool("tx", false, "print the payout's raw transaction when it pays exactly this address (cashctl never broadcasts it)")
	cmd.Flags().Bool("receipt", false, "print the Hub's signed receipt (keep it private)")
	return cmd
}

func txCheckText(check string) string {
	if check == "mismatch" {
		return "the transaction the Hub sent doesn't pay this address exactly; don't broadcast it"
	}
	return "the Hub hasn't sent a signed transaction"
}

func showWithdrawals(cmd *cobra.Command, ids []string, local bool) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	all, err := ledger.Withdrawals()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	var pick []ledger.Withdrawal
	week := time.Now().Unix() - 7*86400
	for _, w := range all {
		if len(ids) == 0 && (w.FinishedAt == 0 || w.FinishedAt > week) {
			pick = append(pick, w)
		}
	}
	for _, id := range ids {
		w, ok, err := ledger.FindWithdrawal(id)
		if err != nil {
			return output.RuntimeError(cmd, err)
		}
		if !ok {
			return output.NotFoundError(cmd, id, fmt.Errorf("no withdrawal %q", id))
		}
		pick = append(pick, w)
	}
	if !local {
		l, err := ledger.Load()
		if err != nil {
			return output.RuntimeError(cmd, err)
		}
		device, _ := deviceIDFn()
		for i, w := range pick {
			if c, ok := l.FindCall(w.CallID); ok && c.Open() {
				if _, got := settleWithdrawOne(cmd, l, c, device); got != nil {
					pick[i].ReceivedAt = got.ReceivedAt
				}
			}
		}
	}
	showTx, _ := cmd.Flags().GetBool("tx")
	showReceipt, _ := cmd.Flags().GetBool("receipt")
	views := []map[string]any{}
	for _, w := range pick {
		fresh, _, _ := ledger.FindWithdrawal(w.WithdrawalID)
		fresh.ReceivedAt = w.ReceivedAt
		v := withdrawalView(fresh)
		var p nipcash.Payout
		_ = json.Unmarshal([]byte(fresh.PayoutJSON), &p)
		if showTx {
			if txCheck(fresh, p) == "ok" {
				v["tx"] = p.Tx
				if !jsonMode {
					output.Printf("%s\n", p.Tx)
					output.Notef(false, "Broadcast it with any node or service you trust; cashctl never broadcasts it.")
				}
			} else if !jsonMode {
				output.Notef(false, "No transaction to broadcast for %s (%s).", fresh.WithdrawalID, txCheckText(txCheck(fresh, p)))
			}
		}
		if showReceipt && p.Receipt != "" {
			v["receipt"] = p.Receipt
			if !jsonMode {
				output.Printf("%s\n", p.Receipt)
				output.Notef(false, "The Hub's signed promise for this payout; keep it private.")
			}
		}
		views = append(views, v)
		if !jsonMode {
			output.Printf("withdrawal %s: %s\n", fresh.WithdrawalID, withdrawalText(fresh))
		}
	}
	if jsonMode {
		output.PrintJSON(map[string]any{"withdrawals": views})
	} else if len(views) == 0 {
		output.Println("No withdrawals to show.")
	}
	return nil
}

func newCashWithdrawCancelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cancel <withdrawal>",
		Short: "Ask the Hub to cancel a payout sent but unconfirmed for 7 days",
		Args:  output.ExactArgs(1),
		RunE:  runCashWithdrawCancel,
	}
	cmd.Flags().String("as", "", "the credential the withdrawal was placed with under --as")
	return cmd
}

func runCashWithdrawCancel(cmd *cobra.Command, args []string) error {
	if err := rejectConnectionFlag(cmd); err != nil {
		return err
	}
	w, ok, err := ledger.FindWithdrawal(args[0])
	if err != nil || !ok {
		return output.NotFoundError(cmd, args[0], fmt.Errorf("no withdrawal %q", args[0]))
	}
	after := w.SentBy + 604800
	if w.State != string(nipcash.PayoutBroadcast) || time.Now().Unix() < after {
		return output.WithReason(output.InvalidInputError(cmd, args[0], fmt.Errorf(
			"a cancel can be asked from %s, only while the payout is sent but unconfirmed",
			time.Unix(after, 0).UTC().Format(time.RFC3339))), reasonCancelNotDue)
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	yes, _ := cmd.Flags().GetBool("yes")
	if jsonMode && !yes {
		return output.UsageError(cmd, errors.New("asking the Hub to cancel needs --yes under --json"))
	}
	if !Confirm(cmd, false, "Ask the Hub to cancel? If a version confirms first, you are paid instead") {
		output.Println("Cancelled.")
		return nil
	}
	l, err := ledger.Load()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	c, ok := l.FindCall(w.CallID)
	if !ok {
		return output.NotFoundError(cmd, w.CallID, errors.New("the withdrawal's call is gone"))
	}
	cred, err := callCredential(cmd, c)
	if err != nil {
		return output.UsageError(cmd, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	bill, err := dialWithdrawFn(ctx, c.TargetCredential)
	if err != nil {
		return output.NetworkError(cmd, err)
	}
	defer bill.Close()
	payout, err := bill.CancelWithdraw(ctx, withdrawPending(c, w), cred)
	var refused *nipcashclient.CancelRefusedError
	switch {
	case errors.As(err, &refused):
		msg := fmt.Errorf("the Hub refused the cancel: %s %s", refused.Code, output.Sanitize(refused.Message))
		switch refused.Code {
		case "RESTRICTED", "UNAUTHORIZED":
			return output.AuthError(cmd, msg)
		case "NOT_FOUND":
			return output.NotFoundError(cmd, args[0], msg)
		}
		return output.InvalidInputError(cmd, args[0], msg)
	case err != nil:
		return output.NetworkError(cmd, err)
	}
	if err := applyPayout(cmd, l, c, &w, payout); err != nil {
		return err
	}
	if !jsonMode && payout.State == nipcash.PayoutBroadcast {
		output.Println("Cancel requested. The payout stays 'sent' until the Hub's cancelling transaction is final; cashctl keeps following it.")
	}
	return printWithdrawal(cmd, w, payout)
}

func newCashWithdrawSettleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "settle [<withdrawal>]",
		Short: "Finish withdrawals whose answer never came",
		Args:  output.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rejectConnectionFlag(cmd); err != nil {
				return err
			}
			l, err := ledger.Load()
			if err != nil {
				return output.RuntimeError(cmd, err)
			}
			device, _ := deviceIDFn()
			if id, _ := cmd.Flags().GetString("replace"); id != "" {
				return replaceWithdrawalByUser(cmd, l, id, device)
			}
			ws, _ := ledger.Withdrawals()
			stillOpen := false
			for _, w := range ws {
				if len(args) == 1 && w.WithdrawalID != args[0] {
					continue
				}
				if c, ok := l.FindCall(w.CallID); ok && c.Open() {
					settleWithdrawCall(cmd, l, c, device)
					if c, ok := l.FindCall(w.CallID); ok && c.Open() && c.State != ledger.CallOpenWithdraw && c.DeviceID == device {
						stillOpen = true
					}
				}
			}
			if err := showWithdrawals(cmd, args, true); err != nil {
				return err
			}
			if stillOpen {
				return output.IndeterminateError(cmd, "", errors.New("a withdrawal still has no answer; nothing was resent"))
			}
			return nil
		},
	}
	cmd.Flags().String("replace", "", "send again a withdrawal the Hub says never arrived (same quote)")
	cmd.Flags().String("as", "", "the credential a withdrawal was placed with under --as")
	return cmd
}

// replaceWithdrawalByUser sends again, under the derived id and with the same
// quote, a withdrawal stuck in confirm_replace, after the user agreed.
func replaceWithdrawalByUser(cmd *cobra.Command, l *ledger.Ledger, id, device string) error {
	w, ok, err := ledger.FindWithdrawal(id)
	if err != nil || !ok {
		return output.NotFoundError(cmd, id, fmt.Errorf("no withdrawal %q", id))
	}
	c, ok := l.FindCall(w.CallID)
	if !ok || c.State != ledger.CallStuck || c.Detail != "confirm_replace" {
		return output.ConflictError(cmd, id, errors.New("only a withdrawal the Hub says never arrived can be sent again — run `cashctl cash withdraw settle` first"))
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	yes, _ := cmd.Flags().GetBool("yes")
	if jsonMode && !yes {
		return output.UsageError(cmd, errors.New("sending a withdrawal again needs --yes under --json"))
	}
	if !Confirm(cmd, false, fmt.Sprintf("The Hub says this withdrawal never arrived. Send it again: %s to %s?",
		output.FormatAmount(int64(w.PayoutMillis)), w.Address)) { //nolint:gosec // amounts fit
		output.Println("Cancelled.")
		return nil
	}
	summary := replaceCallConfirmed(cmd, l, c, true)
	_ = l.Save()
	fresh, _, _ := ledger.FindWithdrawal(id)
	if !jsonMode && summary != "" {
		output.Println(summary)
	}
	return printWithdrawal(cmd, fresh, nil)
}

// adoptRecoveredPayout fills a recovered withdrawal from its first payout: the
// numbers the Hub states, and the quote id its receipt names.
func adoptRecoveredPayout(w *ledger.Withdrawal, p *nipcash.Payout) {
	w.Address, w.AmountMillis, w.FeeMillis, w.PayoutMillis, w.SentBy = p.Address, p.AmountMillis, p.FeeMillis, p.PayoutMillis, p.SentBy
	if id, err := nipcash.ReceiptQuoteID(p.Receipt); err == nil {
		w.QuoteID = id
	}
}

// withdrawRequestIDOf is the withdraw_request_id a cash_status answer names: on
// the tombstone of a bill a withdrawal destroyed, or on the row it ended.
func withdrawRequestIDOf(res *nipcash.CashStatusResult) string {
	if res == nil {
		return ""
	}
	if res.Tombstone != nil {
		return res.Tombstone.WithdrawRequestID
	}
	for _, r := range res.Recipients {
		if r.WithdrawRequestID != "" {
			return r.WithdrawRequestID
		}
	}
	return ""
}

// recoverWithdrawal records a withdrawal of e that this device has no record
// of (its data was lost, or it was placed elsewhere): the bill is withdrawn,
// and a call of device "recovered" is looked up with that request id. It is
// never re-placed; its receipt is checked against the payout it answers with,
// and its quote id is taken from the receipt.
func recoverWithdrawal(cmd *cobra.Command, l *ledger.Ledger, e *ledger.Entry, rid string) (ledger.Withdrawal, error) {
	kind, pub, secret := authorizerOf(cmd, e, nil)
	var amount uint64
	if e.AmountMillis != nil {
		amount = *e.AmountMillis
	}
	c, err := l.AddCall(ledger.Call{RequestID: rid, Method: nipcash.MethodCashWithdraw, Purpose: purposeWithdraw,
		Deadline: time.Now().Unix(), SentH: 60, HubIdentity: xOnlyMinter(e), Target: e.WalletPubkey, TargetCredential: e.Token,
		DeviceID: ledger.DeviceRecovered, AmountMillis: amount, Chain: chainOfToken(e.Token), AuthorizerKind: kind,
		AuthorizerPubkey: pub, AuthorizerSecret: secret, SourceEntryIDs: []string{e.ID}, ParamsJSON: "{}", State: ledger.CallLookup,
		Detail: "recovered"})
	if err != nil {
		return ledger.Withdrawal{}, err
	}
	callID := c.CallID
	_ = l.SetStatus(e.ID, ledger.StatusWithdrawn)
	l.AppendHistory("withdraw", "found a withdrawal this device has no record of (withdrawal "+callID+")")
	if err := l.Save(); err != nil {
		return ledger.Withdrawal{}, err
	}
	w := ledger.Withdrawal{WithdrawalID: callID, CallID: callID, SourceEntryID: e.ID, WalletPubkey: e.WalletPubkey,
		HubIdentity: xOnlyMinter(e), AmountMillis: amount, State: ledger.WithdrawSending, Recovered: true}
	return w, ledger.PutWithdrawal(w)
}
