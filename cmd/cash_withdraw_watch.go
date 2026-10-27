package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// How often watch reads a payout with no notice: every minute until it is
// broadcast, then every ten (no notice marks confirmed or final). A notice
// triggers a read at most every two seconds. Swapped in tests.
var (
	withdrawPollBeforeBroadcast = 60 * time.Second
	withdrawPollAfterBroadcast  = 10 * time.Minute
	withdrawNoticeGap           = 2 * time.Second
)

func newCashWithdrawWatchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "watch <withdrawal>",
		Short: "Follow a payout until it is final or cancelled",
		Args:  output.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rejectConnectionFlag(cmd); err != nil {
				return err
			}
			timeout, _ := cmd.Flags().GetDuration("timeout")
			return watchWithdrawal(cmd, args[0], timeout)
		},
	}
	cmd.Flags().Duration("timeout", 30*time.Minute, "stop after this long")
	cmd.Flags().String("as", "", "the credential the withdrawal was placed with under --as")
	return cmd
}

// watchWithdrawal reads a withdrawal once, then on each state notice (from
// the relays that accepted the call first) and on a poll, saving each answer
// before the next, until it is final or cancelled or the timeout passes.
func watchWithdrawal(cmd *cobra.Command, id string, timeout time.Duration) error {
	w, ok, err := ledger.FindWithdrawal(id)
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	if !ok {
		return output.NotFoundError(cmd, id, fmt.Errorf("no withdrawal %q", id))
	}
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	device, _ := deviceIDFn()
	received := ""
	read := func() bool {
		l, err := ledger.Load()
		if err != nil {
			return false
		}
		c, ok := l.FindCall(w.CallID)
		if !ok || !c.Open() {
			return true
		}
		if _, got := settleWithdrawOne(cmd, l, c, device); got != nil && got.ReceivedAt != "" {
			received = got.ReceivedAt
		}
		w, _, _ = ledger.FindWithdrawal(id)
		return w.FinishedAt != 0
	}
	done := read()
	var notices <-chan struct{}
	if !done && w.Notify != "" {
		if l, err := ledger.Load(); err == nil {
			if c, ok := l.FindCall(w.CallID); ok {
				if bill, err := dialWithdrawFn(ctx, c.TargetCredential); err == nil {
					defer bill.Close()
					notices, _ = bill.WatchPayout(ctx, w.Notify, w.AcceptedRelays)
				}
			}
		}
	}
	last := time.Now()
	for !done && ctx.Err() == nil {
		poll := withdrawPollAfterBroadcast
		if w.State == string(nipcash.PayoutQueued) || w.State == ledger.WithdrawSending {
			poll = withdrawPollBeforeBroadcast
		}
		timer := time.NewTimer(poll)
		select {
		case _, ok := <-notices:
			if !ok {
				notices = nil
			}
			if wait := withdrawNoticeGap - time.Since(last); wait > 0 {
				time.Sleep(wait)
			}
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
		if ctx.Err() != nil {
			break
		}
		last = time.Now()
		done = read()
	}
	w.ReceivedAt = received
	if l, err := ledger.Load(); err == nil {
		if c, ok := l.FindCall(w.CallID); ok && c.Open() && c.State != ledger.CallOpenWithdraw {
			return output.IndeterminateError(cmd, id, errors.New("the withdrawal's placement still has no answer; nothing was resent — run `cashctl cash withdraw settle`"))
		}
	}
	return printWithdrawal(cmd, w, nil)
}
