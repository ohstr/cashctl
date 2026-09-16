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
	cmd.AddCommand(newCircleCreateCmd())
	return cmd
}

func newCircleCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create [hub-connection]",
		Short: "Join a circle (self-service create_circle_wallet)",
		Args:  output.MaximumNArgs(1),
		RunE:  runCircleCreate,
	}
	cmd.Flags().String("hub", "", "the Circle Hub connection: a circlehub1... string (recommended), or a raw NWC URI — same as the positional argument, kept for scripted/agentic use")
	cmd.Flags().Uint64("max-amount", 0, fmt.Sprintf("requested spend cap (%s)", output.CurrencyUnit))
	cmd.Flags().Duration("expiry", 0, "requested expiry duration (0 = Hub default)")
	cmd.Flags().String("budget-renewal", "", "daily|weekly|monthly|yearly|never (default: Hub default)")
	cmd.Flags().String("as", "", "override credential (defaults to your local identity)")
	return cmd
}

func runCircleCreate(cmd *cobra.Command, args []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	hubFlag, _ := cmd.Flags().GetString("hub")
	maxAmount, _ := cmd.Flags().GetUint64("max-amount")
	expiry, _ := cmd.Flags().GetDuration("expiry")
	budgetRenewal, _ := cmd.Flags().GetString("budget-renewal")
	asFlag, _ := cmd.Flags().GetString("as")

	var positionalHub string
	if len(args) > 0 {
		positionalHub = args[0]
	}
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
	client, err := nipcwclient.Connect(ctx, pairingURI)
	if err != nil {
		return output.NetworkError(cmd, err)
	}
	defer client.Close()

	resp, err := client.CreateCircleWallet(ctx, nipcw.CreateCircleWalletParams{
		Credential: cred, MaxAmountMillis: maxAmount, Expiry: expiry, BudgetRenewal: budgetRenewal,
	})
	if err != nil {
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
	fmt.Printf("%s New wallet: %s (max %d %s/%s)\n", greeting, name, maxAmount, output.CurrencyUnit, renewal)
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
