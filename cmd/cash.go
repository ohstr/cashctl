package cmd

import "github.com/spf13/cobra"

func newCashCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cash",
		Short: "Receive, redeem, transfer, and consolidate cash tokens",
		RunE:  groupRunE,
	}
	cmd.AddCommand(
		newCashReceiveCmd(),
		newCashRedeemCmd(),
		newCashTransferCmd(),
		newCashConsolidateCmd(),
		newCashListRecipientsCmd(),
	)
	return cmd
}
