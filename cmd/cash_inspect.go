package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

func newCashStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "status",
		// The old name stays as an alias. The METHOD alias `list_recipients` was
		// removed from the wire (nothing had been minted against it), but a CLI name
		// is a different surface: it appears in scripts, in skills/cashctl-cash and
		// in AGENTS.md, and breaking it buys nothing. The command is renamed because
		// `cash_status` is now the only method it calls, so the old name described
		// something that no longer exists.
		Aliases: []string{"list-recipients"},
		Short:   "Check your allocation and co-recipients of a held token",
		Long:    `Shows your share and co-recipients of a held token's mint batch.`,
		Example: `  cashctl cash status`,
		Args:    output.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rejectConnectionFlag(cmd); err != nil {
				return err
			}
			jsonMode, _ := cmd.Flags().GetBool("json")
			l, err := ledger.Load()
			if err != nil {
				return output.RuntimeError(cmd, err)
			}
			entry, err := resolveHeldToken(cmd, l)
			if err != nil {
				return err
			}
			var result *nipcash.CashStatusResult
			var dialErr bool
			err = WithSpinner(jsonMode, "Fetching recipients...", func() error {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				client, cErr := nipcashclient.Connect(ctx, entry.Token)
				if cErr != nil {
					dialErr = true
					return cErr
				}
				defer client.Close()
				cred, credErr := resolveCredential(cmd, entry)
				if credErr != nil {
					return credErr
				}
				// ScopeAll: `cash list-recipients` exists to show the co-recipients,
				// which is the one place asking for the shared roster is the point.
				r, cErr := client.CashStatus(ctx, cred, nipcash.ScopeAll)
				if cErr != nil {
					return cErr
				}
				result = r
				return nil
			})
			if err != nil {
				if dialErr {
					return output.NetworkError(cmd, err)
				}
				return classifyCashTokenNWCErr(cmd, err)
			}
			if jsonMode {
				result.Recipients = output.NonNil(result.Recipients)
				output.PrintJSON(result)
				return nil
			}
			// A tombstone before the roster, because a spent bill HAS no roster and
			// would otherwise render as zero bytes and exit 0 — silence, which is
			// exactly what this command is asked to distinguish from.
			//
			// This is the CLI's own prescribed recovery step: after a redeem whose
			// reply never arrived, applyRedeemResults tells the user to run
			// `cashctl cash status --token <id>` to find out whether their money
			// moved. Printing nothing answered that question with silence, and the
			// answer was available — the Hub retains a deleted bill precisely so its
			// holder gets a definitive "spent" instead of having to guess. Past
			// retention the Hub does fall silent, and explainNoAnswer covers that
			// well; the gap was the window where the Hub tells the truth.
			if result.IsSpent() {
				fmt.Println("This bill is spent — its value has already moved, and the bill itself is gone.")
				if result.RetainedUntil != nil {
					fmt.Printf("The Hub will keep answering about it until %s; after that it goes silent.\n",
						time.Unix(*result.RetainedUntil, 0).UTC().Format("2006-01-02 15:04 UTC"))
				}
				return nil
			}

			for _, r := range result.Recipients {
				status := "unclaimed"
				if r.Claimed {
					status = "claimed"
					if r.ClaimedAt != nil {
						status = fmt.Sprintf("claimed %s", time.Unix(*r.ClaimedAt, 0).UTC().Format("2006-01-02"))
					}
				}
				// Sanitized: identity_type/identity_value are unvalidated
				// wire strings from the Hub, not cashctl's own text.
				identity := output.Sanitize(r.IdentityType)
				if r.IdentityValue != "" {
					identity = fmt.Sprintf("%s:%s", output.Sanitize(r.IdentityType), output.Sanitize(r.IdentityValue))
				}
				fmt.Printf("%-40s %14s   %s\n", identity, output.FormatAmount(int64(r.AmountMillis)), status)
			}
			// ExpiresAt is identical on every row (NIP-CASH §Listing
			// Recipients: one shared wallet-level deadline) — shown once,
			// after the roster, same wording as decode --check's own cash
			// token report (decode.go's formatExpiry). Unlike a money-
			// moving confirmation (redeem/transfer/consolidate), this is
			// pure inspection with nothing to gate, so it's always shown,
			// not just when it's close.
			if len(result.Recipients) > 0 && result.Recipients[0].ExpiresAt != nil {
				fmt.Println(formatExpiry(*result.Recipients[0].ExpiresAt))
			}
			return nil
		},
	}
	cmd.Flags().String("token", "", "which held token (auto-picked if you only hold one)")
	return cmd
}

// joinStrings renders a token/connection's own relay URLs for display.
// Sanitized: a relay URL has no character restrictions of its own — an
// attacker-crafted one could otherwise carry a terminal escape sequence.
func joinStrings(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += output.Sanitize(s)
	}
	return out
}
