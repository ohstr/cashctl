package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

func newCashStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "status",
		// The old name stays as an alias. The METHOD alias `list_recipients` was
		// removed from the wire (nothing had been minted against it), but a CLI name
		// is a different surface: it appears in scripts, in skills/cashctl-cash and
		// in AGENTS.md, and breaking it buys nothing. The command is renamed because
		// `cash_status` is now the only method it calls, so the old name described
		// something that no longer exists.
		Aliases: []string{"list-recipients"},
		Short:   "Check your allocation and co-recipients of a held token",
		Long: `Shows your share and co-recipients of a held token's mint batch.

With no id and more than one held, reports every one — nothing here can
be lost by an ambiguous pick.`,
		Example: `  cashctl cash status`,
		Args:    output.NoArgs,
		RunE:    runCashStatus,
	}
	cmd.Flags().String("token", "", "which held token (auto-picked if you only hold one)")
	return cmd
}

func runCashStatus(cmd *cobra.Command, _ []string) error {
	if err := rejectConnectionFlag(cmd); err != nil {
		return err
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	l, err := ledger.Load()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}

	entries, err := resolveHeldTokensOrAll(cmd, l)
	if err != nil {
		return err
	}

	outcomes := make([]cashStatusOutcome, len(entries))
	for i, entry := range entries {
		result, fetchErr := fetchCashStatus(cmd, jsonMode, entry)
		outcomes[i] = cashStatusOutcome{ID: entry.ID, Result: result, Err: fetchErr}
	}

	printCashStatusOutcomes(jsonMode, outcomes)
	return firstCashStatusError(outcomes)
}

// cashStatusOutcome is one resolved entry's own cash_status call. Mirrors
// consolidateOutcome/protectOutcome's own reasoning: each entry is its own
// independent, separately-failable network call, so the states are
// genuinely per-entry and a single error return cannot describe a run of
// more than one.
type cashStatusOutcome struct {
	ID     string
	Result *nipcash.CashStatusResult
	Err    error
}

// fetchCashStatus is runCashStatus's own single-entry wire call, factored out
// so it can run once per resolved entry — unchanged from what this command
// has always done for its one-entry case.
func fetchCashStatus(cmd *cobra.Command, jsonMode bool, entry *ledger.Entry) (*nipcash.CashStatusResult, error) {
	var result *nipcash.CashStatusResult
	var dialErr bool
	err := WithSpinner(jsonMode, "Fetching recipients...", func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		client, cErr := nipcashclient.Connect(ctx, entry.Token)
		if cErr != nil {
			dialErr = true
			return cErr
		}
		defer client.Close()
		cred, credErr := resolveCredential(cmd, entry)
		if credErr != nil {
			return credErr
		}
		// ScopeAll: `cash status` exists to show the co-recipients, which is
		// the one place asking for the shared roster is the point.
		r, cErr := client.CashStatus(ctx, cred, nipcash.ScopeAll)
		if cErr != nil {
			return cErr
		}
		result = r
		return nil
	})
	if err != nil {
		if dialErr {
			return nil, output.NetworkError(cmd, err)
		}
		return nil, classifyCashTokenNWCErr(cmd, err)
	}
	return result, nil
}

// firstCashStatusError returns the first failed outcome's own already-
// classified error, or nil if every fetch succeeded — same contract as
// firstOutcomeError/firstProtectOutcomeError: the FIRST failure drives the
// exit code, and no later outcome changes that.
func firstCashStatusError(outcomes []cashStatusOutcome) error {
	for _, o := range outcomes {
		if o.Err != nil {
			return o.Err
		}
	}
	return nil
}

// printCashStatusOutcomes renders the whole run, in both modes.
//
// A single outcome that succeeded keeps today's exact --json shape —
// output.PrintJSON(result) directly, the bare CashStatusResult with no
// wrapper at all — so no existing consumer sees anything different. Every
// other case (more than one outcome, or the one outcome failed) wraps in a
// named "statuses" array, where "id" is what makes each row attributable; a
// single failure still prints nothing on stdout (its classified error on
// stderr already says everything, matching the same rule protect's own
// printProtectOutcomes follows).
//
// Human mode mirrors the single-entry rendering this command has always
// used, once per entry, with a one-line header naming which entry when
// there is more than one — amount and received date, never the entry's own
// ID (plain text deliberately never prints one, same rule pickHeldToken's
// own listing follows).
func printCashStatusOutcomes(jsonMode bool, outcomes []cashStatusOutcome) {
	if jsonMode {
		if len(outcomes) == 1 && outcomes[0].Err == nil {
			output.PrintJSON(outcomes[0].Result)
			return
		}
		if len(outcomes) == 1 {
			return // single failure: nothing on stdout, see doc comment above
		}
		rows := make([]map[string]any, len(outcomes))
		for i, o := range outcomes {
			if o.Err != nil {
				ce := output.AsCLIError(o.Err)
				errMsg := ce.Err.Error()
				if ce.RawMessage != "" {
					errMsg = ce.RawMessage
				}
				row := map[string]any{"id": o.ID, "status": "failed", "error": errMsg, "code": string(ce.Code)}
				if ce.NWCCode != "" {
					row["nwc_code"] = ce.NWCCode
				}
				rows[i] = row
				continue
			}
			o.Result.Recipients = output.NonNil(o.Result.Recipients)
			rows[i] = map[string]any{"id": o.ID, "status": "ok", "result": o.Result}
		}
		output.PrintJSON(map[string]any{"statuses": rows})
		return
	}

	for i, o := range outcomes {
		if len(outcomes) > 1 {
			output.Println(cashStatusHeader(i+1, len(outcomes)))
		}
		if o.Err != nil {
			// Already a classified *CLIError from fetchCashStatus; printed
			// here rather than deferred to EmitError because a run of
			// several must show every outcome, not just the first failure
			// (see firstCashStatusError, which still drives the exit code).
			output.Printf("Error: %v\n", output.AsCLIError(o.Err).Err)
			continue
		}
		printCashStatusResult(o.Result)
	}
}

// cashStatusHeader is the one place this command identifies which entry a
// block of output is about, in text mode, with more than one in play — a
// plain ordinal, never the entry's own ID.
func cashStatusHeader(i, n int) string {
	return fmt.Sprintf("Token %d of %d:", i, n)
}

// printCashStatusResult is the single-entry rendering this command has
// always done — unchanged, just factored out so it runs once per entry
// instead of once per invocation.
func printCashStatusResult(result *nipcash.CashStatusResult) {
	// A tombstone before the roster, because a spent bill HAS no roster and
	// would otherwise render as zero bytes — silence, which is exactly what
	// this command is asked to distinguish from.
	//
	// This is the CLI's own prescribed recovery step: after a redeem whose
	// reply never arrived, applyRedeemResults tells the user to run
	// `cashctl cash status --token <id>` to find out whether their money
	// moved. Printing nothing answered that question with silence, and the
	// answer was available — the Hub retains a deleted bill precisely so its
	// holder gets a definitive "spent" instead of having to guess. Past
	// retention the Hub does fall silent, and explainNoAnswer covers that
	// well; the gap was the window where the Hub tells the truth.
	if result.IsSpent() {
		output.Println("This bill is spent — its value has already moved, and the bill itself is gone.")
		if result.RetainedUntil != nil {
			output.Printf("The Hub will keep answering about it until %s; after that it goes silent.\n",
				time.Unix(*result.RetainedUntil, 0).UTC().Format("2006-01-02 15:04 UTC"))
		}
		return
	}

	for _, r := range result.Recipients {
		status := "unclaimed"
		if r.Claimed {
			status = "claimed"
			if r.ClaimedAt != nil {
				status = fmt.Sprintf("claimed %s", time.Unix(*r.ClaimedAt, 0).UTC().Format("2006-01-02"))
			}
		}
		// Sanitized: identity_type/identity_value are unvalidated
		// wire strings from the Hub, not cashctl's own text.
		identity := output.Sanitize(r.IdentityType)
		if r.IdentityValue != "" {
			identity = fmt.Sprintf("%s:%s", output.Sanitize(r.IdentityType), output.Sanitize(r.IdentityValue))
		}
		output.Printf("%-40s %14s   %s\n", identity, output.FormatAmount(int64(r.AmountMillis)), status)
	}
	// ExpiresAt is identical on every row (NIP-CASH §Listing Recipients:
	// one shared wallet-level deadline) — shown once, after the roster,
	// same wording as decode --check's own cash token report (decode.go's
	// formatExpiry). Unlike a money-moving confirmation
	// (redeem/transfer/consolidate), this is pure inspection with nothing
	// to gate, so it's always shown, not just when it's close.
	if len(result.Recipients) > 0 && result.Recipients[0].ExpiresAt != nil {
		output.Println(formatExpiry(*result.Recipients[0].ExpiresAt))
	}
}

// resolveHeldTokensOrAll is cash status's own multi-entry resolver.
// Deliberately NOT a change to resolveHeldToken (cash_redeem.go), which
// `transfer`'s own bare-no-amount path also depends on and must keep
// refusing an ambiguous pick — transfer pays value out, status doesn't. The
// explicit-id branch duplicates resolveHeldToken's own spent-bill check
// rather than sharing it, the same way pickHeldToken/pickHeldTokens already
// accept some duplication for clarity over entanglement.
func resolveHeldTokensOrAll(cmd *cobra.Command, l *ledger.Ledger) ([]*ledger.Entry, error) {
	if id, _ := cmd.Flags().GetString("token"); id != "" {
		e, ok := l.Find(id)
		if !ok {
			return nil, output.NotFoundError(cmd, id, fmt.Errorf("no held token %q", id))
		}
		if spent := spentStatusDescription(e.Status); spent != "" {
			return nil, output.NotFoundError(cmd, id,
				fmt.Errorf("token %q was already %s, so it no longer exists on the Hub", id, spent))
		}
		return []*ledger.Entry{e}, nil
	}

	held := l.Held()
	if len(held) == 0 {
		return nil, output.NotFoundError(cmd, "", fmt.Errorf("you have no held cash tokens — receive one first with `cashctl receive <token>`"))
	}
	picked, err := pickHeldTokensOrAll(cmd, held)
	if err != nil {
		return nil, err
	}
	out := make([]*ledger.Entry, 0, len(picked))
	for _, p := range picked {
		e, ok := l.Find(p.ID)
		if !ok {
			return nil, output.NotFoundError(cmd, p.ID, fmt.Errorf("no held token %q", p.ID))
		}
		out = append(out, e)
	}
	return out, nil
}

// joinStrings renders a token/connection's own relay URLs for display.
// Sanitized: a relay URL has no character restrictions of its own — an
// attacker-crafted one could otherwise carry a terminal escape sequence.
func joinStrings(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += output.Sanitize(s)
	}
	return out
}
