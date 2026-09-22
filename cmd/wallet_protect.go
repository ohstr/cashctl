package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// newWalletProtectCmd exposes protectRekeyOnly (cmd/receive_secure.go) as
// a standalone action, for the one case `receive`'s own automatic offer
// can't reach: a cash-mode holding that was received unprotected (the offer
// was declined, or the attempt failed) and is still shared — spendable by
// anyone else who was shown the same secret. The documented recovery,
// `consolidate --to cash`, needs 2+ sources and can't re-key a
// single holding alone ("needs at least 2 sources, got 1"); this can,
// since it's the exact same single-source re-key `receive` already runs,
// just triggered manually instead of automatically.
func newWalletProtectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "protect [id]",
		Short: "Re-key a still-shared bearer holding so the original secret can no longer spend it",
		Long: `Re-keys a bearer-mode holding's spending secret in place — the same protection ` +
			"`receive` offers automatically for a fresh bearer gift, for a holding that missed it " +
			"(declined, or the attempt failed) and is still shared with anyone who has the original secret.",
		Example: `  cashctl wallet protect
  cashctl wallet protect <id>`,
		Args: output.MaximumNArgs(1),
		RunE: runWalletProtect,
	}
	cmd.Flags().String("token", "", "which held token to protect (see `cashctl wallet show --json`)")
	return cmd
}

func runWalletProtect(cmd *cobra.Command, args []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	l, err := ledger.Load()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}

	entry, err := resolveUnprotectedCashHolding(cmd, l, args)
	if err != nil {
		return err
	}

	status, _ := protectRekeyOnly(cmd, l, entry, jsonMode)
	if status.Status == "rekeyed" {
		if jsonMode {
			output.PrintJSON(map[string]any{"protected": status.json()})
		}
		return nil
	}

	// protectRekeyOnly already printed the specific reason and remediation
	// in text mode (printProtectFailure/*WrongSecret/*Ambiguous) — jsonMode
	// suppresses those, so the classified error below is the only place a
	// script learns anything went wrong. Nothing else goes to stdout on
	// this path, matching AGENTS.md's "never both a success shape and a
	// failure shape on the same stream" rule.
	statusErr := errors.New(status.Error)
	switch {
	case status.LikelyWrongSecret:
		return output.InvalidInputError(cmd, entry.ID, statusErr)
	case status.PendingSecretUnresolved:
		return output.NetworkError(cmd, statusErr)
	default:
		return output.RuntimeError(cmd, statusErr)
	}
}

// resolveUnprotectedCashHolding is wallet protect's own entry resolver —
// mirrors resolveHeldToken's shape (cash_redeem.go: --token flag, then
// positional, then auto-pick-if-one/prompt-if-many) but scoped to exactly
// what this command can act on: a cash-mode holding still
// ledger.CashShared. An explicit --token/positional naming anything else
// (already protected, pubkey-mode, not held at all) gets a specific
// reason, not a generic "not found."
func resolveUnprotectedCashHolding(cmd *cobra.Command, l *ledger.Ledger, args []string) (*ledger.Entry, error) {
	id, _ := cmd.Flags().GetString("token")
	if id == "" && len(args) > 0 {
		id = args[0]
	}
	if id != "" {
		e, ok := l.Find(id)
		if !ok {
			return nil, output.NotFoundError(cmd, id, fmt.Errorf("no held token %q", id))
		}
		if e.CashProtection != ledger.CashShared {
			return nil, output.ConflictError(cmd, id, errors.New(unprotectableReason(e)))
		}
		return e, nil
	}

	var eligible []ledger.Entry
	for _, e := range l.Held() {
		if e.CashProtection == ledger.CashShared {
			eligible = append(eligible, e)
		}
	}
	if len(eligible) == 0 {
		return nil, output.NotFoundError(cmd, "", errors.New("no unprotected cash-mode holdings — nothing to protect (see `cashctl wallet show`)"))
	}
	pickedID := eligible[0].ID
	if len(eligible) > 1 {
		picked, err := pickHeldToken(cmd, eligible)
		if err != nil {
			return nil, err
		}
		pickedID = picked.ID
	}
	// Re-resolve by ID against l itself (not the Held()/pickHeldToken
	// copy) so the returned pointer is the live entry — same reasoning as
	// resolveHeldToken's own doc comment.
	e, ok := l.Find(pickedID)
	if !ok {
		return nil, output.NotFoundError(cmd, pickedID, fmt.Errorf("no held token %q", pickedID))
	}
	return e, nil
}

func unprotectableReason(e *ledger.Entry) string {
	if e.CashProtection == ledger.CashProtected {
		return fmt.Sprintf("%q is already protected", e.ID)
	}
	return fmt.Sprintf("%q isn't a cash-mode holding — protecting only applies to a shared cash secret", e.ID)
}
