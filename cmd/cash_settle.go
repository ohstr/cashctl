package cmd

import (
	"errors"
	"fmt"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// newCashSettleCmd asks the Hubs about every call whose answer never came and
// acts on what they say: a call that happened is applied, one the Hub never saw
// is placed again under its derived id (transfers and merges only), one it
// refused with a code is closed with that code, and one it no longer remembers
// is shown with its secrets. Every money command settles what is due before it
// starts; this does it on demand.
func newCashSettleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "settle",
		Short: "Ask the Hubs about calls whose answer never came, and finish them",
		Long: `Looks up every call whose answer never came — a transfer, merge or re-key
whose reply was lost — and finishes it from the Hub's answer: one that happened
is recorded, one the Hub never saw is placed again (under its derived id,
never twice), one it refused is closed with its reason. Nothing is resent
under its own id. Exit 0 when nothing placed from this wallet file is still
open, else 8.

--close <call> gives up on one call (its new codes are printed: keep them);
not a withdrawal or an answered order, and under --json only with --yes.
--wait keeps asking, up to 6 minutes, until nothing is due.`,
		Example: `  cashctl cash settle
  cashctl cash settle --wait
  cashctl cash settle --close 1f0d61013abe`,
		Args: output.NoArgs,
		RunE: runCashSettle,
	}
	cmd.Flags().Bool("wait", false, "keep asking until nothing is due, up to 6 minutes")
	cmd.Flags().Bool("no-wait", false, "ask once and exit")
	cmd.Flags().String("close", "", "close one call by its handle, giving up on it (its new codes are printed)")
	cmd.Flags().String("replace", "", "re-place a mint the Hub never saw (only a mint needs this)")
	cmd.Flags().String("as", "", "the credential a call was placed with under --as")
	return cmd
}

// settleWaitLimit bounds `cash settle --wait`.
const settleWaitLimit = 6 * time.Minute

func runCashSettle(cmd *cobra.Command, _ []string) error {
	if err := rejectConnectionFlag(cmd); err != nil {
		return err
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	wait, _ := cmd.Flags().GetBool("wait")
	closeID, _ := cmd.Flags().GetString("close")
	replaceID, _ := cmd.Flags().GetString("replace")
	l, err := ledger.Load()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	device, _ := deviceIDFn()

	if replaceID != "" {
		return replaceMintByUser(cmd, l, replaceID, device, jsonMode)
	}
	if closeID != "" {
		return closeCallByUser(cmd, l, closeID, device, jsonMode)
	}

	deadline := time.Now().Add(settleWaitLimit)
	settled := settleCalls(cmd, l, settleOptions{all: true})
	for wait && time.Now().Before(deadline) {
		next, ok := nextDue(l)
		if !ok || next.After(deadline) {
			break
		}
		time.Sleep(time.Until(next))
		settled = append(settled, settleCalls(cmd, l, settleOptions{})...)
	}
	joins, joinsOpen := settleJoins(cmd, device)
	return reportSettle(cmd, l, settled, joins, joinsOpen, device, jsonMode)
}

// nextDue is when the earliest open call becomes worth asking about again.
func nextDue(l *ledger.Ledger) (time.Time, bool) {
	var next int64
	for _, c := range l.Calls {
		if !c.Open() || c.State == ledger.CallExpired || c.State == ledger.CallStuck || c.State == ledger.CallOpenOrder {
			continue
		}
		at := c.Deadline + 1
		if c.RetryAfter != nil {
			at = *c.RetryAfter
		}
		if next == 0 || at < next {
			next = at
		}
	}
	return time.Unix(next, 0), next != 0
}

// settleRow is one call in `cash settle`'s report.
type settleRow struct {
	Call         string   `json:"call"`
	Method       string   `json:"method"`
	Purpose      string   `json:"purpose"`
	State        string   `json:"state"`
	Detail       string   `json:"detail,omitempty"`
	Deadline     int64    `json:"deadline"`
	RetryAfter   *int64   `json:"retry_after,omitempty"`
	TokenIDs     []string `json:"token_ids"`
	AmountMillis uint64   `json:"amount_millis"`
	Device       string   `json:"device"`
	NextStep     string   `json:"next_step"`
	Summary      string   `json:"summary,omitempty"`
	Recovery     string   `json:"recovery,omitempty"`
}

func rowOf(c ledger.Call, device, summary string) settleRow {
	r := settleRow{Call: c.CallID, Method: c.Method, Purpose: c.Purpose, State: c.State, Detail: c.Detail,
		Deadline: c.Deadline, RetryAfter: c.RetryAfter, TokenIDs: output.NonNil(c.SourceEntryIDs),
		AmountMillis: c.AmountMillis, Device: "other", NextStep: nextStep(&c, device), Summary: summary}
	if c.DeviceID == device {
		r.Device = "this"
	}
	if c.State == ledger.CallExpired || c.State == ledger.CallStuck {
		r.Recovery = callRecovery(&c)
	}
	return r
}

// callRecovery is a call's generated secrets as text to keep.
func callRecovery(c *ledger.Call) string {
	out := ""
	for _, s := range c.Secrets {
		if out != "" {
			out += "; "
		}
		out += fmt.Sprintf("new code (%s) for %v: %s", s.Role, c.SourceEntryIDs, s.Secret)
	}
	return out
}

// reportSettle prints every call looked at and every call still open, and exits
// 8 while anything placed from this file is still open.
func reportSettle(cmd *cobra.Command, l *ledger.Ledger, settled []settledCall, joins []joinSettleRow, joinsOpen bool, device string, jsonMode bool) error {
	rows := map[string]settleRow{}
	var order []string
	for _, s := range settled {
		if _, seen := rows[s.Call.CallID]; !seen {
			order = append(order, s.Call.CallID)
		}
		rows[s.Call.CallID] = rowOf(s.Call, device, s.Summary)
	}
	stillOpen := false
	for _, c := range l.Calls {
		if c.Open() {
			if _, seen := rows[c.CallID]; !seen {
				order = append(order, c.CallID)
				rows[c.CallID] = rowOf(c, device, "")
			}
			// An open order is answered: it waits for its payment, not for
			// cash settle.
			if c.DeviceID == device && c.State != ledger.CallOpenOrder && c.State != ledger.CallOpenWithdraw {
				stillOpen = true
			}
		}
	}
	list := make([]settleRow, 0, len(order))
	for _, id := range order {
		list = append(list, rows[id])
	}
	if jsonMode {
		output.PrintJSON(map[string]any{"calls": list, "joins": output.NonNil(joins)})
	} else {
		if len(list) == 0 && len(joins) == 0 {
			output.Println("Nothing to settle.")
		}
		for _, j := range joins {
			line := fmt.Sprintf("join %s at Hub %s: %s", shortFingerprint(j.ConnectionPubkey), shortFingerprint(j.HubPubkey), j.State)
			if j.Summary != "" {
				line += " — " + j.Summary
			} else if j.Detail != "" {
				line += " — " + j.Detail
			}
			output.Println(line)
		}
		for _, r := range list {
			line := fmt.Sprintf("call %s (%s of %v): %s", r.Call, r.Purpose, r.TokenIDs, r.State)
			if r.Summary != "" {
				line += " — " + r.Summary
			} else if r.Detail != "" {
				line += " — " + r.Detail
			}
			output.Println(line)
			if r.State != ledger.CallDone && r.State != ledger.CallNotApplied && r.State != ledger.CallReplaced {
				output.Printf("  next: %s\n", r.NextStep)
			}
			if r.Recovery != "" {
				output.Printf("  keep: %s\n", r.Recovery)
			}
		}
	}
	if stillOpen || joinsOpen {
		return output.IndeterminateError(cmd, "", errors.New("some calls still have no answer; nothing was resent — run `cashctl cash settle` again later"))
	}
	return nil
}

// closeCallByUser gives up on one call. Its generated secrets are printed
// first: once closed, cashctl stops asking about it.
// refuseUserClose keeps `--close` off calls whose keys are still needed: a
// withdrawal (its payout is followed, and a cancelled payout is re-minted to
// its remint secret) and an answered order (later bills are fetched with its
// buyer key). Those are followed by their own commands.
func refuseUserClose(cmd *cobra.Command, c *ledger.Call) error {
	switch {
	case c.Method == nipcash.MethodCashWithdraw && (c.State == ledger.CallOpenWithdraw || callSecretOf(c, ledger.RoleRemint) != ""):
		next := "`cashctl cash withdraw settle`"
		if w, ok, _ := ledger.WithdrawalForCall(c.CallID); ok {
			next = fmt.Sprintf("`cashctl cash withdraw show %s` (or `cash withdraw cancel %s`)", w.WithdrawalID, w.WithdrawalID)
		}
		return output.UsageError(cmd, fmt.Errorf("call %s is a withdrawal: its payout and re-mint code are still needed, so it can't be closed here — follow it with %s", c.CallID, next))
	case c.Method == nipcash.MethodCashOrder && c.State == ledger.CallOpenOrder:
		return output.UsageError(cmd, fmt.Errorf("call %s is an order the Hub answered: its key fetches the order's bills, so it can't be closed here — follow it with `cashctl cash order show`", c.CallID))
	}
	return nil
}

func closeCallByUser(cmd *cobra.Command, l *ledger.Ledger, id, device string, jsonMode bool) error {
	c, ok := l.FindCall(id)
	if !ok {
		return output.NotFoundError(cmd, id, fmt.Errorf("no call %q", id))
	}
	if !c.Open() {
		return output.ConflictError(cmd, id, fmt.Errorf("call %s is already %s", id, c.State))
	}
	if err := refuseUserClose(cmd, c); err != nil {
		return err
	}
	if yes, _ := cmd.Flags().GetBool("yes"); jsonMode && !yes {
		return output.UsageError(cmd, errors.New("closing a call gives up on its answer: pass --yes"))
	}
	recovery := callRecovery(c)
	if !Confirm(cmd, false, fmt.Sprintf("Stop asking about call %s? If it happened, its result is only reachable with the codes printed below.", id)) {
		output.Println("Cancelled.")
		return nil
	}
	closeCall(l, c, ledger.CallClosed, "closed by you")
	l.AppendHistory("settle", fmt.Sprintf("closed call %s without an answer", id))
	if err := l.Save(); err != nil {
		return output.RuntimeError(cmd, err)
	}
	row := rowOf(*c, device, "closed")
	row.Recovery = recovery
	if jsonMode {
		output.PrintJSON(map[string]any{"calls": []settleRow{row}, "joins": []joinSettleRow{}})
		return nil
	}
	output.Printf("Closed call %s.\n", id)
	if recovery != "" {
		output.Printf("Keep: %s\n", recovery)
	}
	return nil
}

// replaceMintByUser re-places a mint the Hub said it never saw, under its
// derived id, after the user asked for it (`cash settle --replace`): a mint is
// never placed again on cashctl's own initiative.
func replaceMintByUser(cmd *cobra.Command, l *ledger.Ledger, id, device string, jsonMode bool) error {
	c, ok := l.FindCall(id)
	if !ok {
		return output.NotFoundError(cmd, id, fmt.Errorf("no call %q", id))
	}
	if c.Method != nipcash.MethodMintCash {
		return output.UsageError(cmd, fmt.Errorf("call %s is a %s, which cashctl re-places on its own when the Hub never saw it; --replace is only for a mint", c.CallID, c.Method))
	}
	if c.State != ledger.CallStuck || c.Detail != "confirm_replace" {
		return output.ConflictError(cmd, id, fmt.Errorf("call %s is %s: only a mint the Hub said it never saw can be placed again — run `cashctl cash settle` first", id, c.State))
	}
	if !Confirm(cmd, false, fmt.Sprintf("The Hub says it never saw mint %s. Mint it again (under its derived id, never twice)?", id)) {
		output.Println("Cancelled.")
		return nil
	}
	summary := replaceCallConfirmed(cmd, l, c, true)
	_ = l.Save()
	c, _ = l.FindCall(id)
	row := rowOf(*c, device, summary)
	if jsonMode {
		output.PrintJSON(map[string]any{"calls": []settleRow{row}, "joins": []joinSettleRow{}})
		return nil
	}
	output.Printf("call %s: %s", id, c.State)
	if summary != "" {
		output.Printf(" — %s", summary)
	}
	output.Println()
	return nil
}
