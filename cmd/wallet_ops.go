package cmd

import (
	"fmt"
	"time"

	"github.com/ohstr/nmilat/nip47"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/output"
)

func newWalletBudgetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "budget",
		Short: "Show the connected wallet's spend budget",
		Args:  output.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonMode, _ := cmd.Flags().GetBool("json")
			var budget *nip47.GetBudgetResult
			var dialErr bool
			err := WithSpinner(jsonMode, "Fetching budget...", func() error {
				h, hErr := dialForCommand(cmd)
				if hErr != nil {
					dialErr = true
					return hErr
				}
				defer h.Close()
				b, cErr := h.client.GetBudget(h.ctx)
				if cErr != nil {
					return cErr
				}
				budget = b
				return nil
			})
			if err != nil {
				if dialErr {
					return err
				}
				return classifyNWCErr(cmd, err)
			}
			if jsonMode {
				output.PrintJSON(budget)
				return nil
			}
			fmt.Printf("used:    %s\n", output.FormatAmount(budget.UsedBudgetMloki))
			fmt.Printf("total:   %s\n", output.FormatAmount(budget.TotalBudgetMloki))
			fmt.Printf("renewal: %s\n", output.Sanitize(budget.RenewalPeriod))
			if budget.RenewsAt != nil {
				fmt.Printf("renews:  %s\n", time.Unix(*budget.RenewsAt, 0).UTC().Format(time.RFC3339))
			}
			return nil
		},
	}
}

func newWalletInvoiceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     fmt.Sprintf("invoice <amount-%s>", output.CurrencyUnit),
		Short:   "Create a Lightning invoice on the connected wallet",
		Example: `  cashctl invoice 5 --desc "coffee"`,
		Args:    output.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonMode, _ := cmd.Flags().GetBool("json")
			desc, _ := cmd.Flags().GetString("desc")
			mloki, err := output.ParseAmount(args[0])
			if err != nil || mloki == 0 {
				return output.InvalidInputError(cmd, args[0], fmt.Errorf("amount must be a positive number of %s", output.CurrencyUnit))
			}
			amount := int64(mloki)
			var tx *nip47.Transaction
			var dialErr bool
			err = WithSpinner(jsonMode, "Creating invoice...", func() error {
				h, hErr := dialForCommand(cmd)
				if hErr != nil {
					dialErr = true
					return hErr
				}
				defer h.Close()
				t, cErr := h.client.MakeInvoice(h.ctx, nip47.MakeInvoiceParams{Amount: amount, Description: desc})
				if cErr != nil {
					return cErr
				}
				tx = t
				return nil
			})
			if err != nil {
				if dialErr {
					return err
				}
				return classifyNWCErr(cmd, err)
			}
			if jsonMode {
				output.PrintJSON(tx)
				return nil
			}
			fmt.Println(tx.Invoice)
			return nil
		},
	}
	cmd.Flags().String("desc", "", "invoice description")
	return cmd
}

func newWalletPayCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "pay <invoice>",
		Short:   "Pay a Lightning invoice from the connected wallet",
		Example: `  cashctl pay lnbc1...`,
		Args:    output.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonMode, _ := cmd.Flags().GetBool("json")
			if err := validateInvoiceShape(args[0]); err != nil {
				return output.InvalidInputError(cmd, args[0], err)
			}
			// pay used to move money the instant the invoice's shape checked
			// out — no confirmation at all, unlike every other command that
			// spends real funds — so --yes/--json had nothing to skip.
			// bolt11AmountMloki is best-effort: an amountless invoice (legal
			// BOLT11) or a decode miss just falls back to a plain prompt
			// rather than blocking pay over a preview-only detail the
			// wallet's own PayInvoice call will validate for real anyway.
			message := "Pay this Lightning invoice?"
			if amount, aErr := bolt11AmountMloki(args[0]); aErr == nil && amount != nil {
				message = fmt.Sprintf("Pay %s via this Lightning invoice?", output.FormatAmount(*amount))
			}
			// defaultYes=false: moves real money — never accept on a bare Enter.
			if !Confirm(cmd, false, message) {
				fmt.Println("Cancelled.")
				return nil
			}
			var result *nip47.PayInvoiceResult
			var dialErr bool
			err := WithSpinner(jsonMode, "Paying...", func() error {
				h, hErr := dialForCommand(cmd)
				if hErr != nil {
					dialErr = true
					return hErr
				}
				defer h.Close()
				r, cErr := h.client.PayInvoice(h.ctx, nip47.PayInvoiceParams{Invoice: args[0]})
				if cErr != nil {
					return cErr
				}
				result = r
				return nil
			})
			if err != nil {
				if dialErr {
					return err
				}
				return classifyNWCErr(cmd, err)
			}
			if jsonMode {
				output.PrintJSON(result)
				return nil
			}
			// FeeSkimMloki is a circle_wallet-only extra: the parent
			// circle_hub's forwarding-fee cut, on top of FeesPaidMloki (the
			// real Lightning routing fee) — see
			// nip47.PayInvoiceResult.FeeSkimMloki's doc comment. Called out
			// separately rather than folded into "Fee:" since it's a
			// different thing (a Hub policy charge, not network cost) and is
			// 0/absent for every non-circle wallet.
			if result.FeeSkimMloki > 0 {
				fmt.Printf("Paid. Fee: %s (+ %s circle forwarding fee).\n", output.FormatAmount(result.FeesPaidMloki), output.FormatAmount(result.FeeSkimMloki))
			} else {
				fmt.Printf("Paid. Fee: %s.\n", output.FormatAmount(result.FeesPaidMloki))
			}
			return nil
		},
	}
	return cmd
}

func newWalletListTxCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list-tx",
		Short: "List recent transactions on the connected wallet",
		Args:  output.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonMode, _ := cmd.Flags().GetBool("json")
			var result *nip47.ListTransactionsResult
			var dialErr bool
			err := WithSpinner(jsonMode, "Fetching transactions...", func() error {
				h, hErr := dialForCommand(cmd)
				if hErr != nil {
					dialErr = true
					return hErr
				}
				defer h.Close()
				r, cErr := h.client.ListTransactions(h.ctx, nip47.ListTransactionsParams{})
				if cErr != nil {
					return cErr
				}
				result = r
				return nil
			})
			if err != nil {
				if dialErr {
					return err
				}
				return classifyNWCErr(cmd, err)
			}
			if jsonMode {
				result.Transactions = output.NonNil(result.Transactions)
				output.PrintJSON(result)
				return nil
			}
			for _, tx := range result.Transactions {
				// Description is a counterparty-chosen invoice memo —
				// outside cashctl's own control, same as every other
				// Sanitize call site in this package. Type/State too, on
				// the same "anything from a wallet reply" principle, cheap
				// insurance against a misbehaving wallet.
				fmt.Printf("%-9s %-9s %12s %s\n",
					output.Sanitize(tx.Type), output.Sanitize(tx.State),
					output.FormatAmount(tx.AmountMloki), output.Sanitize(tx.Description))
			}
			return nil
		},
	}
}

func newWalletSignMessageCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sign-message <message>",
		Short: "Sign a message with the connected wallet's key",
		Args:  output.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonMode, _ := cmd.Flags().GetBool("json")
			var result *nip47.SignMessageResult
			var dialErr bool
			err := WithSpinner(jsonMode, "Signing...", func() error {
				h, hErr := dialForCommand(cmd)
				if hErr != nil {
					dialErr = true
					return hErr
				}
				defer h.Close()
				r, cErr := h.client.SignMessage(h.ctx, nip47.SignMessageParams{Message: args[0]})
				if cErr != nil {
					return cErr
				}
				result = r
				return nil
			})
			if err != nil {
				if dialErr {
					return err
				}
				return classifyNWCErr(cmd, err)
			}
			if jsonMode {
				output.PrintJSON(result)
				return nil
			}
			fmt.Println(result.Signature)
			return nil
		},
	}
}
