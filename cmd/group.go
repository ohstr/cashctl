package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/output"
)

// groupRunE is the RunE for cashctl's own domain group commands (cash,
// wallet, connect, circle) — AGENTS.md documents "a group command invoked
// without a subcommand" as a usage error, exit 2, not cobra's own default
// of silently printing help and exiting 0. Without a RunE at all, that
// default also swallows a genuinely mistyped subcommand the same way
// (`cashctl cash recieve` prints cash's own help and exits 0 — confirmed
// live): cobra's dispatch, unable to resolve "recieve" as a child of
// "cash", falls back to invoking cash's own Run/RunE with args ["recieve"]
// rather than raising its own "unknown command" error, since a command
// with subcommands but no Run of its own is (by cobra's own design)
// assumed to have nothing meaningful to do when args don't match one —
// this RunE is what gives it something to do instead. Named args[0] when
// present, so a typo is reported as an unknown SUBcommand, not just "you
// forgot one".
func groupRunE(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return output.UsageError(cmd, fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath()))
	}
	return output.UsageError(cmd, fmt.Errorf("%q requires a subcommand", cmd.CommandPath()))
}
