package cmd

import (
	"os"

	"github.com/spf13/cobra"
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
