package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ohstr/nmilat/nip19"
	"github.com/ohstr/nmilat/nip47"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/ohstr/nmilat/utils"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/identity"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

func newWalletCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "wallet",
		Short: "Manage your identity, wallets, and their history",
		RunE:  groupRunE,
	}
	cmd.AddCommand(
		newWalletInitCmd(),
		newWalletShowCmd(),
		newWalletHistoryCmd(),
		&cobra.Command{Use: "use <name>", Short: "Set the default wallet", Args: output.ExactArgs(1), RunE: runWalletUse},
		newWalletGetInfoCmd(), newWalletBalanceCmd(), newWalletBudgetCmd(),
		newWalletInvoiceCmd(), newWalletPayCmd(), newWalletListTxCmd(), newWalletSignMessageCmd(),
		newWalletProtectCmd(),
	)
	return cmd
}

// identityNpub returns the local identity's npub, regardless of source.
func identityNpub() (string, error) {
	s, err := identity.Load()
	if err != nil {
		return "", err
	}
	if s.Source == identity.SourceNcliVault {
		return s.Npub, nil
	}
	pubHex, err := utils.GetPublicKey(s.PrivHex)
	if err != nil {
		return "", err
	}
	return nip19.EncodePublicKey(pubHex)
}

func newWalletShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show your identity and wallet/ledger summary",
		Args:  output.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonMode, _ := cmd.Flags().GetBool("json")

			// Identity is OPTIONAL here, unlike everywhere else it's
			// loaded: a bearer-only wallet (never run `init`, and never
			// needs to — receive/transfer/redeem of a bearer-mode entry
			// all work without one) still holds real wallets/tokens worth
			// listing. Only a genuine identity.Load failure OTHER than
			// "none configured yet" is still a real error.
			var npub, source string
			var err error
			idStored, idErr := identity.Load()
			switch {
			case idErr == nil:
				npub, err = identityNpub()
				if err != nil {
					return output.RuntimeError(cmd, err)
				}
				source = string(idStored.Source)
				if idStored.Source == identity.SourceNcliVault {
					source = fmt.Sprintf("ncli-vault:%s", idStored.Label)
				}
			case errors.Is(idErr, identity.ErrNotConfigured):
				// npub/source stay "" — surfaced as such below.
			default:
				return output.RuntimeError(cmd, idErr)
			}

			s, err := config.Load()
			if err != nil {
				return output.RuntimeError(cmd, err)
			}
			l, err := ledger.Load()
			if err != nil {
				return output.RuntimeError(cmd, err)
			}

			if jsonMode {
				output.PrintJSON(map[string]any{
					"npub":            npub,
					"identity_source": source,
					"wallets":         output.NonNil(s.Connections),
					"default_wallet":  s.Default,
					"held_tokens":     output.NonNil(l.Held()),
				})
				return nil
			}

			if npub != "" {
				fmt.Printf("Identity: %s (%s)\n", npub, source)
			} else {
				fmt.Println("No local identity configured yet — bearer-mode holdings below still work fine without one. Run `cashctl init` if you need a pubkey-mode identity.")
			}
			fmt.Println()
			if s.IsEmpty() {
				fmt.Println("No wallets registered yet. Run `cashctl join <hub-connection>` or `cashctl connect add`.")
			} else {
				fmt.Println("Wallets:")
				for _, c := range s.Connections {
					marker := ""
					if c.Name == s.Default {
						marker = " [default]"
					}
					fmt.Printf("  %s%s\n", c.Name, marker)
				}
			}
			held := l.Held()
			if len(held) == 0 {
				fmt.Println("\nNo held cash tokens.")
			} else {
				fmt.Printf("\nHeld cash tokens: %d\n", len(held))
			}
			for i, e := range held {
				amount := "unknown amount"
				if e.AmountMillis != nil {
					amount = output.FormatAmount(int64(*e.AmountMillis))
				}
				status := "verified"
				if !e.Verified {
					status = "unverified"
				}
				// bearer-mode only (e.BearerProtection is "" — n/a — for a
				// pubkey-mode entry, see its own doc comment): "shared"
				// means the spending secret is still whatever was embedded
				// in the received token/gift string, spendable by anyone
				// else who was shown it too — the whole reason `receive`
				// offers to protect a bearer gift automatically, and the
				// one status here money can actually be at risk from, not
				// just informational the way verified/unverified is.
				switch e.BearerProtection {
				case ledger.BearerShared:
					status += ", bearer (shared — still spendable by anyone with the code; `cashctl wallet protect " + e.ID + "` fixes this)"
				case ledger.BearerProtected:
					status += ", bearer (protected)"
				}
				fmt.Printf("  %d) %s   received %s   %s\n", i+1, amount, formatReceivedDate(e.ReceivedAt), status)
			}
			return nil
		},
	}
}

func newWalletHistoryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "history",
		Short: "Show your local action history",
		Args:  output.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonMode, _ := cmd.Flags().GetBool("json")
			l, err := ledger.Load()
			if err != nil {
				return output.RuntimeError(cmd, err)
			}
			if jsonMode {
				output.PrintJSON(map[string]any{"history": output.NonNil(l.History)})
				return nil
			}
			if len(l.History) == 0 {
				fmt.Println("No local action history yet.")
				return nil
			}
			for _, h := range l.History {
				fmt.Printf("%s  %-12s %s\n", h.At, h.Action, h.Detail)
			}
			return nil
		},
	}
}

// dialForCommand resolves and dials the connection this command should
// act on: -c/--connection or the default wallet. Returns a *CLIError via
// output.NotFoundError when nothing is configured.
func dialForCommand(cmd *cobra.Command) (*nwcHandle, error) {
	value, ok, err := ResolveConnectionValue(cmd)
	if err != nil {
		return nil, output.RuntimeError(cmd, err)
	}
	if !ok {
		return nil, output.NotFoundError(cmd, "", errors.New(noWalletMessage()))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	client, err := DialGeneric(ctx, value)
	if err != nil {
		cancel()
		return nil, output.NetworkError(cmd, err)
	}
	return &nwcHandle{client: client, ctx: ctx, cancel: cancel}, nil
}

type nwcHandle struct {
	client *relayclient.NWCClient
	ctx    context.Context
	cancel context.CancelFunc
}

func (h *nwcHandle) Close() {
	h.cancel()
	h.client.Close()
}

func newWalletGetInfoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get-info",
		Short: "Show the connected wallet's capabilities",
		Args:  output.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonMode, _ := cmd.Flags().GetBool("json")
			value, ok, err := ResolveConnectionValue(cmd)
			if err != nil {
				return output.RuntimeError(cmd, err)
			}
			if !ok {
				return output.NotFoundError(cmd, "", errors.New(noWalletMessage()))
			}
			var info *nip47.GetInfoResult
			var dialErr bool
			err = WithSpinner(jsonMode, "Fetching info...", func() error {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				client, cErr := DialGeneric(ctx, value)
				if cErr != nil {
					dialErr = true
					return cErr
				}
				defer client.Close()
				i, cErr := client.GetInfo(ctx)
				if cErr != nil {
					return cErr
				}
				info = i
				return nil
			})
			if err != nil {
				if dialErr {
					return output.NetworkError(cmd, err)
				}
				return classifyNWCErr(cmd, err)
			}
			if jsonMode {
				output.PrintJSON(info)
				return nil
			}
			// Sanitized: get_info's response is whatever the dialed
			// wallet/Hub returns, not cashctl's own text.
			methods := make([]string, len(info.Methods))
			for i, m := range info.Methods {
				methods[i] = output.Sanitize(m)
			}
			fmt.Printf("alias:   %s\n", output.Sanitize(info.Alias))
			fmt.Printf("network: %s\n", output.Sanitize(info.Network))
			fmt.Printf("methods: %s\n", strings.Join(methods, ", "))
			// CircleWallet is only ever set when the dialed connection IS a
			// circle_hub's own connection (never a joined member's own
			// circle_wallet) — see nip47.GetInfoResult.CircleWallet's doc
			// comment. Surfacing it here is what lets `wallet get-info`
			// against a raw Circle Hub connection show its forwarding-fee
			// rate before joining, matching what `--json` already exposes.
			if cw := info.CircleWallet; cw != nil {
				fmt.Printf("circle policy:    %s\n", output.Sanitize(cw.CirclePolicy))
				fmt.Printf("circle fee:       %d ppm\n", cw.FeesPpm)
				fmt.Printf("circle available: %s\n", output.FormatAmount(cw.AvailableMloki))
			}
			return nil
		},
	}
}

func newWalletBalanceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "balance",
		Short: "Show your unified balance",
		Long:  `Sums every wallet's balance plus unredeemed held cash into one figure.`,
		Example: `  cashctl wallet balance
  cashctl wallet balance --breakdown
  cashctl wallet balance --from work`,
		Args: output.NoArgs,
		RunE: runWalletBalance,
	}
	cmd.Flags().BoolP("breakdown", "v", false, "show the itemized per-wallet/per-token detail")
	cmd.Flags().String("from", "", "show the balance of only this wallet or held token")
	return cmd
}
