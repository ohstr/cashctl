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
			h, err := dialForCommand(cmd)
			if err != nil {
				return err
			}
			defer h.Close()
			client := h.client
			budget, err := client.GetBudget(h.ctx)
			if err != nil {
				return classifyNWCErr(cmd, err)
			}
			if jsonMode {
				output.PrintJSON(budget)
				return nil
			}
			fmt.Printf("used:    %d %s\n", budget.UsedBudgetMloki, output.CurrencyUnit)
			fmt.Printf("total:   %d %s\n", budget.TotalBudgetMloki, output.CurrencyUnit)
			fmt.Printf("renewal: %s\n", budget.RenewalPeriod)
			if budget.RenewsAt != nil {
				fmt.Printf("renews:  %s\n", time.Unix(*budget.RenewsAt, 0).UTC().Format(time.RFC3339))
			}
			return nil
		},
	}
}

func newWalletInvoiceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   fmt.Sprintf("invoice <amount-%s>", output.CurrencyUnit),
		Short: "Create a Lightning invoice on the connected wallet",
		Args:  output.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonMode, _ := cmd.Flags().GetBool("json")
			desc, _ := cmd.Flags().GetString("desc")
			var amount int64
			if _, err := fmt.Sscanf(args[0], "%d", &amount); err != nil || amount <= 0 {
				return output.InvalidInputError(cmd, args[0], fmt.Errorf("amount must be a positive number of %s", output.CurrencyUnit))
			}
			h, err := dialForCommand(cmd)
			if err != nil {
				return err
			}
			defer h.Close()
			client := h.client
			tx, err := client.MakeInvoice(h.ctx, nip47.MakeInvoiceParams{Amount: amount, Description: desc})
			if err != nil {
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
		Use:   "pay <invoice>",
		Short: "Pay a Lightning invoice from the connected wallet",
		Args:  output.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonMode, _ := cmd.Flags().GetBool("json")
			h, err := dialForCommand(cmd)
			if err != nil {
				return err
			}
			defer h.Close()
			client := h.client
			result, err := client.PayInvoice(h.ctx, nip47.PayInvoiceParams{Invoice: args[0]})
			if err != nil {
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
				fmt.Printf("Paid. Fee: %d %s (+ %d %s circle forwarding fee).\n", result.FeesPaidMloki, output.CurrencyUnit, result.FeeSkimMloki, output.CurrencyUnit)
			} else {
				fmt.Printf("Paid. Fee: %d %s.\n", result.FeesPaidMloki, output.CurrencyUnit)
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
			h, err := dialForCommand(cmd)
			if err != nil {
				return err
			}
			defer h.Close()
			client := h.client
			result, err := client.ListTransactions(h.ctx, nip47.ListTransactionsParams{})
			if err != nil {
				return classifyNWCErr(cmd, err)
			}
			if jsonMode {
				output.PrintJSON(result)
				return nil
			}
			for _, tx := range result.Transactions {
				fmt.Printf("%-9s %-9s %8d %-5s %s\n", tx.Type, tx.State, tx.AmountMloki, output.CurrencyUnit, tx.Description)
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
			h, err := dialForCommand(cmd)
			if err != nil {
				return err
			}
			defer h.Close()
			client := h.client
			result, err := client.SignMessage(h.ctx, nip47.SignMessageParams{Message: args[0]})
			if err != nil {
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
