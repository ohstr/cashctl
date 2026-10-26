package cmd

import (
	"errors"
	"fmt"
	"slices"

	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// newWalletProtectCmd exposes protectRekeyOnly (cmd/receive_secure.go) as
// a standalone action, for the one case `receive`'s own automatic offer
// can't reach: a cash-mode holding that was received unprotected (the offer
// was declined, or the attempt failed) and is still shared — spendable by
// anyone else who was shown the same secret. `consolidate --to cash` can't
// stand in for this: it needs 2+ sources, so it can't re-key a single
// holding alone ("needs at least 2 sources, got 1"), and with no sources
// given it auto-groups, which skips cash-mode entries entirely
// (ledger.GroupableForConsolidation). This can, since it's the exact same
// single-source re-key `receive` already runs, just triggered manually.
func newWalletProtectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "protect [id]",
		Short: "Re-key a still-shared cash-mode holding so the original secret can no longer spend it",
		Long: `Re-keys a cash-mode holding that's still shared, so the original secret can ` +
			"no longer spend it. `receive` does this automatically; use this if that was " +
			"declined or failed.\n\n" +
			"With no id and more than one eligible holding, re-keys every one of them.",
		Example: `  cashctl wallet protect
  cashctl wallet protect <id>`,
		Args: output.MaximumNArgs(1),
		RunE: runWalletProtect,
	}
	cmd.Flags().String("token", "", "which held token to protect (IDs from: cashctl wallet show --json)")
	return cmd
}

func runWalletProtect(cmd *cobra.Command, args []string) error {
	if err := rejectConnectionFlag(cmd); err != nil {
		return err
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	l, err := ledger.Load()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}

	// Settle every re-key or move whose answer never came first: one that
	// landed is protected already, and one still open is never re-keyed.
	var waiting []string
	for _, e := range l.Held() {
		if e.CashProtection == ledger.CashShared && hasPendingSecret(&e) {
			waiting = append(waiting, e.ID)
		}
	}
	if err := settlePendingMoves(cmd, l); err != nil {
		return err
	}
	var settled []string
	for _, id := range waiting {
		if e, ok := l.Find(id); ok && e.CashProtection == ledger.CashProtected {
			settled = append(settled, id)
		}
	}

	named := protectNamedID(cmd, args)
	var entries, unresolved []*ledger.Entry
	if named == "" || !slices.Contains(settled, named) {
		var err error
		entries, unresolved, err = resolveUnprotectedCashHoldings(cmd, l, args)
		// Nothing left to protect is not a failure when settling just did it.
		if err != nil && !(named == "" && len(settled) > 0 && output.AsCLIError(err).Code == output.CodeNotFound) {
			return err
		}
	}
	if named != "" {
		settled = slices.DeleteFunc(settled, func(id string) bool { return id != named })
	}

	outcomes := make([]protectOutcome, 0, len(settled)+len(entries)+len(unresolved))
	for _, id := range settled {
		if !jsonMode {
			output.Println("Protected.")
		}
		outcomes = append(outcomes, protectOutcome{ID: id, Status: protectedStatus{Status: "rekeyed"}})
	}
	for _, entry := range entries {
		status, _ := protectRekeyOnly(cmd, l, entry, jsonMode)
		outcomes = append(outcomes, protectOutcome{ID: entry.ID, Status: status})
	}
	for _, e := range unresolved {
		printProtectUnresolved(jsonMode, e.ID)
		outcomes = append(outcomes, protectOutcome{ID: e.ID, Status: protectedStatus{
			Status: "unresolved", Error: errUnresolvedRekey(e.ID).Error(), Unresolved: true}})
	}

	// Mirrors consolidate's own rule exactly (cash_consolidate.go): more
	// than one outcome, or the one outcome succeeded, gets the result
	// printed. A single failure prints nothing on stdout — its classified
	// error on stderr already says everything, matching AGENTS.md's "never
	// both a success shape and a failure shape on the same stream" rule,
	// and this is the one case that must stay byte-identical to before
	// this change: an explicit --token that fails.
	// Holdings left out as unresolved are always listed, so a caller sees why.
	if len(outcomes) > 1 || outcomes[0].Status.Status == "rekeyed" || len(unresolved) > 0 {
		printProtectOutcomes(jsonMode, outcomes)
	}
	return firstProtectOutcomeError(cmd, outcomes)
}

// protectOutcome is one resolved entry's attempt, for wallet protect's own
// multi-entry report. Mirrors consolidateOutcome's reasoning exactly: each
// entry is its own separately committed cash_transfer call (protectRekeyOnly),
// so the states are genuinely per-entry and a single error return cannot
// describe a run of more than one.
type protectOutcome struct {
	ID     string
	Status protectedStatus
}

// classifyProtectError turns one failed outcome into the same classified
// error runWalletProtect has always returned for a single failure —
// LikelyWrongSecret/PendingSecretUnresolved/generic, in that order. Factored
// out so firstProtectOutcomeError can apply it to whichever outcome is first
// to fail, not only ever the sole one.
func classifyProtectError(cmd *cobra.Command, o protectOutcome) error {
	statusErr := errors.New(o.Status.Error)
	switch {
	case o.Status.Recovery != "":
		return output.RecoveryError(cmd, statusErr, o.Status.Recovery)
	case o.Status.Unresolved:
		// Not retryable as is: only the Hub settling the earlier call helps.
		return output.FinalConflictError(cmd, o.ID, statusErr)
	case o.Status.LikelyWrongSecret:
		return output.InvalidInputError(cmd, o.ID, statusErr)
	case o.Status.PendingSecretUnresolved:
		return output.NetworkError(cmd, statusErr)
	default:
		return output.RuntimeError(cmd, statusErr)
	}
}

// firstProtectOutcomeError returns the first failed outcome's own classified
// error, or nil if every outcome rekeyed — mirrors firstOutcomeError
// (cash_consolidate.go): the FIRST failure drives the exit code, and no later
// outcome, success or otherwise, changes that.
func firstProtectOutcomeError(cmd *cobra.Command, outcomes []protectOutcome) error {
	for _, o := range outcomes {
		if o.Status.Status != "rekeyed" {
			return classifyProtectError(cmd, o)
		}
	}
	return nil
}

// printProtectOutcomes renders the whole run under --json.
//
// A single outcome that rekeyed keeps today's exact shape
// ({"protected": {...}}, no "id" field — a caller who named one explicit
// --token already knows which entry this is) so no existing consumer sees
// anything different. Every other case — more than one outcome, since a
// single failure never reaches this function at all (see runWalletProtect)
// — is the array shape, where "id" is what makes each row attributable.
//
// Human mode needs no separate branch here: protectRekeyOnly already prints
// "Protected."/its own failure line as each entry is processed, the same way
// it always has for the single-entry case, so looping just makes that happen
// once per entry — nothing to print centrally without doubling it.
func printProtectOutcomes(jsonMode bool, outcomes []protectOutcome) {
	if !jsonMode {
		return
	}
	if len(outcomes) == 1 && outcomes[0].Status.Status == "rekeyed" {
		output.PrintJSON(map[string]any{"protected": outcomes[0].Status.json()})
		return
	}
	rows := make([]map[string]any, len(outcomes))
	for i, o := range outcomes {
		row := o.Status.json()
		row["id"] = o.ID
		rows[i] = row
	}
	output.PrintJSON(map[string]any{"protected": rows})
}

// resolveUnprotectedCashHoldings is wallet protect's own entry resolver —
// mirrors resolveHeldToken's shape (cash_redeem.go: --token flag, then
// positional, then auto-pick-if-one/prompt-if-many) but scoped to exactly
// what this command can act on: a cash-mode holding still
// ledger.CashShared. An explicit --token/positional naming anything else
// (already protected, pubkey-mode, not held at all) gets a specific
// reason, not a generic "not found."
//
// A holding with a re-key, move or send still pending is never re-keyed: named
// explicitly, it is refused (conflict, not retryable); with nothing named, it
// is left out of the pick and returned in unresolved, to be reported.
//
// Always returns at least one entry on success. With nothing named and more
// than one eligible, returns every one pickHeldTokensOrAll resolves (all of
// them under --json/--yes, or the caller's own explicit pick interactively)
// — protecting keeps the value yours either way, so unlike redeem's own
// resolver there is no ambiguity here worth refusing over.
func resolveUnprotectedCashHoldings(cmd *cobra.Command, l *ledger.Ledger, args []string) (entries, unresolved []*ledger.Entry, err error) {
	id := protectNamedID(cmd, args)
	if id != "" {
		e, ok := l.Find(id)
		if !ok {
			return nil, nil, output.NotFoundError(cmd, id, fmt.Errorf("no held token %q", id))
		}
		if e.CashProtection != ledger.CashShared {
			return nil, nil, output.ConflictError(cmd, id, errors.New(unprotectableReason(e)))
		}
		if hasPendingSecret(e) {
			return nil, nil, output.FinalConflictError(cmd, id, errUnresolvedRekey(id))
		}
		return []*ledger.Entry{e}, nil, nil
	}

	var eligible []ledger.Entry
	for _, e := range l.Held() {
		if e.CashProtection != ledger.CashShared {
			continue
		}
		if hasPendingSecret(&e) {
			if live, ok := l.Find(e.ID); ok {
				unresolved = append(unresolved, live)
			}
			continue
		}
		eligible = append(eligible, e)
	}
	if len(eligible) == 0 {
		if len(unresolved) > 0 {
			return nil, unresolved, nil
		}
		return nil, nil, output.NotFoundError(cmd, "", errors.New("no unprotected cash-mode holdings — nothing to protect (see `cashctl wallet show`)"))
	}

	picked, err := pickHeldTokensOrAll(cmd, eligible)
	if err != nil {
		return nil, nil, err
	}
	// Re-resolve each by ID against l itself (not pickHeldTokensOrAll's own
	// copies) so every returned pointer is the live entry — same reasoning
	// as resolveHeldToken's own doc comment.
	out := make([]*ledger.Entry, 0, len(picked))
	for _, p := range picked {
		e, ok := l.Find(p.ID)
		if !ok {
			return nil, nil, output.NotFoundError(cmd, p.ID, fmt.Errorf("no held token %q", p.ID))
		}
		out = append(out, e)
	}
	return out, unresolved, nil
}

// protectNamedID is the holding wallet protect was pointed at: --token, else
// the positional id, else "".
func protectNamedID(cmd *cobra.Command, args []string) string {
	id, _ := cmd.Flags().GetString("token")
	if id == "" && len(args) > 0 {
		id = args[0]
	}
	return id
}

func unprotectableReason(e *ledger.Entry) string {
	if e.CashProtection == ledger.CashProtected {
		return fmt.Sprintf("%q is already protected", e.ID)
	}
	return fmt.Sprintf("%q isn't a cash-mode holding — protecting only applies to a shared cash secret", e.ID)
}
