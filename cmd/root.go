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
	Long: `A wallet CLI for Cash and Circle wallets.

Results go to stdout (one JSON document with --json); errors to stderr.
With --json an error is {"error", "code", "retryable", "input"?, "nwc_code"?,
"recovery"?}. Keep "recovery" verbatim: it is the only copy of a handoff whose
local save failed.
Exit codes, and --json's "code":
  0  ok
  1  internal        anything else
  2  usage           bad or missing flags/args
  3  invalid_input   a value failed validation
  4  not_found       no such token, wallet or entry
  5  conflict        collides with current state (retryable)
  6  network         relay/wallet unreachable, or Hub not ready (retryable)
  7  auth            not authorized, or no longer
  8  indeterminate   no answer to a spend: check ` + "`cash status`" + ` before resending
  9  exists          already exists (join: you hold a wallet from that circle)
  10 spent           the Hub confirmed the bill is spent`,
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
	RootCmd.PersistentFlags().Int("pow-max", defaultPowMax, "the most proof of work (NIP-13 difficulty) cashctl does for a Hub's relays; above it nothing is sent (also CASHCTL_POW_MAX, or the pow_max_difficulty setting)")
	RootCmd.PersistentFlags().String("config-dir", "", "override cashctl's local config directory (default: OS config dir)")

	RootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if dir, _ := cmd.Flags().GetString("config-dir"); dir != "" {
			appdir.SetOverride(dir)
		}
		return nil
	}
}
