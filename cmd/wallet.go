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

// cashModeLabel renders which of the three cash modes e is held in —
// "cash"/"identity cash"/"web identity cash", the same vocabulary
// cash_transfer's own confirm/result messages use — plus cash mode's
// own shared/protected sub-state (see Entry.CashProtection's own doc
// comment): "shared" means the spending secret is still whatever was
// embedded in the received token/gift string, spendable by anyone else
// who was shown it too — the whole reason `receive` offers to protect a
// cash gift automatically, and the one status here money can actually
// be at risk from, not just informational the way verified/unverified
// is. Identity cash and web identity cash have no such sub-state: once
// reassigned to a specific pubkey/connection identity, only that
// identity's own key can ever spend it.
func cashModeLabel(e ledger.Entry) string {
	switch e.CashProtection {
	case ledger.CashShared:
		return "cash (shared — anyone with this can spend it; `cashctl wallet protect " + e.ID + "` fixes this)"
	case ledger.CashProtected:
		return "cash (protected)"
	}
	if e.IdentityRequired != nil && !*e.IdentityRequired {
		// Cash-mode with no CashProtection recorded (data saved before
		// that field existed) — still cash, just without a shared/protected
		// read to report.
		return "cash"
	}
	if e.ConnectionKeyPlatform != "" {
		return "web identity cash"
	}
	return "identity cash"
}

func newWalletShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show your identity and wallet/ledger summary",
		Long: `Shows your identity, registered wallets and held cash tokens. No network
call: amounts are as last known.

--json adds: held_tokens (each with its id and cash_hub — only same-Hub
tokens merge), unclaimed_handoffs (cash notes still unclaimed and gifts,
which stay "sent": branch on status), open_calls (a transfer, merge or
re-key with no answer yet, with each new code it generated — keep them),
pending_joins and pending_merge.`,
		Args: output.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonMode, _ := cmd.Flags().GetBool("json")

			// Identity is OPTIONAL here, unlike everywhere else it's
			// loaded: a cash-mode-only wallet (never run `init`, and never
			// needs to — receive/transfer/redeem of a cash-mode entry
			// all work without one) still holds real wallets/tokens worth
			// listing. Only a genuine identity.Load failure OTHER than
			// "none configured yet" is still a real error.
			npub, source, err := currentIdentity()
			if err != nil {
				return output.RuntimeError(cmd, err)
			}

			s, err := config.Load()
			if err != nil {
				return output.RuntimeError(cmd, err)
			}
			l, err := ledger.Load()
			if err != nil {
				return output.RuntimeError(cmd, err)
			}
			pendingJoins, err := config.PendingJoins()
			if err != nil {
				return output.RuntimeError(cmd, err)
			}
			device, _ := deviceIDFn()
			var recovered []string
			if ws, err := ledger.Withdrawals(); err == nil {
				for _, w := range ws {
					if w.Recovered && w.FinishedAt == 0 {
						recovered = append(recovered, w.WithdrawalID)
					}
				}
			}

			if jsonMode {
				output.PrintJSON(map[string]any{
					"npub":            npub,
					"identity_source": source,
					"wallets":         output.NonNil(s.Connections),
					"default_wallet":  s.Default,
					"held_tokens":     heldTokensJSON(l.Held()),
					// Calls with no answer yet, with every secret they generated:
					// each may be the only copy of what spends a bill.
					"open_calls": output.NonNil(openCalls(l, device)),
					// Handed out and not known to be claimed, secrets hidden: `wallet
					// history --show-secrets` shows them in full to resend.
					"unclaimed_handoffs": unclaimedHandoffs(l),
					"pending_merge":      pendingMergeJSON(l),
					// Joins not finished (open, or kept to show, such as an expired
					// one): the wallet may exist, known to the Hub by
					// connection_pubkey. The secret is never shown.
					"pending_joins": pendingJoins,
					// Withdrawals found through the Hub that this device has no
					// record of.
					"recovered_withdrawals": output.NonNil(recovered),
				})
				return nil
			}

			if npub != "" {
				output.Printf("Identity: %s (%s)\n", npub, source)
			} else {
				output.Println("No local identity configured yet — cash holdings below still work fine without one. Run `cashctl init` if you need a pubkey-mode identity.")
			}
			output.Println()
			if s.IsEmpty() {
				output.Println("No wallets registered yet. Run `cashctl join <hub-connection>` or `cashctl connect add`.")
			} else {
				output.Println("Wallets:")
				for _, c := range s.Connections {
					marker := ""
					if c.Name == s.Default {
						marker = " [default]"
					}
					output.Printf("  %s (%s)%s%s\n", c.Name, c.Kind, walletPubkeySuffix(c), marker)
				}
			}
			held := l.Held()
			if len(held) == 0 {
				output.Println("\nNo held cash tokens.")
			} else if len(held) == 1 {
				output.Printf("\nHeld cash tokens: %d\n", len(held))
			} else {
				output.Printf("\nHeld cash tokens: %d (tokens on the same hub can be merged with `cashctl consolidate`)\n", len(held))
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
				status += ", " + cashModeLabel(e)
				if hub := ledger.HubGroupKey(e); hub != "" {
					status += ", hub " + shortHub(hub)
				}
				output.Printf("  %d) %s   received %s   %s\n", i+1, amount, formatReceivedDate(e.ReceivedAt), status)
			}
			printPendingMerge(l)
			if pending := unclaimedHandoffs(l); len(pending) > 0 {
				output.Printf("\nHanded out, not yet claimed: %d — `cashctl wallet history --show-secrets` to resend\n", len(pending))
			}
			if len(pendingJoins) > 0 {
				output.Printf("\nJoins not finished: %d — `cashctl cash settle` asks the Hub about open ones\n", len(pendingJoins))
				for _, j := range pendingJoins {
					hub := output.Sanitize(j.HubLabel)
					if hub == "" {
						hub = "Hub " + shortFingerprint(j.HubPubkey)
					}
					if !j.ConnVerified {
						hub += " (Hub not confirmed)"
					}
					state := j.State
					if j.Detail != "" {
						state += ": " + output.Sanitize(j.Detail)
					}
					output.Printf("  %s   connection key %s   %s   %s\n", hub, j.ConnectionPubkey, state, time.Unix(j.CreatedAt, 0).UTC().Format(time.RFC3339))
				}
			}
			for _, id := range recovered {
				output.Printf("\nFound a withdrawal this device has no record of: %s — `cashctl cash withdraw show %s`\n", id, id)
			}
			// Last, and only when there is something to say: calls whose answer
			// never came, with the secrets they generated — the one place those
			// are readable again.
			printOpenCalls(l, device)
			return nil
		},
	}
}

func newWalletHistoryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "history",
		Short: "Show your local action history and everything you handed out",
		Long: `Shows your local action log, then every cash note and gift you handed out,
kept permanently so a lost one can be resent. An unclaimed cash note's status
is re-checked with its Hub; its secret, which still spends it, is hidden
unless --show-secrets.`,
		Args: output.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonMode, _ := cmd.Flags().GetBool("json")
			showSecrets, _ := cmd.Flags().GetBool("show-secrets")
			l, err := ledger.Load()
			if err != nil {
				return output.RuntimeError(cmd, err)
			}
			_ = WithSpinner(jsonMode, "Checking sent notes...", func() error {
				refreshHandoffs(l)
				return nil
			})
			if jsonMode {
				handoffs := make([]map[string]any, 0, len(l.Handoffs))
				for _, h := range l.Handoffs {
					handoffs = append(handoffs, handoffJSON(h, showSecrets))
				}
				output.PrintJSON(map[string]any{"history": output.NonNil(l.History), "handoffs": handoffs})
				return nil
			}
			if len(l.History) == 0 && len(l.Handoffs) == 0 {
				output.Println("No local action history yet.")
				return nil
			}
			for _, h := range l.History {
				output.Printf("%s  %-12s %s\n", h.At, h.Action, h.Detail)
			}
			if len(l.Handoffs) > 0 {
				output.Println("\nHanded out:")
				for _, h := range l.Handoffs {
					shown, _ := maskedHandoff(h, showSecrets)
					output.Printf("  %s  %s to %s  [%s]\n    %s\n", h.At, output.FormatAmount(int64(h.AmountMillis)), output.Sanitize(h.Target), h.Status, shown)
				}
			}
			return nil
		},
	}
	cmd.Flags().Bool("show-secrets", false, "show unclaimed cash notes in full (their secret still spends them)")
	return cmd
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
			output.Printf("alias:   %s\n", output.Sanitize(info.Alias))
			output.Printf("network: %s\n", output.Sanitize(info.Network))
			output.Printf("methods: %s\n", strings.Join(methods, ", "))
			// CircleWallet is only ever set when the dialed connection IS a
			// circle_hub's own connection (never a joined member's own
			// circle_wallet) — see nip47.GetInfoResult.CircleWallet's doc
			// comment. Surfacing it here is what lets `wallet get-info`
			// against a raw Circle Hub connection show its forwarding-fee
			// rate before joining, matching what `--json` already exposes.
			if cw := info.CircleWallet; cw != nil {
				output.Printf("circle policy:    %s\n", circlePolicyPhrase(cw.CirclePolicy))
				output.Printf("private join:     %s\n", offeredText(cw.Identity != ""))
				output.Printf("circle fee:       %d ppm\n", cw.FeesPpm)
				output.Printf("circle available: %s\n", output.FormatAmount(cw.AvailableMillis))
			}
			return nil
		},
	}
}

func newWalletBalanceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "balance",
		Aliases: []string{"get-balance"},
		Short:   "Show your unified balance",
		Long:    `Sums every wallet's balance plus unredeemed held cash into one figure.`,
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

// heldToken is a held entry plus cash_hub, the fingerprint of the Cash Hub
// that issued it: only tokens with the same cash_hub can merge.
type heldToken struct {
	ledger.Entry
	CashHub string `json:"cash_hub"`
}

func heldTokensJSON(held []ledger.Entry) []heldToken {
	out := make([]heldToken, len(held))
	for i, e := range held {
		out[i] = heldToken{Entry: e, CashHub: ledger.HubGroupKey(e)}
	}
	return out
}

// currentIdentity is the configured identity's npub and source label, or two
// empty strings when none is configured yet (not an error: "none configured
// yet" is something `wallet show` and `init` report, not fail on).
func currentIdentity() (npub, source string, err error) {
	idStored, idErr := identity.Load()
	switch {
	case idErr == nil:
		npub, err = identityNpub()
		if err != nil {
			return "", "", err
		}
		source = string(idStored.Source)
		if idStored.Source == identity.SourceNcliVault {
			source = fmt.Sprintf("ncli-vault:%s", idStored.Label)
		}
		return npub, source, nil
	case errors.Is(idErr, identity.ErrNotConfigured):
		return "", "", nil
	default:
		return "", "", idErr
	}
}
