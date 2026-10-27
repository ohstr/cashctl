package cmd

import (
	"errors"
	"strings"

	"github.com/ohstr/nmilat/nip19"
	"github.com/ohstr/nmilat/nipcash"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/output"
)

// newCashMintersCmd manages trusted_minters: the minters whose Cash Zaps are
// checked with their Hub without asking first (NIP-CZ rule 8), each for one
// chain.
func newCashMintersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "minters",
		Short: "The minters you trust for Cash Zaps, per chain",
		RunE:  groupRunE,
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List the trusted minters",
		Args:  output.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return printMinters(cmd)
		},
	}
	add := &cobra.Command{
		Use:   "add <minter>... --chain <chain>",
		Short: "Trust minters (hex, compressed hex or npub) for one chain",
		Args:  output.MinimumNArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return editMinters(cmd, args, config.TrustMinter) },
	}
	rm := &cobra.Command{
		Use:   "rm <minter>... --chain <chain>",
		Short: "Stop trusting minters for one chain",
		Args:  output.MinimumNArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return editMinters(cmd, args, config.UntrustMinter) },
	}
	for _, c := range []*cobra.Command{add, rm} {
		c.Flags().String("chain", "", "the chain the trust is for (bitcoin, flokicoin, ...) — required")
	}
	cmd.AddCommand(list, add, rm)
	return cmd
}

// minterXOnly reads a minter key: 64 hex, 66 hex (02/03), or an npub.
func minterXOnly(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if strings.HasPrefix(s, "npub1") {
		return nip19.DecodePublicKey(s)
	}
	if len(s) == 66 {
		return nipcash.MinterXOnly(s)
	}
	if isHexKey(s) {
		return s, nil
	}
	return "", errors.New("a minter is a hex public key (x-only or compressed) or an npub")
}

func isHexKey(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func editMinters(cmd *cobra.Command, args []string, apply func(config.TrustedMinter) error) error {
	if err := rejectConnectionFlag(cmd); err != nil {
		return err
	}
	chain, _ := cmd.Flags().GetString("chain")
	if chain == "" {
		return output.InvocationError(cmd, errors.New("--chain is required: trust is for one chain only"))
	}
	for _, a := range args {
		x, err := minterXOnly(a)
		if err != nil {
			return output.InvalidInputError(cmd, a, err)
		}
		if err := apply(config.TrustedMinter{MinterXOnly: x, Chain: chain}); err != nil {
			return output.RuntimeError(cmd, err)
		}
	}
	return printMinters(cmd)
}

func printMinters(cmd *cobra.Command) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	list, err := config.TrustedMinters()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	if jsonMode {
		output.PrintJSON(map[string]any{"minters": output.NonNil(list)})
		return nil
	}
	if len(list) == 0 {
		output.Println("No trusted minters.")
	}
	for _, m := range list {
		output.Printf("%s  %s\n", m.MinterXOnly, m.Chain)
	}
	return nil
}
