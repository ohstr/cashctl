package cmd

import (
	"context"
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
	w, ok, err := ledger.WithdrawalForCall(c.CallID)
	if err != nil {
		return "no withdrawal record"
	}
	if !ok {
		// The row is written before anything is sent: a call without one
		// stopped between its two writes and never went out.
		closeCall(l, c, ledger.CallNotApplied, "not sent")
		_ = l.Save()
		return "never sent: the bill is still yours"
	}
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
	if act.Kind == nipcashclient.ActionWait && payout != nil {
		if err := applyPayout(cmd, l, c, &w, payout); err != nil {
			return "the payout doesn't check out; kept"
		}
		return withdrawalText(w)
	}
	summary := applyAction(cmd, l, c, act, device)
	finishRefusedWithdrawal(l, c.CallID)
	return summary
}

// finishRefusedWithdrawal ends the withdrawal row of call id once that call is
// closed not applied (a refusal, or a re-placement the Hub refused).
func finishRefusedWithdrawal(l *ledger.Ledger, id string) {
	c, ok := l.FindCall(id)
	if !ok || c.Method != nipcash.MethodCashWithdraw || c.State != ledger.CallNotApplied {
		return
	}
	if w, ok, _ := ledger.WithdrawalForCall(id); ok && w.FinishedAt == 0 {
		w.State, w.FinishedAt = refusedWithdrawState(c), time.Now().Unix()
		_ = ledger.PutWithdrawal(w)
	}
}

// refusedWithdrawState is the withdrawal state of a call closed not applied:
// bill_gone when the Hub answered with the bill's tombstone (the bill was
// already spent), else not_applied (the bill is still yours).
func refusedWithdrawState(c *ledger.Call) string {
	if c.Detail == "tombstone" {
		return ledger.WithdrawBillGone
	}
	return ledger.WithdrawNotApplied
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
	return cmd
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
		for _, w := range pick {
			if c, ok := l.FindCall(w.CallID); ok && c.Open() {
				settleWithdrawCall(cmd, l, c, device)
			}
		}
	}
	views := []map[string]any{}
	for _, w := range pick {
		fresh, _, _ := ledger.FindWithdrawal(w.WithdrawalID)
		views = append(views, withdrawalView(fresh))
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
