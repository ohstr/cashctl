package cmd

import (
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/output"
)

// Version is injected at build time via -ldflags
// "-X github.com/ohstr/cashctl/cmd.Version=..." (see .goreleaser.yaml) —
// "dev" for a plain `go build`/`go run`.
var Version = "dev"

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the cashctl version",
		Args:  output.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonMode, _ := cmd.Flags().GetBool("json")
			if jsonMode {
				output.PrintJSON(map[string]any{"version": Version})
				return nil
			}
			output.Println(Version)
			return nil
		},
	}
}
