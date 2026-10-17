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

// walletExistsFromLegacyRefusal reports a pre-SD-20 Hub's "already has a wallet"
// refusal, sent as RESTRICTED with a specific message, as the WALLET_EXISTS a
// current Hub sends, so a caller branches on one nwc_code for it either way.
func walletExistsFromLegacyRefusal(err error) error {
	var walletErr *relayclient.WalletError
	if errors.As(err, &walletErr) && walletErr.Code == nip47.ErrRestricted &&
		strings.Contains(walletErr.Message, "already has an active circle wallet") {
		normalized := *walletErr
		normalized.Code = nipcw.ErrWalletExists
		return &normalized
	}
	return err
}

// withHeldWalletHint adds to a WALLET_EXISTS refusal which wallet here is
// that circle's, so "you already hold one" comes with the name to use it by.
// The Hub only says one exists; the local name is cashctl's to supply.
func withHeldWalletHint(err error, label string) error {
	ce, ok := err.(*output.CLIError)
	if !ok || ce.NWCCode != nipcw.ErrWalletExists {
		return err
	}
	s, loadErr := config.Load()
	if loadErr != nil {
		return err
	}
	ce.Hint = heldWalletHint(s, label)
	return ce
}

func heldWalletHint(s *config.Store, label string) string {
	if name := config.HintedName("circle", label); name != "" {
		if _, ok := s.Find(name); ok {
			return fmt.Sprintf("It is registered here as %q — use it with `-c %s` or `cashctl wallet use %s`.", name, name, name)
		}
	}
	var circles []string
	for _, c := range s.Connections {
		if strings.HasPrefix(c.Name, "circle:") {
			circles = append(circles, c.Name)
		}
	}
	if len(circles) > 0 {
		return fmt.Sprintf("Your circle wallets here: %s — use one with `-c <name>`.", strings.Join(circles, ", "))
	}
	return "It is not registered on this device (joined from another one, or removed); `cashctl connect list` shows what is."
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
	asFlag := asCredentialValue(cmd)

	positionalHub, positionalMaxAmount := disambiguateJoinArgs(args)

	hub, err := resolvePositionalOrFlag(cmd, positionalHub, "hub", hubFlag)
	if err != nil {
		return err
	}
	// Checked explicitly (not cobra's own MarkFlagRequired) so a missing
	// hub is a classified UsageError — see cash_consolidate.go's own
	// comment on --sources for why.
	// Both arguments named whenever the max amount is missing too: a hint
	// naming only the hub led straight to the next refusal (I-29).
	if hub == "" {
		if positionalMaxAmount == "" && strings.TrimSpace(maxAmountFlag) == "" {
			return output.InvocationError(cmd, fmt.Errorf("a Circle Hub connection and a max amount are required — pass both (cashctl join <hub-connection> <max-amount>) or use --hub and --max-amount"))
		}
		return output.InvocationError(cmd, fmt.Errorf("a Circle Hub connection is required — pass it directly (cashctl join <hub-connection> <max-amount>) or via --hub"))
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
		// Absent and zero are different mistakes and used to share one
		// message, which told a user who had just typed `0` that they were
		// missing an argument. Someone passing 0 has almost certainly
		// assumed the "0 means unlimited" convention this method does not
		// have, so say that instead.
		if positionalMaxAmount != "" || maxAmountFlag != "" {
			return output.InvalidInputError(cmd, "0", fmt.Errorf("0 isn't a spend cap — there's no \"0 means unlimited\" convention here, so pass the most this wallet may spend"))
		}
		return output.InvocationError(cmd, fmt.Errorf("a max amount is required — pass it directly (cashctl join <hub-connection> <max-amount>) or via --max-amount"))
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

	// NIP-CW §Pairing Connection: the member generates the wallet's connection
	// key and only its pubkey is sent. The secret is saved before the request
	// goes out, so no outcome of the call can leave it only in memory.
	connKey, err := nipcw.NewConnectionKey()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	hubInfo, err := nip47.ParsePairingURI(pairingURI)
	if err != nil {
		return output.InvalidInputError(cmd, hub, err)
	}
	if err := config.SavePendingJoin(config.PendingJoin{
		ConnectionPubkey: connKey.Pubkey(), ConnectionSecret: connKey.Secret(),
		HubPubkey: hubInfo.WalletPubkey, HubLabel: label,
		At: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return output.RuntimeError(cmd, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var dialErr bool
	var resp *nipcw.CreateCircleWalletResponse
	attemptJoin := func() error {
		return WithSpinner(jsonMode, "Joining...", func() error {
			r, dialed, cErr := joinCircleFn(ctx, pairingURI, nipcw.CreateCircleWalletParams{
				Credential: cred, Connection: connKey,
				MaxAmountMillis: maxAmount, Expiry: expiry, BudgetRenewal: budgetRenewal,
			})
			if cErr != nil {
				dialErr = !dialed
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
		output.Notef(jsonMode, "Another join from this identity reached the Hub within the same second, so it could not tell them apart; asking again in a second for this join's own answer...")
		time.Sleep(1100 * time.Millisecond)
		dialErr = false
		err = attemptJoin()
	}
	if err != nil {
		// A refusal, or a request that never left, created nothing: the key is
		// forgotten. Anything else may have created the wallet, so it is kept.
		if dialErr || joinWasRefused(err) {
			if dErr := config.DeletePendingJoin(connKey.Pubkey()); dErr != nil {
				output.Notef(jsonMode, "Could not forget the unused connection key: %v", dErr)
			}
		} else {
			err = fmt.Errorf("%w — whether the wallet was created is unknown. If a later join says you already hold one, "+
				"it is the wallet with connection key %s (listed under pending joins in `cashctl wallet show`): "+
				"the Hub sends its details only once, so ask the circle's host to delete it, then join again", err, connKey.Pubkey())
		}
		if dialErr {
			return output.NetworkError(cmd, err)
		}
		if isUnusableJoinReply(err) {
			// The Hub did answer, so asking again only meets the wallet it made.
			return output.RuntimeError(cmd, fmt.Errorf("the Hub's reply has no usable wallet details; it may run a NIP-CW version from before member-generated connection keys: %w", err))
		}
		if isIdentityEventReplayErr(err) {
			// Still colliding even after the retry (another process
			// landed on the same fresh second, most likely) — conflict,
			// not invalid_input: nothing about the request itself was
			// wrong, and a further retry is exactly the right move.
			return output.ConflictError(cmd, "", errors.New(
				"the Hub's identity-proof replay guard rejected two attempts within the same second, even after retrying once — wait a few seconds and run `join` again"))
		}
		return withHeldWalletHint(classifyNWCErr(cmd, walletExistsFromLegacyRefusal(err)), label)
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
	s.SetKind(name, config.KindCircle)
	s.AnsweredJoin(connKey.Pubkey())

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
		// The wallet exists on the Hub and this is the only copy of its
		// connection string: the Hub cannot send it again.
		return output.RecoveryError(cmd, err, fmt.Sprintf(
			"Your new wallet was created but could not be saved. Register it with `cashctl connect add %s <connection>`, using: %s", name, resp.PairingURI))
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
				"wallet_pubkey":     resp.WalletPubkey,
				"relays":            resp.Relays,
				"connection_pubkey": connKey.Pubkey(),
				"expires_at":        expiresAt,
				// The cap as the Hub resolved it, which may differ from the one asked
				// for (NIP-CW §Response); null when the Hub did not say.
				"max_amount_millis": resp.MaxAmountMillis,
				"max_amount":        maxAmountText(resp.MaxAmountMillis),
				"fees_ppm":          resp.FeesPpm,
				"budget_renewal":    resp.BudgetRenewal,
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
	output.Printf("%s New wallet: %s (%s)\n", greeting, name, joinCapSummary(maxAmount, resp, renewal))
	if setDefault {
		output.Printf("Default wallet set to %s.\n", name)
	} else if !wasEmpty {
		output.Printf("Saved as %s. Switch anytime with `cashctl wallet use %s`.\n", name, name)
	}
	return nil
}

// joinCircle dials the Circle Hub and places one create_circle_wallet call.
// dialed is false when the request never left: the dial itself failed.
func joinCircle(ctx context.Context, hubURI string, params nipcw.CreateCircleWalletParams) (resp *nipcw.CreateCircleWalletResponse, dialed bool, err error) {
	client, err := nipcwclient.Connect(ctx, hubURI)
	if err != nil {
		return nil, false, err
	}
	defer client.Close()
	resp, err = client.CreateCircleWallet(ctx, params)
	return resp, true, err
}

// joinCircleFn is joinCircle, swapped out in tests.
var joinCircleFn = joinCircle

// isUnusableJoinReply reports a reply the Hub did send whose wallet details
// cashctl cannot build a connection from (no wallet_pubkey or relays).
func isUnusableJoinReply(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "nipcw: wallet details carry no") || strings.Contains(msg, "nipcw: parse wallet details")
}

// joinWasRefused reports an explicit answer from the Hub, after which no
// wallet exists for this request: a decline, or the SDK refusing to send it.
func joinWasRefused(err error) bool {
	var walletErr *relayclient.WalletError
	return errors.As(err, &walletErr) || errors.Is(err, nipcw.ErrNoConnectionKey)
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

// joinCapSummary describes the wallet's terms as the Hub resolved them: its cap
// (saying so when it differs from the one asked for), renewal, in-circle fee
// and expiry.
func joinCapSummary(requested uint64, resp *nipcw.CreateCircleWalletResponse, renewal string) string {
	capMillis := requested
	note := ""
	if resp.MaxAmountMillis != nil {
		capMillis = *resp.MaxAmountMillis
		if capMillis != requested {
			note = fmt.Sprintf(", you asked for %s", output.FormatAmount(int64(requested)))
		}
	}
	summary := fmt.Sprintf("max %s/%s%s", output.FormatAmount(int64(capMillis)), output.Sanitize(renewal), note)
	if resp.FeesPpm > 0 {
		summary += fmt.Sprintf("; in-circle fee %d ppm", resp.FeesPpm)
	}
	if resp.ExpiresAt > 0 {
		summary += "; expires " + time.Unix(resp.ExpiresAt, 0).UTC().Format("2006-01-02 15:04 UTC")
	}
	return summary
}

// maxAmountText is the granted cap in the unit `join` takes it in, next to
// max_amount_millis, so a caller can check it without converting; "" when the
// Hub did not say.
func maxAmountText(millis *uint64) string {
	if millis == nil {
		return ""
	}
	return output.FormatAmount(int64(*millis))
}
