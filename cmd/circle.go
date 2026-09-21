package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ohstr/nmilat/nip47"
	"github.com/ohstr/nmilat/nipcw"
	nipcwclient "github.com/ohstr/nmilat/nipcw/client"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/credential"
	"github.com/ohstr/cashctl/internal/dial"
	"github.com/ohstr/cashctl/internal/output"
)

// isIdentityEventReplayErr reports whether err is the Hub's own replay
// guard on `join`'s identity proof — nmilat's nipcw/identity.go builds
// that proof with created_at at whole-second granularity and no nonce, so
// two join attempts (by the same identity, at the same hub) landing in
// the same wall-clock second produce byte-identical proofs, and the Hub
// rejects the second as a replay: BAD_REQUEST, "identity_event has
// already been used" — indistinguishable by NWC code alone from any other
// BAD_REQUEST (an over-cap max-amount, a malformed budget-renewal, ...),
// so this matches on the Hub's own specific message text instead.
// Confirmed live: neither call actually gets processed when this fires,
// so retrying once the clock has ticked over is always safe — never a
// double-join — and recovers the request's real outcome (an allowlist/cap
// decline, or success) instead of this transport-level collision masking
// it as a hard, non-retryable failure.
func isIdentityEventReplayErr(err error) bool {
	var walletErr *relayclient.WalletError
	return errors.As(err, &walletErr) && walletErr.Code == "BAD_REQUEST" &&
		strings.Contains(walletErr.Message, "identity_event") && strings.Contains(walletErr.Message, "already been used")
}

func newCircleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "circle",
		Short: "Join a circle to get a personal Lightning wallet",
		RunE:  groupRunE,
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
//
// When NEITHER side parses as an amount (a malformed one, most often),
// looksLikeHubArg breaks the tie: exactly one side being hub-shaped means
// the OTHER side is the (malformed) amount — confirmed live:
// `join 0.0001 <hub>` used to blame the 232-char hub connection string as
// "not a valid amount" and echo the whole thing (secret included) back in
// the error, because the old position-only fallback never checked
// whether the OTHER argument looked like a hub either. Genuinely
// ambiguous input (both or neither look hub-shaped) falls back to the
// documented hub-first order.
func disambiguateJoinArgs(args []string) (positionalHub, positionalMaxAmount string) {
	looksLikeAmount := func(s string) bool {
		_, err := output.ParseAmount(s)
		return err == nil
	}
	looksLikeHubArg := func(s string) bool {
		switch dial.Sniff(s) {
		case dial.KindCircleHub, dial.KindCashHub, dial.KindNWCURI:
			return true
		default:
			return false
		}
	}
	switch len(args) {
	case 1:
		if looksLikeAmount(args[0]) {
			positionalMaxAmount = args[0]
		} else {
			positionalHub = args[0]
		}
	case 2:
		amount0, amount1 := looksLikeAmount(args[0]), looksLikeAmount(args[1])
		hub0, hub1 := looksLikeHubArg(args[0]), looksLikeHubArg(args[1])
		switch {
		case amount0 && !amount1:
			positionalMaxAmount, positionalHub = args[0], args[1]
		case amount1 && !amount0:
			positionalHub, positionalMaxAmount = args[0], args[1]
		case hub0 && !hub1:
			positionalHub, positionalMaxAmount = args[0], args[1]
		case hub1 && !hub0:
			positionalHub, positionalMaxAmount = args[1], args[0]
		default:
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
	attemptJoin := func() error {
		return WithSpinner(jsonMode, "Joining...", func() error {
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
	}
	err = attemptJoin()
	if err != nil && !dialErr && isIdentityEventReplayErr(err) {
		// See isIdentityEventReplayErr's own doc comment: nothing was
		// actually submitted, so retrying once the clock has ticked over
		// is safe and (confirmed live) recovers the real outcome — 1.1s
		// guarantees crossing at least one whole-second boundary from any
		// starting sub-second offset, matching the proof's own
		// whole-second granularity.
		output.Notef(jsonMode, "Retrying — the Hub saw a duplicate identity proof from within the same second...")
		time.Sleep(1100 * time.Millisecond)
		dialErr = false
		err = attemptJoin()
	}
	if err != nil {
		if dialErr {
			return output.NetworkError(cmd, err)
		}
		if isIdentityEventReplayErr(err) {
			// Still colliding even after the retry (another process
			// landed on the same fresh second, most likely) — conflict,
			// not invalid_input: nothing about the request itself was
			// wrong, and a further retry is exactly the right move.
			return output.ConflictError(cmd, "", errors.New(
				"the Hub's identity-proof replay guard rejected two attempts within the same second, even after retrying once — wait a few seconds and run `join` again"))
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
		// Same rule as `connect add`: --yes never silently replaces an
		// existing default.
		yes, _ := cmd.Flags().GetBool("yes")
		setDefault = !jsonMode && !yes && Confirm(cmd, false, "Set as your default?")
	}
	if setDefault {
		_ = s.SetDefault(name)
	}
	if err := s.Save(); err != nil {
		return output.RuntimeError(cmd, err)
	}

	if jsonMode {
		// Built explicitly rather than embedding resp directly (same fix
		// as cash_inspect.go's own decode command, and printAndSaveTransferResult's
		// own CashTransferResult handling): nipcw.CreateCircleWalletResponse
		// has no JSON tags of its own, so a raw marshal would leak
		// CamelCase Go field names AND, worse, resp.PairingURI itself —
		// the new wallet's actual NWC secret, already saved locally under
		// name and never needed again from output alone.
		var expiresAt any
		if resp.ExpiresAt > 0 {
			expiresAt = time.Unix(resp.ExpiresAt, 0).UTC().Format(time.RFC3339)
		}
		output.PrintJSON(map[string]any{
			"wallet": name, "default": setDefault,
			"response": map[string]any{
				"wallet_pubkey":  resp.WalletPubkey,
				"expires_at":     expiresAt,
				"fees_ppm":       resp.FeesPpm,
				"budget_renewal": resp.BudgetRenewal,
			},
		})
		return nil
	}
	greeting := "Joined!"
	if label != "" {
		// label came out of the hub connection string itself — chosen by
		// whoever set the Hub up, outside cashctl's own control.
		greeting = fmt.Sprintf("Joined %q!", output.Sanitize(label))
	}
	renewal := resp.BudgetRenewal
	if renewal == "" {
		renewal = "never"
	}
	fmt.Printf("%s New wallet: %s (max %s/%s)\n", greeting, name, output.FormatAmount(int64(maxAmount)), output.Sanitize(renewal))
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
