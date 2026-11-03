package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/cashkitbackup"
	"github.com/ohstr/cashctl/internal/cashkitimport"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

func newWalletImportCashkitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "import-cashkit <backup-file>",
		Short: "Import cash tokens from a cashkit wallet backup",
		Long: `Restores the cash held by a cashkit wallet into this (fresh) cashctl wallet,
from a cashkit v3 backup file.

The whole backup is authenticated before anything in it is trusted: a wrong
passphrase, a foreign file, or one changed since it was written is refused.
Set CASHKIT_BACKUP_PASSPHRASE to supply the passphrase non-interactively
(required under --json); otherwise you are prompted on the terminal.

Only live, cash-mode tokens are imported — their spending secret exists only in
the backup. Identity-bound tokens (pubkey / web-identity cash) are reported but
not imported: spending them needs the cashkit identity key, which this command
does not carry yet. Import refuses a cashctl wallet that already holds cash: it
is a restore into a fresh wallet, not a merge of two ledgers.`,
		Example: `  cashctl wallet import-cashkit ./cashkit-backup.json
  CASHKIT_BACKUP_PASSPHRASE=… cashctl wallet import-cashkit ./bk.json --json`,
		Args: output.ExactArgs(1),
		RunE: runWalletImportCashkit,
	}
}

func runWalletImportCashkit(cmd *cobra.Command, args []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")

	data, err := os.ReadFile(args[0])
	if err != nil {
		return output.RuntimeError(cmd, fmt.Errorf("read backup file: %w", err))
	}

	passphrase, err := resolveBackupPassphrase(jsonMode)
	if err != nil {
		return output.RuntimeError(cmd, err)
	}

	b, err := cashkitbackup.Open(data, passphrase)
	if err != nil {
		switch {
		case errors.Is(err, cashkitbackup.ErrWrongPassphrase):
			return output.AuthError(cmd, err)
		case errors.Is(err, cashkitbackup.ErrTampered),
			errors.Is(err, cashkitbackup.ErrNotBackup),
			errors.Is(err, cashkitbackup.ErrVersion):
			return output.InvalidInputError(cmd, "", err)
		default:
			return output.RuntimeError(cmd, err)
		}
	}

	l, err := ledger.Load()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	for i := range l.Entries {
		if ledger.Live(l.Entries[i].Status) {
			return output.FinalConflictError(cmd, "", errors.New(
				"this cashctl wallet already holds cash; import a cashkit backup into a fresh wallet (its tokens are a restore, not a merge)"))
		}
	}

	plan, err := cashkitimport.BuildPlan(b)
	if err != nil {
		return output.InvalidInputError(cmd, "", err)
	}

	var ids []string
	for _, e := range plan.Entries {
		added, aerr := l.Add(e)
		if aerr != nil {
			// A token already live in the ledger (e.g. a duplicate within the
			// backup) — count it skipped rather than fail the whole restore.
			plan.Skipped["duplicate"]++
			continue
		}
		ids = append(ids, added.ID)
	}
	imported := len(ids)
	if imported > 0 {
		l.AppendHistory("import-cashkit", fmt.Sprintf("imported %d cash token(s) from cashkit wallet %s", imported, plan.WalletID))
		if err := l.Save(); err != nil {
			return output.RuntimeError(cmd, err)
		}
	}

	if jsonMode {
		output.PrintJSON(map[string]any{
			"wallet_id":     plan.WalletID,
			"imported":      imported,
			"amount_millis": plan.TotalMillis,
			"token_ids":     output.NonNil(ids),
			"skipped":       plan.Skipped,
		})
		return nil
	}

	output.Printf("Imported %d cash token(s) totaling %s from cashkit wallet %s.\n",
		imported, output.FormatAmount(int64(plan.TotalMillis)), plan.WalletID)
	if n := plan.IdentityBoundSkipped(); n > 0 {
		output.Printf("Skipped %d identity-bound token(s): importing these needs the cashkit identity key, which this command does not carry yet.\n", n)
	}
	if n := plan.Skipped[cashkitimport.SkipNotLive]; n > 0 {
		output.Printf("Skipped %d spent/expired/past token(s).\n", n)
	}
	if n := plan.Skipped[cashkitimport.SkipNoSecret]; n > 0 {
		output.Printf("Skipped %d cash token(s) with no recoverable spending secret.\n", n)
	}
	if n := plan.Skipped[cashkitimport.SkipUndecodable]; n > 0 {
		output.Printf("Skipped %d token(s) that did not decode.\n", n)
	}
	if n := plan.Skipped["duplicate"]; n > 0 {
		output.Printf("Skipped %d duplicate token(s).\n", n)
	}
	if imported > 0 {
		output.Println("Run `cashctl wallet show` to see them.")
	}
	return nil
}

// resolveBackupPassphrase returns the cashkit backup passphrase:
// CASHKIT_BACKUP_PASSPHRASE if set (the non-interactive/agentic path),
// otherwise a hidden terminal prompt. Errors under --json with no env var set,
// rather than blocking on a prompt no agent can answer (mirrors
// ResolveVaultPassword's shape, with the backup's own variable).
func resolveBackupPassphrase(jsonMode bool) (string, error) {
	if pw := os.Getenv("CASHKIT_BACKUP_PASSPHRASE"); pw != "" {
		return pw, nil
	}
	if jsonMode {
		return "", errors.New("backup passphrase required: set CASHKIT_BACKUP_PASSPHRASE (no interactive prompt under --json)")
	}
	return readSecretFromTTY("cashkit backup passphrase: ")
}
