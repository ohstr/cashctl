package cmd

import (
	"strings"

	"github.com/spf13/cobra"
)

// Top-level shortcuts for the highest-frequency verbs — same relationship
// as `docker run` to `docker container run`: each shares its RunE (and
// flags) with the canonical, fully-namespaced command instead of
// duplicating logic (cashctl-plan.md's "Top-level shortcuts (docker-style)").
func init() {
	RootCmd.AddCommand(
		newWalletInitCmd(),
		shortcutOf(newCashReceiveCmd()),
		shortcutOf(newCashRedeemCmd()),
		shortcutOf(newCashTransferCmd()),
		shortcutOf(newCashConsolidateCmd()),
		shortcutOf(newCashSettleCmd()),
		shortcutOf(newWalletBalanceCmd()),
		shortcutOf(newWalletPayCmd()),
		shortcutOf(newWalletInvoiceCmd()),
		shortcutOf(newCircleJoinCmd()),
		newCashCmd(),
		newCircleCmd(),
		newWalletCmd(),
		newConnectCmd(),
		newDecodeCmd(),
		newSkillsCmd(),
		newConfigCmd(),
		newVersionCmd(),
	)
}

// shortcutOf returns a fresh top-level command sharing cmd's Use/Short/
// Long/Example/Args/RunE/flags — a distinct *cobra.Command instance
// (cobra commands are single-parent), not the same node reparented.
func shortcutOf(cmd *cobra.Command) *cobra.Command {
	clone := &cobra.Command{
		Use:     cmd.Use,
		Short:   cmd.Short,
		Long:    cmd.Long,
		Example: shortcutExample(cmd),
		Args:    cmd.Args,
		RunE:    cmd.RunE,
	}
	clone.Flags().AddFlagSet(cmd.Flags())
	return clone
}

// shortcutExample is cmd's Example written the way the shortcut is typed:
// `cashctl balance` under `cashctl balance --help`, not `cashctl wallet balance`.
func shortcutExample(cmd *cobra.Command) string {
	verb := strings.Fields(cmd.Use)[0]
	ex := cmd.Example
	for _, group := range []string{"wallet", "cash", "circle"} {
		ex = strings.ReplaceAll(ex, "cashctl "+group+" "+verb, "cashctl "+verb)
	}
	return ex
}
