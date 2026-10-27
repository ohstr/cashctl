package cmd

import (
	"fmt"
	"io"
	"os"
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
		RunE: groupRunE,
	}
	cmd.AddCommand(newConnectAddCmd(), newConnectListCmd(), newConnectUseCmd(), newConnectRmCmd())
	return cmd
}

func newConnectAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add <name> <connection>",
		Short: "Register a connection under a name",
		Long: `Registers a wallet connection under a name, for -c/--connection, --into
and the default wallet.

<connection> is a nostr+walletconnect:// URI, or a circle wallet's backup
file (the {"type":"circle_wallet",...} JSON ` + "`join --backup-file`" + ` writes), given
as a path or on stdin with -. A bill is never a wallet: a cash token or
<token>#<secret> is taken with ` + "`cashctl receive`" + `, and a circlehub1...
connection is joined with ` + "`cashctl join`" + `; both are refused here. Your first
wallet becomes the default.`,
		Example: `  cashctl connect add savings "nostr+walletconnect://<pubkey>?relay=wss://...&secret=..."
  cashctl connect add savings - < savings.uri    # read it from stdin, keeping the secret out of ps and shell history
  cashctl connect add circle:family wallet.json  # a circle wallet's backup file`,
		Args: output.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonMode, _ := cmd.Flags().GetBool("json")
			// Trimmed: a stray space/newline makes url.Parse reject an
			// otherwise-valid nostr+walletconnect:// URI outright.
			raw, err := connectionArg(cmd, args[1])
			if err != nil {
				return err
			}
			name, value := args[0], strings.TrimSpace(raw)
			if err := validateConnectionValue(value); err != nil {
				// A circle wallet backup is never echoed, even redacted: only
				// its wallet_pubkey names it (C-1).
				input := output.RedactSecretInput(value)
				if w, ok, _ := config.ParseCircleWallet(value); ok {
					input = output.RedactSecretInput(w.WalletPubkey)
				} else if strings.HasPrefix(value, "{") {
					input = ""
				}
				return output.InvalidInputError(cmd, input, err)
			}
			kind := config.KindNWC
			if w, ok, _ := config.ParseCircleWallet(value); ok {
				// Stored as its fields, compacted; never as a URI.
				if value, err = w.Encode(); err != nil {
					return output.RuntimeError(cmd, err)
				}
				kind = config.KindCircle
			}

			s, err := config.Load()
			if err != nil {
				return output.RuntimeError(cmd, err)
			}
			wasEmpty := s.IsEmpty()
			if err := s.Add(name, value); err != nil {
				// invalid_input, not conflict: conflict is documented as
				// retryable, and re-running with the same taken name can
				// never succeed — the caller has to pick a different one.
				return output.InvalidInputError(cmd, name, err)
			}
			s.SetKind(name, kind)

			setDefault := false
			if wasEmpty {
				setDefault = jsonMode || Confirm(cmd, true, "This is your only wallet — use it as your default?")
			} else {
				// An existing default is only ever replaced by an explicit
				// answer: --yes means "don't ask", not "yes to changing which
				// wallet my money goes to" (--json already behaves this way).
				yes, _ := cmd.Flags().GetBool("yes")
				setDefault = !jsonMode && !yes && Confirm(cmd, false, fmt.Sprintf("Set %s as your default wallet?", name))
			}
			if setDefault {
				_ = s.SetDefault(name)
			}
			if err := s.Save(); err != nil {
				return output.RuntimeError(cmd, err)
			}

			if jsonMode {
				out := map[string]any{"name": name, "default": setDefault, "kind": kind}
				if c, ok := s.Find(name); ok && c.WalletPubkey != "" {
					out["wallet_pubkey"] = c.WalletPubkey
				}
				output.PrintJSON(out)
				return nil
			}
			output.Printf("Saved connection: %s.\n", name)
			if setDefault {
				output.Printf("Default wallet set to %s.\n", name)
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
				output.PrintJSON(map[string]any{"connections": output.NonNil(s.Connections), "default": s.Default})
				return nil
			}
			if s.IsEmpty() {
				output.Println("No connections registered yet. Run `cashctl init` or `cashctl connect add`.")
				return nil
			}
			for _, c := range s.Connections {
				marker := ""
				if c.Name == s.Default {
					marker = " [default]"
				}
				output.Printf("%s (%s)%s%s\n", c.Name, c.Kind, walletPubkeySuffix(c), marker)
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
		return output.NotFoundError(cmd, args[0], fmt.Errorf("%w; see `cashctl connect list`", err))
	}
	if err := s.Save(); err != nil {
		return output.RuntimeError(cmd, err)
	}
	if jsonMode {
		output.PrintJSON(map[string]any{"default": args[0]})
		return nil
	}
	output.Printf("Default wallet set to %s.\n", args[0])
	return nil
}

func newConnectRmCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a registered connection",
		Long: `Forgets a registered connection here. A circle wallet's key is held only on
your devices, so forgetting it can strand its balance: it is refused unless
--force; delete the wallet on its Hub with ` + "`cashctl wallet delete`" + ` instead.`,
		Args: output.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonMode, _ := cmd.Flags().GetBool("json")
			force, _ := cmd.Flags().GetBool("force")
			s, err := config.Load()
			if err != nil {
				return output.RuntimeError(cmd, err)
			}
			if c, ok := s.Find(args[0]); ok && c.Kind == config.KindCircle && !force {
				return output.UsageError(cmd, fmt.Errorf(
					"%s is a circle wallet whose key is held only on your devices: removing it here can strand its balance. Pay it out and `cashctl wallet delete %s`, or pass --force", args[0], args[0]))
			}
			if !s.Remove(args[0]) {
				return output.NotFoundError(cmd, args[0], fmt.Errorf("no connection named %q; see `cashctl connect list`", args[0]))
			}
			if err := s.Save(); err != nil {
				return output.RuntimeError(cmd, err)
			}
			if jsonMode {
				output.PrintJSON(map[string]any{"removed": args[0]})
				return nil
			}
			output.Printf("Removed %s.\n", args[0])
			return nil
		},
	}
	cmd.Flags().Bool("force", false, "remove a circle wallet here even though its balance may be stranded")
	return cmd
}

// walletPubkeySuffix names a circle wallet by its wallet_pubkey, never more.
func walletPubkeySuffix(c config.Connection) string {
	if c.WalletPubkey == "" {
		return ""
	}
	return " wallet " + c.WalletPubkey
}

// connectionArg is connect add's <connection>: "-" reads all of stdin (a
// backup file spans several lines), a path to an existing file reads that
// file, anything else is the value itself.
func connectionArg(cmd *cobra.Command, arg string) (string, error) {
	if strings.TrimSpace(arg) == stdinArg {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return "", output.RuntimeError(cmd, err)
		}
		return string(b), nil
	}
	if !strings.Contains(arg, "://") {
		if st, err := os.Stat(arg); err == nil && st.Mode().IsRegular() {
			b, err := os.ReadFile(arg)
			if err != nil {
				return "", output.InvalidInputError(cmd, arg, err)
			}
			return string(b), nil
		}
	}
	return arg, nil
}
