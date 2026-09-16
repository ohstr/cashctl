package cmd

import (
	"context"
	"fmt"
	"time"

	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

func newCashListRecipientsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list-recipients",
		Short: "Check your allocation and co-recipients of a held token",
		Long: `Recipients of the same mint_cash batch share one wallet connection —
this is how a receiver checks their own allocation and co-recipients
within it. It's the exact call "cashctl receive" makes internally, to
check a token before saving it.`,
		Args: output.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonMode, _ := cmd.Flags().GetBool("json")
			l, err := ledger.Load()
			if err != nil {
				return output.RuntimeError(cmd, err)
			}
			entry, err := resolveHeldToken(cmd, l)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			client, err := nipcashclient.Connect(ctx, entry.Token)
			if err != nil {
				return output.NetworkError(cmd, err)
			}
			defer client.Close()
			result, err := client.ListRecipients(ctx)
			if err != nil {
				return classifyNWCErr(cmd, err)
			}
			if jsonMode {
				output.PrintJSON(result)
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
				fmt.Printf("%-40s %10d %s   %s\n", identity, r.AmountMillis, output.CurrencyUnit, status)
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
