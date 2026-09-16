package cmd

import (
	"context"
	"fmt"
	"strconv"
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
		Long: `Bare local ledger IDs, as positional arguments or comma-separated via
--sources (amount and credential are already known per entry) — the
verbose <token>:<amount>:<credential> form (--sources only) is only needed
for a source that isn't in your local ledger (e.g. consolidating on
someone else's behalf via a captured proof), and only supports pubkey/
bearer credentials (a connection-key credential's own colons make it
ambiguous in this shorthand — use a locally-held entry for that case).

With neither positional IDs nor --sources given, consolidates every
currently held token.`,
		Args: cobra.ArbitraryArgs,
		RunE: runCashConsolidate,
	}
	cmd.Flags().String("sources", "", "comma-separated: local ledger IDs, or <token>:<amount>:<credential>; same as the positional arguments for the bare-ID form, kept for scripted/agentic use")
	cmd.Flags().String("to", "", "defaults to your own identity")
	cmd.Flags().String("ia", "", "Identity Authority to trust (hex pubkey or NIP-05) for an nconnection1... target that doesn't specify one")
	return cmd
}

func runCashConsolidate(cmd *cobra.Command, args []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	sourcesFlag, _ := cmd.Flags().GetString("sources")
	toFlag, _ := cmd.Flags().GetString("to")

	l, err := ledger.Load()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}

	items, err := resolveConsolidateSources(args, sourcesFlag, l.Held())
	if err != nil {
		return output.UsageError(cmd, err)
	}

	yesFlag, _ := cmd.Flags().GetBool("yes")

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
		amount, err := strconv.ParseUint(parts[1], 10, 64)
		if err != nil {
			return output.InvalidInputError(cmd, item, fmt.Errorf("invalid amount %q", parts[1]))
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
	if len(sources) < 2 {
		return output.UsageError(cmd, fmt.Errorf("consolidate needs at least 2 sources, got %d", len(sources)))
	}

	var target nipcash.Target
	var targetResolved string
	if toFlag != "" {
		rt, err := resolveTarget(cmd, toFlag)
		if err != nil {
			return err
		}
		target = rt.Target
		targetResolved = rt.Resolved
		if targetResolved != "" {
			output.Linef(jsonMode, "  resolves to: %s", targetResolved)
		}
	} else {
		myPub, err := localPubKeyHex(cmd)
		if err != nil {
			return output.RuntimeError(cmd, err)
		}
		target = nipcash.Pubkey(myPub)
	}

	message := fmt.Sprintf("This will consolidate %d tokens (%d %s total) into one%s.",
		len(sources), total, output.CurrencyUnit, ifTargetIsSelf(toFlag))
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
		message += fmt.Sprintf(" WARNING: %d of these has already expired — merging it in will make the WHOLE resulting %d %s token immediately unusable via this wallet connection too (same as the expired one already was), not just its own share. Consolidating without it keeps the rest spendable; the expired one is recoverable only by contacting the Hub operator either way.",
			expiredSources, total, output.CurrencyUnit)
	} else {
		message += expiryWarningSuffix(earliestExpiresAt, "consolidate")
	}
	// defaultYes=false: moves real money — never accept on a bare Enter.
	if !Confirm(cmd, false, message+" Continue?") {
		fmt.Println("Cancelled.")
		return nil
	}

	newEntry, expiresAt, err := doCashConsolidate(cmd, l, sourceTokens, sources, localIDs, target)
	if err != nil {
		return err
	}
	_ = l.Save()

	if jsonMode {
		// new_entry (a ledger.Entry, properly snake_case-tagged) already
		// carries result's token/wallet_pubkey/amount — expires_at is the
		// only field of nipcash.CashConsolidateResult (no JSON tags of its
		// own) worth surfacing separately (see cash_transfer.go's own fix
		// for the same underlying issue).
		output.PrintJSON(map[string]any{"new_entry": newEntry, "expires_at": expiresAt, "target_resolved": targetResolved})
		return nil
	}
	fmt.Printf("Consolidated into one %d %s note, saved to your wallet.\n", *newEntry.AmountMillis, output.CurrencyUnit)
	return nil
}

// resolveConsolidateSources decides which source items runCashConsolidate
// acts on: positional args, --sources (comma-separated), or — with
// neither given — every currently held token, the same "everything, by
// default" stance `wallet balance` already takes (the common case, "tidy
// up my dust," shouldn't require first running `wallet show` to collect
// IDs to paste back in). Pure and cobra-free so it's unit-testable
// directly, without a ledger file or a network call — mirrors
// resolvePositionalOrFlag's own reasoning in cmd/positional.go, just for
// a list instead of a single value.
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
func doCashConsolidate(cmd *cobra.Command, l *ledger.Ledger, dialCandidates []string, sources []nipcash.Source, localIDs []string, target nipcash.Target) (newEntry *ledger.Entry, expiresAt *int64, err error) {
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
			newLedgerEntry := ledger.Entry{Token: result.NewWalletToken, WalletPubkey: result.NewWalletPubkey, AmountMillis: &result.AmountMillis, Verified: true}
			// A bearer target's own secret only ever exists in target
			// itself — the wire response never carries it (NIP-CASH
			// §Bearer Slices: the caller supplies the commitment, the node
			// never mints/returns a secret) — discarding it here would be
			// the exact same fund-loss bug already found and fixed for
			// transfer's own bearer-target path.
			if bt, ok := target.(*nipcash.BearerTarget); ok {
				newLedgerEntry.BearerSecret = bt.Secret()
				newLedgerEntry.IdentityRequired = ptrTo(false)
			}
			newEntry, _ = l.Add(newLedgerEntry)
			l.AppendHistory("consolidate", fmt.Sprintf("consolidated %d tokens into one %d %s note", len(localIDs), result.AmountMillis, output.CurrencyUnit))
			return newEntry, result.ExpiresAt, nil
		}

		lastErr = classifyNWCErr(cmd, callErr)
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
