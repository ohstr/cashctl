package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/ohstr/cashctl/internal/output"
)

// asCredentialEnv is the non-argv way to supply --as.
const asCredentialEnv = "CASHCTL_AS"

// asCredentialValue returns the --as credential: the flag if given, otherwise
// CASHCTL_AS.
//
// --as is always secret-bearing — pubkey:<priv>, cash:<secret>, or
// connection-key:<privkey>,... — and a value passed on the command line is
// visible to `ps` for as long as the process runs and is written to the shell's
// history file. No process can hide its own argv, so the only fix available from
// inside cashctl is to offer somewhere else to put it (D-CLI-5).
//
// Be clear about what this does and does not buy. It removes the `ps` and
// shell-history exposure. It does NOT make the value secret from everything: an
// environment variable is inherited by child processes and readable via
// /proc/<pid>/environ by the same user. It is better, not airtight, and the docs
// say so rather than implying otherwise.
//
// An env var rather than a flag or a stdin mode, following NCLI_VAULT_PASSWORD
// and NO_COLOR: it keeps the capability off the --help surface and out of
// per-invocation argv, so it cannot be slipped into one agent-driven command —
// it has to be set deliberately for the session.
func asCredentialValue(cmd *cobra.Command) string {
	if as, _ := cmd.Flags().GetString("as"); as != "" {
		return as
	}
	return os.Getenv(asCredentialEnv)
}

// stdinArg, given in place of a secret-bearing argument (a <token>#<secret>
// bill, an NWC URI, a Circle Hub connection), reads that value from stdin
// instead: like CASHCTL_AS, it keeps the secret out of `ps` and shell history.
const stdinArg = "-"

// argOrStdin returns arg, or the first line of stdin when arg is stdinArg.
// It reads through the same buffered reader as the prompts, so a later
// confirmation still gets the lines after it.
func argOrStdin(cmd *cobra.Command, arg string) (string, error) {
	if strings.TrimSpace(arg) != stdinArg {
		return arg, nil
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		// Typed rather than piped in: say what is being waited for, on
		// stderr like every prompt.
		output.Fprintf(os.Stderr, "Paste the value, then press Enter: ")
	}
	line, err := stdin.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", output.RuntimeError(cmd, fmt.Errorf("read the value from stdin: %w", err))
	}
	if v := strings.TrimSpace(line); v != "" {
		return v, nil
	}
	return "", output.InvocationError(cmd, errors.New(`"-" reads the value from stdin, but stdin was empty`))
}
