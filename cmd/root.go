// Package cmd wires cashctl's Cobra command tree. Every command here stays
// thin — the actual logic lives in internal/ (config, identity, ledger,
// credential, output, dial) so it can be unit-tested directly; commands
// that touch the network are instead exercised by the integration and
// agent-eval suites (mirroring ncli's own cli/ vs client/ split).
package cmd

import (
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/appdir"
)

// RootCmd is cashctl's top-level command.
var RootCmd = &cobra.Command{
	Use:   "cashctl",
	Short: "A wallet CLI for Cash and Circle wallets",
	Example: `  cashctl init
  cashctl join <hub-connection> <max-amount>
  cashctl receive <token>
  cashctl transfer 5 <pubkey>`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	RootCmd.PersistentFlags().Bool("json", false, "output machine-readable JSON instead of human-readable text")
	RootCmd.PersistentFlags().StringP("connection", "c", "", "use this connection/wallet by name (or a raw connection string) instead of the default")
	RootCmd.PersistentFlags().Bool("yes", false, "skip confirmation prompts")
	RootCmd.PersistentFlags().String("config-dir", "", "override cashctl's local config directory (default: OS config dir)")

	RootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if dir, _ := cmd.Flags().GetString("config-dir"); dir != "" {
			appdir.SetOverride(dir)
		}
		return nil
	}
}
