package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/ohstr/nmilat/nip47"
	"github.com/ohstr/nmilat/nipcw"
	nipcwclient "github.com/ohstr/nmilat/nipcw/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/credential"
	"github.com/ohstr/cashctl/internal/dial"
	"github.com/ohstr/cashctl/internal/output"
)

func newCircleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "circle",
		Short: "Join a circle to get a personal Lightning wallet",
	}
	cmd.AddCommand(newCircleJoinCmd())
	return cmd
}

func newCircleJoinCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "join [hub-connection] [max-amount]",
		Short: "Join a circle",
		Example: `  cashctl join circlehub1... 100
  cashctl join circlehub1... --max-amount 100`,
		Args: output.MaximumNArgs(2),
		RunE: runCircleJoin,
	}
	cmd.Flags().String("hub", "", "the Circle Hub connection: a circlehub1... string (recommended), or a raw NWC URI")
	cmd.Flags().String("max-amount", "", fmt.Sprintf("requested spend cap, in %s — required", output.CurrencyUnit))
	cmd.Flags().Duration("expiry", 0, "requested expiry duration (0 = Hub default)")
	cmd.Flags().String("budget-renewal", "", "daily|weekly|monthly|yearly|never (default: Hub default)")
	cmd.Flags().String("as", "", "override credential (defaults to your local identity)")
	return cmd
}

// disambiguateJoinArgs decides which of join's up-to-2 positional args is
// the hub connection and which is the max-amount — sniffed by shape, the
// same way disambiguateTransferArgs picks apart transfer's target/amount
// pair (cash_transfer.go): a circlehub1.../NWC URI never parses as a loki
// amount, so whichever arg does is the max-amount, regardless of which
// side it's on.
func disambiguateJoinArgs(args []string) (positionalHub, positionalMaxAmount string) {
	switch len(args) {
	case 1:
		if _, numErr := output.ParseAmount(args[0]); numErr == nil {
			positionalMaxAmount = args[0]
		} else {
			positionalHub = args[0]
		}
	case 2:
		if _, numErr := output.ParseAmount(args[0]); numErr == nil {
			positionalMaxAmount, positionalHub = args[0], args[1]
		} else {
			positionalHub, positionalMaxAmount = args[0], args[1]
		}
	}
	return positionalHub, positionalMaxAmount
}

func runCircleJoin(cmd *cobra.Command, args []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	hubFlag, _ := cmd.Flags().GetString("hub")
	maxAmountFlag, _ := cmd.Flags().GetString("max-amount")
	expiry, _ := cmd.Flags().GetDuration("expiry")
	budgetRenewal, _ := cmd.Flags().GetString("budget-renewal")
	asFlag, _ := cmd.Flags().GetString("as")

	positionalHub, positionalMaxAmount := disambiguateJoinArgs(args)

	hub, err := resolvePositionalOrFlag(cmd, positionalHub, "hub", hubFlag)
	if err != nil {
		return err
	}
	// Checked explicitly (not cobra's own MarkFlagRequired) so a missing
	// hub is a classified UsageError — see cash_consolidate.go's own
	// comment on --sources for why.
	if hub == "" {
		return output.UsageError(cmd, fmt.Errorf("a Circle Hub connection is required — pass it directly (cashctl join <hub-connection>) or via --hub"))
	}
	maxAmount, err := resolvePositionalOrFlagAmount(cmd, positionalMaxAmount, "max-amount", maxAmountFlag)
	if err != nil {
		return err
	}
	// NIP-CW requires max_amount on every create_circle_wallet request —
	// there's no "0 means unlimited"/"use the Hub's default" wire
	// convention for this method (confirmed against lokihub's own
	// create_circle_wallet_controller.go: anything under 1000 mloki, which
	// includes the omitted/zero case, is hard-rejected). Checked here so a
	// missing cap fails fast, locally, instead of round-tripping to the
	// Hub for the same rejection.
	if maxAmount == 0 {
		return output.UsageError(cmd, fmt.Errorf("a max amount is required — pass it directly (cashctl join <hub-connection> <amount>) or via --max-amount"))
	}

	pairingURI, label, err := resolveHubConnection(cmd, hub)
	if err != nil {
		return err
	}

	var cred nipcw.Credential
	if asFlag != "" {
		cred, err = credential.ParseCircle(asFlag)
		if err != nil {
			return output.InvalidInputError(cmd, output.RedactSecretInput(asFlag), err)
		}
	} else {
		cred, err = localCircleCredential(cmd)
		if err != nil {
			return output.RuntimeError(cmd, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var dialErr bool
	var resp *nipcw.CreateCircleWalletResponse
	err = WithSpinner(jsonMode, "Joining...", func() error {
		client, cErr := nipcwclient.Connect(ctx, pairingURI)
		if cErr != nil {
			dialErr = true
			return cErr
		}
		defer client.Close()
		r, cErr := client.CreateCircleWallet(ctx, nipcw.CreateCircleWalletParams{
			Credential: cred, MaxAmountMillis: maxAmount, Expiry: expiry, BudgetRenewal: budgetRenewal,
		})
		if cErr != nil {
			return cErr
		}
		resp = r
		return nil
	})
	if err != nil {
		if dialErr {
			return output.NetworkError(cmd, err)
		}
		return classifyNWCErr(cmd, err)
	}

	s, err := config.Load()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	wasEmpty := s.IsEmpty()
	name := s.SuggestName("circle", label)
	if err := s.Add(name, resp.PairingURI); err != nil {
		return output.RuntimeError(cmd, err)
	}

	setDefault := false
	if wasEmpty {
		setDefault = jsonMode || Confirm(cmd, true, "This is your first wallet — use it as your default?")
	} else {
		setDefault = !jsonMode && Confirm(cmd, false, "Set as your default?")
	}
	if setDefault {
		_ = s.SetDefault(name)
	}
	if err := s.Save(); err != nil {
		return output.RuntimeError(cmd, err)
	}

	if jsonMode {
		output.PrintJSON(map[string]any{"wallet": name, "default": setDefault, "response": resp})
		return nil
	}
	greeting := "Joined!"
	if label != "" {
		greeting = fmt.Sprintf("Joined %q!", label)
	}
	renewal := resp.BudgetRenewal
	if renewal == "" {
		renewal = "never"
	}
	fmt.Printf("%s New wallet: %s (max %s/%s)\n", greeting, name, output.FormatAmount(int64(maxAmount)), renewal)
	if setDefault {
		fmt.Printf("Default wallet set to %s.\n", name)
	} else if !wasEmpty {
		fmt.Printf("Saved as %s. Switch anytime with `cashctl wallet use %s`.\n", name, name)
	}
	return nil
}

// resolveHubConnection sniffs --hub and returns a plain NWC pairing URI to
// dial plus a display label. A circlehub1... string decodes locally (no
// network call) — nipcw/client.Connect itself doesn't understand this
// format (Circle Wallet pairing data was never wrapped in bech32), so it's
// rebuilt into an ordinary pairing URI before being handed to it. A raw
// NWC URI is used as-is, for Hubs that haven't adopted the new format yet.
func resolveHubConnection(cmd *cobra.Command, hub string) (pairingURI, label string, err error) {
	switch dial.Sniff(hub) {
	case dial.KindCashHub:
		return "", "", output.InvalidInputError(cmd, hub, fmt.Errorf(
			"that's a Cash Hub connection (for minting cash), not a Circle Hub connection. cashctl can't mint — this needs the Hub operator's own tooling"))
	case dial.KindCircleHub:
		conn, err := nipcw.DecodeCircleHubConnection(hub)
		if err != nil {
			return "", "", output.InvalidInputError(cmd, hub, err)
		}
		uri := nip47.BuildPairingURI(conn.WalletPubkey, conn.RelayURLs, conn.Secret, nil)
		return uri, conn.Label, nil
	case dial.KindNWCURI:
		return hub, "", nil
	default:
		return "", "", output.InvalidInputError(cmd, hub, fmt.Errorf("not a valid Circle Hub connection"))
	}
}
