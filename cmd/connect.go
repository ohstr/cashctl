package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/output"
)

func newConnectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "connect",
		Short: "Manage foreign wallet connections",
		Long:  `Registers any NWC connection cashctl didn't create itself.`,
		Example: `  cashctl connect add work nostr+walletconnect://...
  cashctl connect list`,
	}
	cmd.AddCommand(newConnectAddCmd(), newConnectListCmd(), newConnectUseCmd(), newConnectRmCmd())
	return cmd
}

func newConnectAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add <name> <connection>",
		Short: "Register a connection under a name",
		Args:  output.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonMode, _ := cmd.Flags().GetBool("json")
			// Trimmed: a stray space/newline makes url.Parse reject an
			// otherwise-valid nostr+walletconnect:// URI outright.
			name, value := args[0], strings.TrimSpace(args[1])

			s, err := config.Load()
			if err != nil {
				return output.RuntimeError(cmd, err)
			}
			wasEmpty := s.IsEmpty()
			if err := s.Add(name, value); err != nil {
				return output.ConflictError(cmd, name, err)
			}

			setDefault := false
			if wasEmpty {
				setDefault = jsonMode || Confirm(cmd, true, "This is your only wallet — use it as your default?")
			} else {
				setDefault = !jsonMode && Confirm(cmd, false, fmt.Sprintf("Set %s as your default wallet?", name))
			}
			if setDefault {
				_ = s.SetDefault(name)
			}
			if err := s.Save(); err != nil {
				return output.RuntimeError(cmd, err)
			}

			if jsonMode {
				output.PrintJSON(map[string]any{"name": name, "default": setDefault})
				return nil
			}
			fmt.Printf("Saved connection: %s.\n", name)
			if setDefault {
				fmt.Printf("Default wallet set to %s.\n", name)
			}
			return nil
		},
	}
}

func newConnectListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered connections",
		Args:  output.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonMode, _ := cmd.Flags().GetBool("json")
			s, err := config.Load()
			if err != nil {
				return output.RuntimeError(cmd, err)
			}
			if jsonMode {
				output.PrintJSON(map[string]any{"connections": s.Connections, "default": s.Default})
				return nil
			}
			if s.IsEmpty() {
				fmt.Println("No connections registered yet. Run `cashctl init` or `cashctl connect add`.")
				return nil
			}
			for _, c := range s.Connections {
				marker := ""
				if c.Name == s.Default {
					marker = " [default]"
				}
				fmt.Printf("%s%s\n", c.Name, marker)
			}
			return nil
		},
	}
}

func newConnectUseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use <name>",
		Short: "Set the default connection",
		Args:  output.ExactArgs(1),
		RunE:  runWalletUse,
	}
}

// runWalletUse backs both `cashctl wallet use` (cashctl-plan.md's canonical
// form, mirroring ncli's own `relay context` pattern) and `cashctl connect
// use` (the Command Tree section's own connect-group listing) — same
// action, two entry points sharing one function rather than duplicating
// the logic, the same pattern the top-level shortcuts use throughout.
func runWalletUse(cmd *cobra.Command, args []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	s, err := config.Load()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	if err := s.SetDefault(args[0]); err != nil {
		return output.NotFoundError(cmd, args[0], err)
	}
	if err := s.Save(); err != nil {
		return output.RuntimeError(cmd, err)
	}
	if jsonMode {
		output.PrintJSON(map[string]any{"default": args[0]})
		return nil
	}
	fmt.Printf("Default wallet set to %s.\n", args[0])
	return nil
}

func newConnectRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a registered connection",
		Args:  output.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonMode, _ := cmd.Flags().GetBool("json")
			s, err := config.Load()
			if err != nil {
				return output.RuntimeError(cmd, err)
			}
			if !s.Remove(args[0]) {
				return output.NotFoundError(cmd, args[0], fmt.Errorf("no connection named %q", args[0]))
			}
			if err := s.Save(); err != nil {
				return output.RuntimeError(cmd, err)
			}
			if jsonMode {
				output.PrintJSON(map[string]any{"removed": args[0]})
				return nil
			}
			fmt.Printf("Removed %s.\n", args[0])
			return nil
		},
	}
}
