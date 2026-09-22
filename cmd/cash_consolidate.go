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
		Long: `Merges several held cash tokens into one. With no IDs/--sources given,
auto-detects which of your held tokens share a minter (only same-minter
tokens can actually be merged) and consolidates each such group — one
call per minter, not a single "everything" attempt that would fail across
minters. Interactive sessions are asked which group(s) to proceed with
when more than one qualifies; --json/--yes processes every qualifying
group. Pass IDs/--sources for exact control instead (see "cashctl wallet
show --json" for the IDs — plain-text "wallet show" never prints them).`,
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
	// can actually be merged instead (see mergeableMinterGroups) and
	// process each such group, rather than erroring out and making the
	// common "tidy up my dust" case require first hunting down IDs.
	groups := mergeableMinterGroups(l.Held())
	if len(groups) == 0 {
		if jsonMode {
			output.PrintJSON(map[string]any{"consolidated": []any{}})
		} else {
			fmt.Println("Nothing to consolidate — no minter has more than one held token.")
		}
		return nil
	}

	chosen, err := pickMinterGroups(cmd, groups)
	if err != nil {
		return err
	}

	var results []*consolidateResult
	for _, group := range chosen {
		ids := make([]string, len(group))
		for i, e := range group {
			ids[i] = e.ID
		}
		result, err := consolidateItems(cmd, l, ids, toFlag, jsonMode, yesFlag)
		if err != nil {
			return err
		}
		if result != nil {
			results = append(results, result)
		}
		// A decline (result == nil, only reachable outside --json/--yes —
		// see Confirm's own doc comment) is this group's own call to make;
		// it doesn't cancel the remaining chosen groups.
	}
	printConsolidateResults(jsonMode, results)
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
	err := WithSpinner(jsonMode, "Checking sources...", func() error {
		for _, item := range items {
			item = strings.TrimSpace(item)
			if e, ok := l.Find(item); ok {
				src, err := sourceFromEntry(cmd, l, e)
				if err != nil {
					return err
				}
				sources = append(sources, src)
				sourceTokens = append(sourceTokens, e.Token)
				total += src.Amount
				localIDs = append(localIDs, e.ID)
				if !jsonMode && !yesFlag {
					// Best-effort, interactive-only — see fetchExpiresAt's own
					// doc comment on why this applies here too, not just to
					// cash_redeem. Tracked per-source, not just as one overall
					// earliest, because a MIX of already-expired and healthy
					// sources needs a sharper warning than "expires soon" — see
					// this function's own message-building below.
					if ea := fetchExpiresAt(cmd, e.Token); ea != nil {
						earliestExpiresAt = earliestExpiry(earliestExpiresAt, ea)
						if time.Until(time.Unix(*ea, 0)) <= 0 {
							expiredSources++
						} else {
							healthySources++
						}
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
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(sources) < 2 {
		return nil, output.UsageError(cmd, fmt.Errorf("consolidate needs at least 2 sources, got %d", len(sources)))
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
	if expiredSources > 0 && healthySources > 0 {
		// Sharper than expiryWarningSuffix's generic "will likely be
		// rejected": confirmed live (see docs/private/
		// audit-round2-expiration-matrix.md) that this call actually
		// SUCCEEDS — doCashConsolidate's own retry logic places it via a
		// still-valid sibling connection — but NIP-CASH's merge rule
		// inherits the EARLIEST expiry across every source
		// (§Consolidating Tokens), so the merged result is born with the
		// already-past deadline too. That's not a rejection; it's every
		// healthy source's own money losing its own, later, still-good
		// deadline in the process. This is the one case worth blocking on
		// even under this command's ordinary "just note it" expiry
		// posture.
		output.Notef(jsonMode, "%d of these already expired — merging it in makes the WHOLE %s token unusable too. Leave it out to keep the rest spendable.",
			expiredSources, output.FormatAmount(int64(total)))
	} else if w := expiryWarningSuffix(earliestExpiresAt, "consolidate"); w != "" {
		output.Notef(jsonMode, "%s", w)
	}
	// defaultYes=false: moves real money — never accept on a bare Enter.
	// Only reachable outside --json/--yes (Confirm auto-accepts under
	// either), so a declined group is always an interactive choice, never
	// something a script/agent needs to handle.
	if !Confirm(cmd, false, message) {
		fmt.Println("Cancelled.")
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
		fmt.Printf("Consolidated into one %s note, saved to your wallet.\n", output.FormatAmount(int64(*r.NewEntry.AmountMillis)))
	}
}

// resolveConsolidateSources decides which source items runCashConsolidate
// acts on when the caller gave explicit positional args or --sources
// (comma-separated) — runCashConsolidate only calls this once it's
// already checked at least one of the two is set; with neither given, it
// takes the auto-detected-minter-groups path instead (mergeableMinterGroups/
// pickMinterGroups), never this function's own "every held token" fallback
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

// mergeableMinterGroups returns held's consolidation-eligible entries
// (ledger.GroupableForConsolidation — pubkey-mode, known minter, known
// amount), grouped by minter (ledger.GroupByMinter), restricted to
// minters with 2+ held tokens: a singleton has nothing to merge into.
// Used by runCashConsolidate's no-args/no---sources default instead of
// naively trying to merge every held token together, which isn't possible
// across minters (cash_consolidate only accepts same-minter sources).
func mergeableMinterGroups(held []ledger.Entry) map[string][]ledger.Entry {
	groups := ledger.GroupByMinter(ledger.GroupableForConsolidation(held))
	out := make(map[string][]ledger.Entry, len(groups))
	for minter, entries := range groups {
		if len(entries) >= 2 {
			out[minter] = entries
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

// sortedMinterKeys returns groups' minter keys in a stable order, so
// pickMinterGroups' numbered list (and its own tests) don't depend on Go's
// randomized map iteration order.
func sortedMinterKeys(groups map[string][]ledger.Entry) []string {
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// pickMinterGroups decides which of groups' minter clusters
// runCashConsolidate should actually process. Under --json/--yes there's
// no terminal to ask from (and no point asking a question a script can't
// answer) — every qualifying group is processed, matching this command's
// "tidy up my dust" default spirit for scripted/agentic use. A single
// group needs no prompt either way — it's the only sensible choice.
// Otherwise, interactively: shown as a numbered list (mirroring
// pickHeldToken's own style in cash_redeem.go), a bare Enter or "all"
// picks every group, or a comma-separated subset of numbers picks just
// those.
func pickMinterGroups(cmd *cobra.Command, groups map[string][]ledger.Entry) ([][]ledger.Entry, error) {
	keys := sortedMinterKeys(groups)
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

	output.Notef(false, "Found %d separate minters you can consolidate:", len(keys))
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
func reconcileAmbiguousSources(cmd *cobra.Command, l *ledger.Ledger, localIDs []string) []string {
	myPubHex, _ := localPubKeyHex(cmd)
	var confirmedGone []string
	for _, id := range localIDs {
		e, ok := l.Find(id)
		if !ok {
			continue
		}
		tok, decErr := nipcash.Decode(e.Token)
		if decErr != nil {
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
			if _, claimErr := client.CheckClaim(ctx, tok, myPubHex); errors.Is(claimErr, nipcash.ErrClaimNotFound) {
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
			// transfer's own cash (bearer) path.
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
	return client.CashConsolidate(ctx, nipcash.CashConsolidateParams{Sources: sources, To: target})
}

// attemptCashConsolidateFn is attemptCashConsolidate by default — a
// package-level indirection swapped out in cash_consolidate_test.go (same
// "reassign a package var for the test" pattern prompt.go's own stdin
// uses for PromptLine/Confirm) so doCashConsolidate's own retry-on-EXPIRED
// policy can be exercised deterministically, with no real dial.
var attemptCashConsolidateFn = attemptCashConsolidate
