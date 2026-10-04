package cmd

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/credential"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

func newCashConsolidateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "consolidate [id...]",
		Short: "Merge several held cash tokens into one",
		Long: `Merges several held cash tokens into one. With no IDs/--sources,
auto-groups held tokens by Cash Hub and merges each group — only
same-Hub tokens can combine. Pass IDs/--sources for exact control; see
"cashctl wallet show --json" for the IDs (plain-text output never prints
them).`,
		Example: `  cashctl consolidate
  cashctl consolidate tok-a tok-b
  cashctl consolidate --sources tok-a,tok-b --to cash`,
		Args: cobra.ArbitraryArgs,
		RunE: runCashConsolidate,
	}
	cmd.Flags().String("sources", "", fmt.Sprintf("comma-separated: local ledger IDs, or <token>:<amount-%s>:<credential>", output.CurrencyUnit))
	cmd.Flags().String("to", "", "defaults to your own identity")
	cmd.Flags().String("ia", "", "Identity Authority to trust (hex pubkey or name@domain) for an nconnection1... target that doesn't specify one")
	return cmd
}

// consolidateResult is one completed cash_consolidate call's outcome —
// runCashConsolidate collects one per minter group it actually processed
// (almost always exactly one) so the explicit-sources path and the
// auto-detected, possibly-multi-group path can share the same result
// rendering.
type consolidateResult struct {
	NewEntry       *ledger.Entry
	ExpiresAt      *int64
	TargetResolved string
}

// consolidateOutcome is what happened to ONE minter group, and it has three
// states rather than two — mirroring the SDK's own ItemOutcome, for the same
// reason. A group either merged (Result set), was declined by the person at the
// prompt (both nil), or failed (Err set). Collapsing declined into failed would
// report a deliberate choice as a fault; collapsing failed into "nothing
// happened" would hide that the Hub may have moved money.
//
// This exists because each group is a separate committed call: the states are
// genuinely per-group, so a single error return cannot describe the run.
type consolidateOutcome struct {
	// Hub is the group's shared Cash Hub fingerprint — the thing that MADE it a
	// group, and the only stable way to name it in output, since the ledger IDs
	// of a failed group are not otherwise interesting to a caller.
	//
	// Was the shared MINTER pubkey, which became actively misleading once grouping
	// moved to the Hub: one node runs several Hubs, so two different groups would
	// have reported the same minter and a reader could not tell them apart.
	Hub string
	// IDs are the source ledger entries this group tried to merge.
	IDs []string
	// Result is set only when the merge completed.
	Result *consolidateResult
	// Err is set only when it failed. A group with neither is a decline.
	Err error
}

// declined reports the third state: attempted, and the person said no.
func (o consolidateOutcome) declined() bool { return o.Result == nil && o.Err == nil }

// printConsolidateOutcomes renders a whole multi-group run, successes and
// failures together.
//
// The single-successful-group case deliberately keeps printConsolidateResults'
// original top-level {"new_entry", "expires_at", "target_resolved"} JSON shape,
// because that is what every existing --json consumer parses (integration/ reads
// consolidateResp["new_entry"] directly). The richer per-group array is used only
// when there is more than one outcome or one of them did not succeed — states in
// which this command previously returned an error and printed no result at all,
// so no consumer can be depending on the old shape there.
func printConsolidateOutcomes(jsonMode bool, outcomes []consolidateOutcome) {
	succeeded := make([]*consolidateResult, 0, len(outcomes))
	for _, o := range outcomes {
		if o.Result != nil {
			succeeded = append(succeeded, o.Result)
		}
	}
	allOK := len(succeeded) == len(outcomes)

	if jsonMode {
		if len(outcomes) == 1 && allOK {
			printConsolidateResults(true, succeeded)
			return
		}
		out := make([]map[string]any, len(outcomes))
		for i, o := range outcomes {
			row := map[string]any{"hub": o.Hub, "sources": o.IDs}
			switch {
			case o.Result != nil:
				row["status"] = "ok"
				row["new_entry"] = o.Result.NewEntry
				row["expires_at"] = o.Result.ExpiresAt
				row["target_resolved"] = o.Result.TargetResolved
			case o.declined():
				row["status"] = "declined"
			default:
				// Classified the same way a fatal error would have been, so a
				// script branches on one vocabulary whether a failure was the
				// only group or one of several.
				ce := output.AsCLIError(o.Err)
				row["status"] = "failed"
				row["error"] = ce.Err.Error()
				if ce.RawMessage != "" {
					row["error"] = ce.RawMessage
				}
				row["code"] = string(ce.Code)
				if ce.NWCCode != "" {
					row["nwc_code"] = ce.NWCCode
				}
			}
			out[i] = row
		}
		output.PrintJSON(map[string]any{"consolidated": out})
		return
	}

	for _, o := range outcomes {
		switch {
		case o.Result != nil:
			output.Printf("Consolidated into one %s note, saved to your wallet.\n", output.FormatAmount(int64(*o.Result.NewEntry.AmountMillis)))
		case o.declined():
			// consolidateItems already printed "Cancelled." for this group.
		default:
			// Named by Hub and source count: with several groups in play,
			// "it failed" without saying which one is not actionable.
			output.Printf("Failed to consolidate %d tokens from Cash Hub %s: %v\n",
				len(o.IDs), output.Sanitize(shortHub(o.Hub)), output.AsCLIError(o.Err).Err)
		}
	}
	// Only when the run was genuinely mixed. A single failure speaks for itself
	// through the returned error's own "Error:" line, and repeating a count of
	// one would be noise.
	if len(outcomes) > 1 && !allOK {
		output.Printf("%d of %d groups consolidated.\n", len(succeeded), len(outcomes))
	}
}

// shortHub trims a Hub fingerprint to something readable in a line of output, the
// way the rest of the CLI abbreviates keys. A fingerprint is already short, so this
// usually returns it unchanged.
func shortHub(hub string) string {
	if len(hub) <= 12 {
		return hub
	}
	return hub[:12] + "…"
}

func runCashConsolidate(cmd *cobra.Command, args []string) error {
	if err := rejectConnectionFlag(cmd); err != nil {
		return err
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	sourcesFlag, _ := cmd.Flags().GetString("sources")
	toFlag, _ := cmd.Flags().GetString("to")
	yesFlag, _ := cmd.Flags().GetBool("yes")

	l, err := ledger.Load()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}

	if len(args) > 0 || sourcesFlag != "" {
		items, err := resolveConsolidateSources(args, sourcesFlag, l.Held())
		if err != nil {
			return output.InvocationError(cmd, err)
		}
		result, err := consolidateItems(cmd, l, items, toFlag, jsonMode, yesFlag)
		if err != nil {
			return err
		}
		var results []*consolidateResult
		if result != nil {
			results = []*consolidateResult{result}
		}
		printConsolidateResults(jsonMode, results)
		return nil
	}

	// No explicit sources: naively merging every held token together isn't
	// possible — NIP-CASH sources are minter-scoped (cash_consolidate only
	// accepts same-minter sources) — so auto-detect which minter's tokens
	// can actually be merged instead (see mergeableHubGroups) and
	// process each such group, rather than erroring out and making the
	// common "tidy up my dust" case require first hunting down IDs.
	groups := mergeableHubGroups(l.Held())
	if len(groups) == 0 {
		if jsonMode {
			output.PrintJSON(map[string]any{"consolidated": []any{}})
		} else {
			output.Println("Nothing to consolidate — no minter has more than one held token.")
		}
		return nil
	}

	chosen, err := pickHubGroups(cmd, groups)
	if err != nil {
		return err
	}

	outcomes := consolidateGroups(cmd, l, chosen, toFlag, jsonMode, yesFlag)
	// A single group that failed is fully described by its own error on stderr,
	// which is exactly what this command has always printed for it — so no
	// result document is emitted there, keeping the single-group failure path
	// unchanged. With more than one group, or any group that actually merged,
	// the error alone cannot describe the run.
	if len(outcomes) > 1 || (len(outcomes) == 1 && outcomes[0].Err == nil) {
		printConsolidateOutcomes(jsonMode, outcomes)
	}

	// Reported first, then failed. The full picture is already on stdout in both
	// modes, so the returned error only has to drive the exit code.
	return firstOutcomeError(outcomes)
}

// consolidateGroups attempts every chosen minter group and returns one outcome
// per group, in order, whatever happened.
//
// A failure in one group never stops the others or hides their outcome. This
// used to `return err` on the first failure, which was wrong in the one way that
// costs a user real information: each group is its own separately committed
// cash_consolidate call, so failing on group 2 of 3 left group 1 ALREADY MERGED
// on the Hub, group 3 never attempted, and only group 2's error printed. The
// caller could not tell which of those three states each group was in, and the
// new token group 1 had just produced went unreported — a token that exists,
// holds real value, and was never named in the output.
//
// A decline is likewise this group's own call to make (both fields nil,
// reachable only outside --json/--yes — see Confirm) and does not cancel the
// rest.
func consolidateGroups(cmd *cobra.Command, l *ledger.Ledger, chosen [][]ledger.Entry, toFlag string, jsonMode, yesFlag bool) []consolidateOutcome {
	outcomes := make([]consolidateOutcome, 0, len(chosen))
	for _, group := range chosen {
		ids := make([]string, len(group))
		for i, e := range group {
			ids[i] = e.ID
		}
		o := consolidateOutcome{IDs: ids}
		if len(group) > 0 {
			o.Hub = ledger.HubGroupKey(group[0])
		}
		o.Result, o.Err = consolidateItemsFn(cmd, l, ids, toFlag, jsonMode, yesFlag)
		if o.Err != nil && isCrossHubDeclineClassified(o.Err) {
			o.Err = crossHubGroupError(cmd, ids)
		}
		outcomes = append(outcomes, o)
	}
	return outcomes
}

// isCrossHubDeclineClassified recognises the Hub's "same Cash Hub" refusal AFTER
// it has been classified.
//
// isCrossHubSourcesDecline cannot be used here, and the difference is easy to miss:
// it matches a raw *relayclient.WalletError, which works at a call site that sees
// the error straight off the wire (cash_transfer's does). consolidateItemsFn has
// already run it through output.NWCErrorForCashToken by the time it returns, and
// that builds a fresh CLIError around a plainError — the WalletError is not in the
// chain any more, so errors.As can never find it and the match silently never
// fires. The raw text survives only on CLIError.RawMessage, so that is what this
// reads.
func isCrossHubDeclineClassified(err error) bool {
	if isCrossHubSourcesDecline(err) {
		return true
	}
	ce := output.AsCLIError(err)
	if ce == nil {
		return false
	}
	return strings.Contains(strings.ToLower(ce.RawMessage), "same cash hub") ||
		strings.Contains(strings.ToLower(ce.Err.Error()), "same cash hub")
}

// crossHubGroupError replaces the Hub's own "all sources must belong to the same
// Cash Hub" with something a person can act on.
//
// Now a DEFENCE rather than the common path. The auto-grouped path used to group by
// MINTER — the Lightning node that signed the mint, not the Hub that issued the bill
// — and since one node routinely runs several Hubs, bills from different Hubs landed
// in one group and the Hub refused it. Grouping by the token's own hub-group
// fingerprint (ledger.GroupByHub) removed that whole class, so reaching this now
// means a group was built wrongly.
//
// Kept because the refusal must stay legible if it ever happens again: a bill whose
// fingerprint is absent or wrong, a future grouping change, a Hub that re-parented a
// bill. The raw refusal reads as a protocol error the user caused, when the selection
// was made for them.
//
// Classified invalid_input, matching the Hub: the selection genuinely cannot be
// merged as given, and nothing moved.
func crossHubGroupError(cmd *cobra.Command, ids []string) error {
	return output.InvalidInputError(cmd, strings.Join(ids, ","), fmt.Errorf(
		"these bills (%s) were minted by the same Lightning node but not by the same Cash Hub, "+
			"and a consolidation can only merge bills of one Hub. Nothing was changed. "+
			"Consolidate one Hub's bills at a time with --sources, e.g. --sources %s",
		strings.Join(ids, ","), strings.Join(ids[:1], ",")))
}

// consolidateItemsFn is a package-var seam so the group loop's own policy —
// attempt everything, report everything — is testable without a ledger file or a
// network call, the same pattern attemptCashConsolidateFn already uses below.
var consolidateItemsFn = consolidateItems

// firstOutcomeError returns the first failure's own error, or nil.
//
// Deliberately the original error, neither wrapped nor joined, so its
// classification, retryable flag and any wallet-side NWC code survive exactly as
// they would have for a single group. Wrapping it to say "partial" would be
// discarded anyway: AsCLIError unwraps to the inner *CLIError and prints that, so
// a wrapper's text never reaches the user. The per-group detail that a wrapper
// would have carried is on stdout instead, where it can be complete.
func firstOutcomeError(outcomes []consolidateOutcome) error {
	for _, o := range outcomes {
		if o.Err != nil {
			return o.Err
		}
	}
	return nil
}

// consolidateItems builds nipcash.Sources from items (local ledger IDs, or
// verbose <token>:<amount>:<credential> entries), confirms once, and
// executes a single cash_consolidate call. Returns (nil, nil) on a
// declined confirmation — not an error, the same way
// transferWithAutoConsolidate's own decline path returns cleanly
// (cash_transfer.go) — printing "Cancelled." itself so a caller loading
// several groups doesn't need to special-case which one(s) a human
// declined.
func consolidateItems(cmd *cobra.Command, l *ledger.Ledger, items []string, toFlag string, jsonMode, yesFlag bool) (*consolidateResult, error) {
	var sources []nipcash.Source
	// sourceTokens is parallel to sources: one raw token string per source,
	// whichever produced it (a local entry's own token, or the verbose
	// <token>:...  form's own token). Any of them can dial the actual
	// cash_consolidate call (see doCashConsolidate's own doc comment) —
	// this also fixes a latent index-out-of-range panic on an
	// all-verbose-sources call, which had no local ID to dial through at
	// all before this (l.Find(localIDs[0]) with an empty localIDs).
	var sourceTokens []string
	var total uint64
	var localIDs []string
	var earliestExpiresAt *int64
	var expiredSources, healthySources int
	// expiredIDs names which source(s) are confirmed already-expired —
	// a local entry's own ID, or (no ID to name) a verbose source's
	// redacted item string — for the hard refusal below. Never raw: a
	// verbose entry's string carries a credential's secret component,
	// same as the other error paths in this loop (output.RedactSecretInput
	// at the verbose branch's own cred-parse failure, right below).
	var expiredIDs []string
	err := WithSpinner(jsonMode, "Checking sources...", func() error {
		for _, item := range items {
			item = strings.TrimSpace(item)
			if e, ok := l.Find(item); ok {
				// Find returns an entry whatever its status, so an explicit
				// source could name a bill we already spent. Dialling one
				// hangs and then reports a retryable network failure, telling
				// an agent to retry something that can never succeed — the
				// same trap cash_redeem.go's own --token path documents and
				// guards. The auto-detect path picks from l.Held() and so
				// cannot reach this.
				if spent := spentStatusDescription(e.Status); spent != "" {
					return output.NotFoundError(cmd, e.ID,
						fmt.Errorf("token %q was already %s, so it no longer exists on the Hub", e.ID, spent))
				}
				src, err := sourceFromEntry(cmd, l, e)
				if err != nil {
					return err
				}
				sources = append(sources, src)
				sourceTokens = append(sourceTokens, e.Token)
				total += src.Amount
				localIDs = append(localIDs, e.ID)
				// Unconditional, not gated on !jsonMode && !yesFlag like the
				// cosmetic "expires soon" warning below still is: this feeds
				// the hard mixed-expiry refusal after this loop, which must
				// fire under --json/--yes too — that's exactly the mode a
				// scripted/agentic merge would otherwise poison a healthy
				// source in with zero signal. src.Credential is already
				// resolved above, so this probes with the real credential
				// rather than fetchExpiresAt's own local-identity guess.
				if ea := fetchExpiresAtWithCredential(cmd, e.Token, src.Credential); ea != nil {
					earliestExpiresAt = earliestExpiry(earliestExpiresAt, ea)
					if time.Until(time.Unix(*ea, 0)) <= 0 {
						expiredSources++
						expiredIDs = append(expiredIDs, e.ID)
					} else {
						healthySources++
					}
				}
				continue
			}

			parts := strings.SplitN(item, ":", 3)
			if len(parts) != 3 {
				return output.InvalidInputError(cmd, item, fmt.Errorf("not a held token ID, and not a valid <token>:<amount>:<credential> entry"))
			}
			amount, err := output.ParseAmount(parts[1])
			if err != nil {
				return output.InvalidInputError(cmd, item, fmt.Errorf("invalid amount %q — must be a valid amount in loki", parts[1]))
			}
			cred, err := credential.ParseCash(parts[2])
			if err != nil {
				return output.InvalidInputError(cmd, output.RedactSecretInput(item), err)
			}
			tok, err := decodeCashTokenForConsolidate(parts[0])
			if err != nil {
				return output.InvalidInputError(cmd, parts[0], err)
			}
			sources = append(sources, nipcash.Source{WalletPubkey: tok, Amount: amount, Credential: cred})
			sourceTokens = append(sourceTokens, parts[0])
			total += amount
			// Same probe, same reasoning as the local-entry branch above —
			// this verbose form had no expiry awareness at all before this,
			// an obvious way to route around the refusal below otherwise.
			if ea := fetchExpiresAtWithCredential(cmd, parts[0], cred); ea != nil {
				earliestExpiresAt = earliestExpiry(earliestExpiresAt, ea)
				if time.Until(time.Unix(*ea, 0)) <= 0 {
					expiredSources++
					expiredIDs = append(expiredIDs, output.RedactSecretInput(item))
				} else {
					healthySources++
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(sources) < 2 {
		return nil, output.UsageError(cmd, fmt.Errorf("consolidate needs at least 2 sources, got %d", len(sources)))
	}
	if expiredSources > 0 && healthySources > 0 {
		// Hard refusal, not just a warning: NIP-CASH's merge rule inherits
		// the EARLIEST expiry across every source (§Consolidating Tokens),
		// so merging an already-expired source into a healthy one doesn't
		// just fail — it silently kills the healthy source's own good
		// deadline too, the instant the merge lands. Before target
		// resolution and before Confirm: nothing has moved yet, so "Nothing
		// was changed" is trivially true. Deliberately does NOT fire when
		// every source is expired (healthySources == 0) — that's the Hub's
		// own exit-7/auth/EXPIRED decline, already correct, left alone.
		return nil, output.InvalidInputError(cmd, strings.Join(expiredIDs, ","), fmt.Errorf(
			"%s already expired — merging with a healthy source would make the whole result "+
				"unusable too. Nothing was changed. Drop it and retry with just the healthy source(s)",
			strings.Join(expiredIDs, ",")))
	}

	var target nipcash.Target
	var targetResolved string
	if toFlag != "" {
		rt, err := resolveTarget(cmd, toFlag)
		if err != nil {
			return nil, err
		}
		target = rt.Target
		targetResolved = rt.Resolved
		// shouldPrintResolvedTarget (cash_transfer.go): a cash-mode target's
		// Resolved carries the freshly generated secret itself — printing
		// it here, before anything is confirmed, is the exact cash-mode-
		// target-secret-before-confirm bug already found and fixed for
		// transfer's own "resolves to:" line. Still returned in --json's
		// target_resolved either way, same as transfer.
		if shouldPrintResolvedTarget(rt) {
			output.Notef(jsonMode, "  resolves to: %s", targetResolved)
		}
	} else {
		myPub, err := localPubKeyHex(cmd)
		if err != nil {
			return nil, output.RuntimeError(cmd, err)
		}
		target = nipcash.Pubkey(myPub)
	}

	message := fmt.Sprintf("Consolidate %d tokens (%s) into one%s?",
		len(sources), output.FormatAmount(int64(total)), ifTargetIsSelf(toFlag))
	// Mixed expired+healthy already refused above, unconditionally — this
	// is only the cosmetic "expires soon, not yet" notice, so it keeps its
	// original interactive-only gate: earliestExpiresAt is now always
	// computed (the probe above runs regardless of mode), and without this
	// explicit gate it would start printing under --yes too.
	if !jsonMode && !yesFlag {
		if w := expiryWarningSuffix(earliestExpiresAt, "consolidate"); w != "" {
			output.Notef(jsonMode, "%s", w)
		}
	}
	// defaultYes=false: moves real money — never accept on a bare Enter.
	// Only reachable outside --json/--yes (Confirm auto-accepts under
	// either), so a declined group is always an interactive choice, never
	// something a script/agent needs to handle.
	if !Confirm(cmd, false, message) {
		output.Println("Cancelled.")
		return nil, nil
	}

	var newEntry *ledger.Entry
	var expiresAt *int64
	// toFlag == "" is the same "target is my own identity" signal
	// ifTargetIsSelf's display text above already relies on — an explicit
	// --to <my own pubkey> isn't specially detected here either, matching
	// that existing simplification rather than introducing a new one.
	isSelfTarget := toFlag == ""
	err = WithSpinner(jsonMode, "Consolidating...", func() error {
		var cErr error
		newEntry, expiresAt, cErr = doCashConsolidate(cmd, l, sourceTokens, sources, localIDs, target, isSelfTarget)
		return cErr
	})
	if err != nil {
		return nil, err
	}
	// doCashConsolidate has already mutated l in memory (sources marked
	// consolidated, newEntry added) but never saves it itself — a failure
	// here means the Hub-side merge is real and this is the only place
	// left to say so, or newEntry's own token/secret (the only way to ever
	// reach that money again) is gone the moment this process exits.
	if err := l.Save(); err != nil {
		return nil, reportUnsavedResult(cmd, err, "Consolidate", entryRecoveryHint(newEntry))
	}

	return &consolidateResult{NewEntry: newEntry, ExpiresAt: expiresAt, TargetResolved: targetResolved}, nil
}

// printConsolidateResults renders results — empty when every group
// processed was declined (interactive-only; see consolidateItems' own doc
// comment). The single-result shape matches exactly what runCashConsolidate
// has always printed for its one-call path (JSON keys unchanged, for
// existing scripts); multiple results (auto-detected across more than one
// minter) use a "consolidated" list instead, only ever reached via the
// no-args/no---sources path.
func printConsolidateResults(jsonMode bool, results []*consolidateResult) {
	if jsonMode {
		if len(results) == 1 {
			// new_entry (a ledger.Entry, properly snake_case-tagged) already
			// carries result's token/wallet_pubkey/amount — expires_at is the
			// only field of nipcash.CashConsolidateResult (no JSON tags of its
			// own) worth surfacing separately (see cash_transfer.go's own fix
			// for the same underlying issue).
			r := results[0]
			output.PrintJSON(map[string]any{"new_entry": r.NewEntry, "expires_at": r.ExpiresAt, "target_resolved": r.TargetResolved})
			return
		}
		out := make([]map[string]any, len(results))
		for i, r := range results {
			out[i] = map[string]any{"new_entry": r.NewEntry, "expires_at": r.ExpiresAt, "target_resolved": r.TargetResolved}
		}
		output.PrintJSON(map[string]any{"consolidated": out})
		return
	}
	for _, r := range results {
		output.Printf("Consolidated into one %s note, saved to your wallet.\n", output.FormatAmount(int64(*r.NewEntry.AmountMillis)))
	}
}

// resolveConsolidateSources decides which source items runCashConsolidate
// acts on when the caller gave explicit positional args or --sources
// (comma-separated) — runCashConsolidate only calls this once it's
// already checked at least one of the two is set; with neither given, it
// takes the auto-detected-Hub-groups path instead (mergeableHubGroups/
// pickHubGroups), never this function's own "every held token" fallback
// below (kept for this function's own standalone testability/contract,
// not reachable through the real command anymore). Pure and cobra-free so
// it's unit-testable directly, without a ledger file or a network call —
// mirrors resolvePositionalOrFlag's own reasoning in cmd/positional.go,
// just for a list instead of a single value.
func resolveConsolidateSources(args []string, sourcesFlag string, held []ledger.Entry) ([]string, error) {
	if len(args) > 0 && sourcesFlag != "" {
		return nil, fmt.Errorf("got both positional source IDs and --sources — pass only one")
	}
	if len(args) > 0 {
		return args, nil
	}
	if sourcesFlag != "" {
		return strings.Split(sourcesFlag, ","), nil
	}
	items := make([]string, 0, len(held))
	for _, e := range held {
		items = append(items, e.ID)
	}
	return items, nil
}

// mergeableHubGroups returns held's consolidation-eligible entries
// (ledger.GroupableForConsolidation — pubkey-mode, known issuing Hub, known
// amount), grouped by that Hub's own fingerprint (ledger.GroupByHub),
// restricted to Hubs with 2+ held tokens: a singleton has nothing to merge
// into. Used by runCashConsolidate's no-args/no---sources default instead of
// naively trying to merge every held token together, which isn't possible
// across Hubs (cash_consolidate only accepts sources of one Cash Hub).
//
// Grouped by HUB, not by minter, and the difference is not cosmetic. A mint
// signature names the minting NODE, and one node routinely runs several Hubs,
// so the old same-minter grouping merged bills from sibling Hubs and the Hub
// refused the whole selection — an ordinary `consolidate` failing with a
// protocol error the holder had not caused.
func mergeableHubGroups(held []ledger.Entry) map[string][]ledger.Entry {
	groups := ledger.GroupByHub(ledger.GroupableForConsolidation(held))
	out := make(map[string][]ledger.Entry, len(groups))
	for hub, entries := range groups {
		if len(entries) >= 2 {
			out[hub] = entries
		}
	}
	return out
}

// sharedMinter returns the minter pubkey every one of entries verifiably
// shares, or nil when any of them has none or they differ. This is what a
// token DERIVED from entries (a split's remainder, a consolidate's merged
// output) inherits as its own Entry.MinterPubkey: the derived token is a
// brand-new wallet, so no mint signature can verify against it directly,
// but the same Hub that minted its verified sources created it, and
// cash-selection (mergeableMinterGroups, pickHeldToken) needs that fact to
// keep treating it as spendable together with its siblings. nil, never a
// guess, when the lineage isn't uniformly verified — claiming a minter that
// wasn't confirmed would defeat what the field exists to guarantee.
func sharedMinter(entries []ledger.Entry) *string {
	var minter *string
	for _, e := range entries {
		if e.MinterPubkey == nil {
			return nil
		}
		if minter == nil {
			m := *e.MinterPubkey
			minter = &m
		} else if *minter != *e.MinterPubkey {
			return nil
		}
	}
	return minter
}

// sharedMinterOfIDs is sharedMinter over the ledger entries named by ids;
// nil if any id isn't found.
func sharedMinterOfIDs(l *ledger.Ledger, ids []string) *string {
	entries := make([]ledger.Entry, 0, len(ids))
	for _, id := range ids {
		e, ok := l.Find(id)
		if !ok {
			return nil
		}
		entries = append(entries, *e)
	}
	return sharedMinter(entries)
}

// sortedHubKeys returns groups' minter keys in a stable order, so
// pickHubGroups' numbered list (and its own tests) don't depend on Go's
// randomized map iteration order.
func sortedHubKeys(groups map[string][]ledger.Entry) []string {
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// pickHubGroups decides which of groups' minter clusters
// runCashConsolidate should actually process. Under --json/--yes there's
// no terminal to ask from (and no point asking a question a script can't
// answer) — every qualifying group is processed, matching this command's
// "tidy up my dust" default spirit for scripted/agentic use. A single
// group needs no prompt either way — it's the only sensible choice.
// Otherwise, interactively: shown as a numbered list (mirroring
// pickHeldToken's own style in cash_redeem.go), a bare Enter or "all"
// picks every group, or a comma-separated subset of numbers picks just
// those.
func pickHubGroups(cmd *cobra.Command, groups map[string][]ledger.Entry) ([][]ledger.Entry, error) {
	keys := sortedHubKeys(groups)
	all := func() [][]ledger.Entry {
		picked := make([][]ledger.Entry, len(keys))
		for i, k := range keys {
			picked[i] = groups[k]
		}
		return picked
	}

	jsonMode, _ := cmd.Flags().GetBool("json")
	yesFlag, _ := cmd.Flags().GetBool("yes")
	if jsonMode || yesFlag || len(keys) == 1 {
		return all(), nil
	}

	output.Notef(false, "Found %d separate Cash Hubs you can consolidate:", len(keys))
	for i, k := range keys {
		entries := groups[k]
		output.Notef(false, "  %d) %d tokens (%s)", i+1, len(entries), output.FormatAmount(int64(ledger.SumAmounts(entries))))
	}
	choice, err := PromptLine(fmt.Sprintf("Which one(s)? [1-%d, comma-separated, or Enter for all] ", len(keys)))
	if err != nil {
		return nil, output.RuntimeError(cmd, err)
	}
	choice = strings.TrimSpace(choice)
	if choice == "" || strings.EqualFold(choice, "all") {
		return all(), nil
	}
	var picked [][]ledger.Entry
	for _, part := range strings.Split(choice, ",") {
		idx := parseChoice(strings.TrimSpace(part), len(keys))
		if idx < 0 {
			return nil, output.UsageError(cmd, fmt.Errorf("invalid selection %q", part))
		}
		picked = append(picked, groups[keys[idx]])
	}
	return picked, nil
}

func ifTargetIsSelf(to string) string {
	if to == "" {
		return ", sent to your own identity"
	}
	return ""
}

// decodeCashTokenForConsolidate decodes a bare token string (the verbose
// non-ledger source form) down to just its wallet pubkey.
func decodeCashTokenForConsolidate(token string) (string, error) {
	tok, err := nipcash.Decode(token)
	if err != nil {
		return "", err
	}
	return tok.WalletPubkey, nil
}

// sourceFromEntry resolves e's credential and current amount into a
// nipcash.Source usable in a cash_consolidate call — the local-ledger-
// entry half of runCashConsolidate's own source-building loop, factored
// out so cash selection's auto-consolidate step (cash_transfer.go, see
// docs/ux-review.md Part 2) can build the same shape from a group of
// entries it already knows are all local.
func sourceFromEntry(cmd *cobra.Command, l *ledger.Ledger, e *ledger.Entry) (nipcash.Source, error) {
	cred, err := resolveCredential(cmd, e)
	if err != nil {
		return nipcash.Source{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := nipcashclient.Connect(ctx, e.Token)
	if err != nil {
		return nipcash.Source{}, output.NetworkError(cmd, err)
	}
	defer client.Close()
	amount, err := resolveAmount(cmd, l, e, client)
	if err != nil {
		return nipcash.Source{}, err
	}
	return nipcash.Source{WalletPubkey: e.WalletPubkey, Amount: amount, Credential: cred}, nil
}

// doCashConsolidate calls cash_consolidate for sources (each corresponding
// to the local ledger entry with the same index's ID in localIDs) against
// target, updates the ledger, and returns the new merged entry.
// dialCandidates are each source's own token, tried in order — any
// connected source's client works to PLACE the call, since sources are
// authorized per-source by their own proof, not by the calling connection
// (see NIP-CASH §Consolidating Tokens). What isn't per-source is the
// generic Hub-side permission-expiry gate (nip47/permissions.HasPermission
// in lokihub): it checks only the connection actually dialed, not any
// other source in the batch — confirmed live against
// cash_consolidate_controller.go's own resolveConsolidateSource, which
// never re-checks a non-dialed source's own wallet expiry. Without this,
// whether a batch including one already-expired member could be placed AT
// ALL depended on the arbitrary order dialCandidates happened to be built
// in (previously always dialCandidates[0], i.e. ledger insertion order) —
// this tries each remaining candidate on an EXPIRED decline specifically
// before giving up, so an unrelated implementation detail no longer
// decides that.
//
// This does NOT "rescue" an already-expired source into a spendable
// result, and callers must not describe it that way: NIP-CASH's own merge
// rule inherits the EARLIEST expiry across every source (§Consolidating
// Tokens) — confirmed live — so a batch including one already-expired
// member produces a merged wallet that is ALSO already expired the moment
// it's created, dragging every other (otherwise healthy) source's share
// down with it. What this buys is narrower but still real: the call
// actually places (funds move into one traceable wallet an operator can
// act on, rather than the whole request just failing on an unrelated
// technicality) and, for a batch where every member is still short of its
// own deadline, it removes a spurious failure mode entirely. See
// runCashConsolidate's own pre-confirm warning for the interactive-path
// half of this same finding.
// reconcileAmbiguousSources is doCashConsolidate's only recourse when a
// consolidate call fails with the ambiguous "decrypt delivery" shape
// (warnAmbiguousDelivery's own doc comment): it can't read what the Hub
// merged the sources into, but it CAN ask each source's own original
// token, independently, whether the Hub still lists a claim for it — a
// call this codebase already makes elsewhere for the exact same reason
// (cash_receive.go's checkClaimWithCashHub, cash_redeem.go's
// resolveAmount). A confirmed-gone source is marked Consolidated (no
// destination to record — StatusConsolidated already means "became part
// of some merge", true here even though which merge is unrecoverable);
// anything else (still live, or the check itself couldn't be completed)
// is left exactly as it was — never guessed at either way. Returns the
// local IDs it could confirm gone, for the caller's own error message.
//
// What counts as proof: a "spent" TOMBSTONE, and nothing else. A Hub retains a
// deleted bill for a window precisely so its holder gets a definitive answer
// instead of having to infer one, and that is the only answer here that actually
// means "this bill's value has moved".
//
// It used to accept nipcash.ErrClaimNotFound as proof, which is a different
// statement — "this token does not name you" — and at least three live,
// non-consumed states produce it:
//
//   - a connection_key row can NEVER match: nipcash.MatchClaimAuto only matches
//     cash and pubkey rows, so every live connection_key source read as consumed,
//     deterministically and with no race;
//   - a pubkey source under --as, because the credential honours the override
//     while the pubkey compared against it was read from the LOCAL identity;
//   - a cash-mode source whose PendingCashSecret is the wrong one, since a wrong
//     secret also declines NOT_FOUND.
//
// Writing one of those off set StatusConsolidated, which removes the entry from
// Held() — so it vanished from redeem, redeem --all, wallet balance and --token
// resolution, while the user was told it had been "confirmed consumed on the Hub".
// For the cash-mode case it was worse than cosmetic: it disabled the one code path
// that could ever have discovered which secret was live.
//
// The new failure mode is the safe one. A bill that really is gone, but whose Hub
// has passed its retention window and fallen silent, is now left alone rather than
// marked — a stale entry the user can see and retry, instead of a live one that
// disappeared.
func reconcileAmbiguousSources(cmd *cobra.Command, l *ledger.Ledger, localIDs []string) []string {
	var confirmedGone []string
	for _, id := range localIDs {
		e, ok := l.Find(id)
		if !ok {
			continue
		}
		if _, decErr := nipcash.Decode(e.Token); decErr != nil {
			continue
		}
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			client, dialErr := nipcashclient.Connect(ctx, e.Token)
			if dialErr != nil {
				return // inconclusive — this source's own Hub might just be slow/unreachable right now
			}
			defer client.Close()
			cred, credErr := resolveCredential(cmd, e)
			if credErr != nil {
				return // inconclusive: without a credential we cannot ask at all
			}
			status, statusErr := client.CashStatus(ctx, cred, nipcash.ScopeMine)
			if statusErr != nil {
				return // inconclusive: an error is not evidence either way
			}
			if status.IsSpent() {
				_ = l.SetStatus(id, ledger.StatusConsolidated)
				confirmedGone = append(confirmedGone, id)
			}
		}()
	}
	if len(confirmedGone) > 0 {
		_ = l.Save() // best-effort: worst case, the next successful Save picks up the same corrected statuses
	}
	return confirmedGone
}

func doCashConsolidate(cmd *cobra.Command, l *ledger.Ledger, dialCandidates []string, sources []nipcash.Source, localIDs []string, target nipcash.Target, isSelfTarget bool) (newEntry *ledger.Entry, expiresAt *int64, err error) {
	if len(dialCandidates) == 0 {
		// Not reachable through runCashConsolidate today — sources and
		// dialCandidates (sourceTokens there) are always built in lockstep,
		// one append per item, so len(sources)>=2 (checked before this is
		// ever called) implies len(dialCandidates)>=2 too. Guarded anyway:
		// falling through the loop below with zero candidates would
		// silently return (nil, nil, nil) — a fake "success" with no call
		// ever placed, and a nil newEntry that then nil-derefs in
		// runCashConsolidate's own text-mode print (*newEntry.AmountMillis).
		return nil, nil, output.RuntimeError(cmd, fmt.Errorf("doCashConsolidate: no dial candidates given"))
	}
	// Written to disk BEFORE the first attempt, and outside the retry loop:
	// every candidate carries the same target, so the secret at risk is the
	// same one on every pass — see parkDestinationCashSecret for why the
	// window it closes destroys money rather than merely inconveniencing a
	// retry (D-CLI-1).
	parked, err := parkDestinationCashSecret(l, localIDs, target)
	if err != nil {
		return nil, nil, output.RuntimeError(cmd, err)
	}

	var lastErr error
	for i, dialToken := range dialCandidates {
		result, callErr := attemptCashConsolidateFn(dialToken, sources, target)
		if callErr == nil {
			for _, id := range localIDs {
				_ = l.SetStatus(id, ledger.StatusConsolidated)
			}
			newLedgerEntry := ledger.Entry{
				Token: result.NewWalletToken, WalletPubkey: result.NewWalletPubkey, AmountMillis: &result.AmountMillis, Verified: true,
				// Inherited, not verified against the new wallet itself — see sharedMinter.
				MinterPubkey: sharedMinterOfIDs(l, localIDs),
			}
			// A cash-mode target's own secret only ever exists in target
			// itself — the wire response never carries it (NIP-CASH
			// §Cash-Mode Slices: the caller supplies the commitment, the node
			// never mints/returns a secret) — discarding it here would be
			// the exact same fund-loss bug already found and fixed for
			// transfer's own cash (cash-mode) path.
			if bt, ok := target.(*nipcash.CashTarget); ok {
				newLedgerEntry.CashSecret = bt.Secret()
				newLedgerEntry.IdentityRequired = ptrTo(false)
			}
			// A pubkey/connection_key target other than the caller's own
			// identity is a gift — the caller doesn't own the resulting
			// wallet and can't redeem it themselves, so it must not be
			// saved as one of the caller's own held tokens (same rule
			// cash_transfer's own third-party spin-off already follows:
			// only ever persists the caller's own remainder, never the
			// recipient's token). A cash-mode target keeps the existing
			// behavior above (CashSecret set): unlike a pubkey/
			// connection_key gift, only the caller ever holds that secret,
			// so it's genuinely theirs to keep track of. newEntry is still
			// returned for display/hand-off either way, just not added to
			// l for a non-self, identity-bound target.
			_, isCashTarget := target.(*nipcash.CashTarget)
			if isSelfTarget || isCashTarget {
				newEntry, _ = l.Add(newLedgerEntry)
			} else {
				newEntry = &newLedgerEntry
			}
			// The destination secret now lives on the new entry's own
			// CashSecret, so the parked copy has done its job. Cleared
			// here rather than Saved here: this function never Saves —
			// its callers do, immediately — and that caller's single
			// write is the one that must carry both the result and this
			// clear, so a failed write keeps the park (see release()).
			parked.release()
			l.AppendHistory("consolidate", fmt.Sprintf("consolidated %d tokens into one %s note", len(localIDs), output.FormatAmount(int64(result.AmountMillis))))
			return newEntry, result.ExpiresAt, nil
		}

		reportErr := callErr
		if isAmbiguousDeliveryErr(callErr) {
			if gone := reconcileAmbiguousSources(cmd, l, localIDs); len(gone) > 0 {
				reportErr = fmt.Errorf("%w (confirmed consumed on the Hub despite the unreadable reply: %s — marked accordingly so they won't be offered again, though the merged result itself could not be recovered)",
					callErr, strings.Join(gone, ", "))
			} else {
				reportErr = warnAmbiguousDelivery(callErr, localIDs)
			}
		}
		lastErr = classifyCashTokenNWCErr(cmd, reportErr)
		if !isExpiredWalletErr(callErr) || i == len(dialCandidates)-1 {
			return nil, nil, lastErr
		}
		// callErr is specifically EXPIRED and a sibling candidate remains —
		// try it instead of failing a batch a different connection could
		// have carried just fine (see this function's own doc comment).
	}
	return nil, nil, lastErr
}

// attemptCashConsolidate places one cash_consolidate call, dialed through
// dialToken specifically. Returns the raw (unclassified) error so
// doCashConsolidate's own retry loop can inspect the real NWC code (via
// isExpiredWalletErr) before deciding whether to try a different dial
// candidate — classifying here would erase which underlying failure this
// even was.
func attemptCashConsolidate(dialToken string, sources []nipcash.Source, target nipcash.Target) (*nipcash.CashConsolidateResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := nipcashclient.Connect(ctx, dialToken)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	// The call itself is authorized by the first source's own credential: the caller
	// proves control of every source individually inside Sources, and any one of them
	// is equally a proof that this caller may make the call.
	if len(sources) == 0 {
		return nil, errors.New("attemptCashConsolidate: no sources")
	}
	return client.CashConsolidate(ctx, sources[0].Credential, nipcash.CashConsolidateParams{Sources: sources, To: target})
}

// attemptCashConsolidateFn is attemptCashConsolidate by default — a
// package-level indirection swapped out in cash_consolidate_test.go (same
// "reassign a package var for the test" pattern prompt.go's own stdin
// uses for PromptLine/Confirm) so doCashConsolidate's own retry-on-EXPIRED
// policy can be exercised deterministically, with no real dial.
var attemptCashConsolidateFn = attemptCashConsolidate
