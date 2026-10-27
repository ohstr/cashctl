package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ohstr/nmilat/nip47"
	"github.com/ohstr/nmilat/nipcw"
	nipcwclient "github.com/ohstr/nmilat/nipcw/client"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/crashpoint"
	"github.com/ohstr/cashctl/internal/output"
)

// A member deletes their own Circle Wallet with delete_circle_wallet over that
// wallet's own connection (NIP-CW §Deleting a Circle Wallet), once it holds at
// most dust, after being told the dust goes to the host. The deletion is
// written ahead on the wallet's row; a lost reply keeps the wallet here, and
// a later `wallet delete` asks the Hub again.

const (
	reasonAboveDust       = "above_dust"
	reasonDustNotAccepted = "dust_not_accepted"
	reasonDeleteUnknown   = "delete_unknown"
)

// circleWalletClient is a member's own Circle Wallet connection, behind an
// interface so tests can stand in for the Hub.
type circleWalletClient interface {
	GetBalance(ctx context.Context) (*nip47.GetBalanceResult, error)
	DeleteWallet(ctx context.Context) error
	Close()
}

type liveCircleWallet struct {
	nwc *relayclient.NWCClient
	w   config.CircleWallet
}

func (l liveCircleWallet) GetBalance(ctx context.Context) (*nip47.GetBalanceResult, error) {
	return l.nwc.GetBalance(ctx)
}

// DeleteWallet sends delete_circle_wallet; the member already agreed to leave
// the dust to the host.
func (l liveCircleWallet) DeleteWallet(ctx context.Context) error {
	return nipcwclient.DeleteWallet(ctx, nipcw.WalletConnection{WalletPubkey: l.w.WalletPubkey, Relays: l.w.Relays, Secret: l.w.Secret},
		func(context.Context) bool { return true })
}

func (l liveCircleWallet) Close() { l.nwc.Close() }

// dialCircleWalletFn dials a member's own Circle Wallet (value is its stored
// fields), swapped out in tests.
var dialCircleWalletFn = func(ctx context.Context, value string) (circleWalletClient, error) {
	w, _, err := config.ParseCircleWallet(value)
	if err != nil {
		return nil, err
	}
	nwc, err := DialGeneric(ctx, value)
	if err != nil {
		return nil, err
	}
	return liveCircleWallet{nwc: nwc, w: w}, nil
}

func newWalletDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete [name]",
		Short: "Delete one of your circle wallets once it holds at most dust",
		Long: `Deletes a circle wallet of yours on its Hub, over the wallet's own connection,
then forgets it here. The Hub refuses while the wallet holds more than its dust
threshold or has a payment in flight: pay out first. Whatever dust is left goes to
the circle's host, and you are asked to agree to that (--yes; --json alone does
not count). [name] defaults to the current wallet.`,
		Example: `  cashctl wallet delete circle-1
  cashctl wallet delete circle-1 --yes --json`,
		Args: output.MaximumNArgs(1),
		RunE: runWalletDelete,
	}
}

func runWalletDelete(cmd *cobra.Command, args []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	yes, _ := cmd.Flags().GetBool("yes")
	s, err := config.Load()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	var c *config.Connection
	switch {
	case len(args) == 1:
		c, _ = s.Find(args[0])
		if c == nil {
			return output.NotFoundError(cmd, args[0], fmt.Errorf("no wallet named %q; see `cashctl connect list`", output.Sanitize(args[0])))
		}
	default:
		explicit, _ := cmd.Flags().GetString("connection")
		if explicit != "" {
			c, _ = s.Find(explicit)
		} else {
			c, _ = s.DefaultConnection()
		}
		if c == nil {
			return output.NotFoundError(cmd, explicit, errors.New(noWalletMessage()))
		}
	}
	w, ok, err := config.ParseCircleWallet(c.Value)
	if !ok || err != nil || c.Kind != config.KindCircle {
		return output.InvalidInputError(cmd, c.Name, fmt.Errorf(
			"%q is not a circle wallet: only a circle wallet is deleted on its Hub; `cashctl connect rm %s` forgets another connection", c.Name, c.Name))
	}
	name := c.Name

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	ctx, err = allowRotationUse(cmd, ctx, c.Value)
	if err != nil {
		return err
	}
	client, err := dialCircleWalletFn(ctx, c.Value)
	if err != nil {
		return output.NetworkError(cmd, err)
	}
	defer client.Close()

	bal, err := client.GetBalance(ctx)
	if err != nil {
		var walletErr *relayclient.WalletError
		if w.DeleteSentAt != 0 && !errors.As(err, &walletErr) {
			return deleteUnknownErr(cmd, name, w, fmt.Errorf(
				"the Hub does not answer this wallet's key: it is unreachable, or the delete sent at %s went through (a deleted wallet's key gets no answer). Try again later; once you're sure it is gone, `cashctl connect rm --force %s` forgets it here",
				time.Unix(w.DeleteSentAt, 0).UTC().Format(time.RFC3339), name))
		}
		return classifyWalletCallErr(cmd, err)
	}
	balance := bal.BalanceMloki
	if w.DetailsKnown && balance > int64(w.DustThresholdMillis) { //nolint:gosec // a dust threshold fits
		return output.WithReason(output.FinalConflictError(cmd, name, fmt.Errorf(
			"wallet %s holds %s, more than its dust threshold %s: pay it out first (`cashctl pay <invoice>`), then delete it",
			name, output.FormatAmount(balance), output.FormatAmount(int64(w.DustThresholdMillis)))), reasonAboveDust) //nolint:gosec // fits
	}
	notice := fmt.Sprintf("Deleting wallet %s: the remaining %s goes to the circle's host, and the wallet is gone for good.", name, output.FormatAmount(balance))
	switch {
	case yes:
	case jsonMode:
		return output.WithReason(output.UsageError(cmd, fmt.Errorf(
			"%s Pass --yes to agree; nothing was sent", notice)), reasonDustNotAccepted)
	default:
		output.Notef(false, "%s", notice)
		if !Confirm(cmd, false, "Delete it?") {
			return output.WithReason(output.UsageError(cmd, errors.New("not deleted; nothing was sent")), reasonDustNotAccepted)
		}
	}

	// Written ahead: a lost reply is then recognised by the next attempt.
	w.DeleteSentAt = time.Now().Unix()
	value, err := w.Encode()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	c.Value = value
	if err := s.Save(); err != nil {
		return output.RuntimeError(cmd, fmt.Errorf("could not write the deletion down, so it was not sent: %w", err))
	}
	crashpoint.Hit("delete.after_writeahead")

	if err := client.DeleteWallet(ctx); err != nil {
		var walletErr *relayclient.WalletError
		if errors.As(err, &walletErr) {
			// Refused: nothing changed on the Hub; the mark goes.
			w.DeleteSentAt = 0
			if v, eErr := w.Encode(); eErr == nil {
				c.Value = v
				_ = s.Save()
			}
			ce := classifyWalletCallErr(cmd, err)
			if walletErr.Code == nipcw.ErrRestricted {
				if cli := output.AsCLIError(ce); cli != nil {
					cli.Hint = "a payment is in flight, or the balance rose above dust: pay out what is left and try again later"
				}
			}
			return ce
		}
		return deleteUnknownErr(cmd, name, w, fmt.Errorf("no answer to the deletion (%v): run `cashctl wallet delete %s` again", err, name))
	}
	crashpoint.Hit("delete.after_send")

	s.Remove(name)
	if err := s.Save(); err != nil {
		return output.RuntimeError(cmd, fmt.Errorf("the wallet was deleted on its Hub but could not be forgotten here: %w; `cashctl connect rm --force %s`", err, name))
	}
	if jsonMode {
		output.PrintJSON(map[string]any{"deleted": name, "wallet_pubkey": w.WalletPubkey, "dust_left_millis": balance})
		return nil
	}
	output.Printf("Deleted wallet %s.\n", name)
	return nil
}

// deleteUnknownErr is a deletion whose outcome is unknown: exit 8, the
// wallet kept here.
func deleteUnknownErr(cmd *cobra.Command, name string, w config.CircleWallet, err error) error {
	return output.WithReason(output.WithCall(output.IndeterminateError(cmd, name, err), map[string]any{
		"method": nipcw.MethodDeleteCircleWallet, "wallet": name, "wallet_pubkey": w.WalletPubkey,
		"sent_at": w.DeleteSentAt, "next_step": "`cashctl wallet delete " + name + "` again",
	}), reasonDeleteUnknown)
}
