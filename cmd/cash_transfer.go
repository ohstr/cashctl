package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/credential"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

func newCashTransferCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "transfer [amount] [target]",
		Short: "Send a held cash token, in full or split",
		Example: `  cashctl transfer 5
  cashctl transfer 5 <pubkey>
  cashctl transfer alice@example.com --amount 2`,
		Args: output.MaximumNArgs(2),
		RunE: runCashTransfer,
	}
	cmd.Flags().String("token", "", "which held token to transfer (auto-picked if you only hold one)")
	cmd.Flags().String("to", "", "recipient — hex pubkey, npub1..., name@domain, nconnection1..., bearer-target, or pubkey:/connection: forms")
	cmd.Flags().String("amount", "", fmt.Sprintf("how much to send, in %s — omitted sends the whole held token", output.CurrencyUnit))
	cmd.Flags().String("as", "", "override credential")
	cmd.Flags().String("ia", "", "Identity Authority to trust (hex pubkey or name@domain) for an nconnection1... target that doesn't specify one")
	return cmd
}

// resolveTarget wraps credential.ParseTarget with the one further step it
// can't do itself (internal/credential is a pure parser, no I/O): an
// nconnection1... string never carries an Identity Authority by design
// (docs/ux-review.md Part 1) — resolveTarget asks for one, gated on the
// same !jsonMode && !yesFlag rule every other interactive prompt in this
// codebase uses (see pickHeldToken). Shared by transfer and consolidate,
// the two ParseTarget callers.
func resolveTarget(cmd *cobra.Command, s string) (credential.ResolvedTarget, error) {
	rt, err := credential.ParseTarget(s)
	var needsIA *credential.NeedsIAError
	if !errors.As(err, &needsIA) {
		if err != nil {
			return credential.ResolvedTarget{}, output.InvalidInputError(cmd, s, err)
		}
		return rt, nil
	}

	iaIdentity, _ := cmd.Flags().GetString("ia")
	if iaIdentity == "" {
		jsonMode, _ := cmd.Flags().GetBool("json")
		yesFlag, _ := cmd.Flags().GetBool("yes")
		if jsonMode || yesFlag {
			return credential.ResolvedTarget{}, output.UsageError(cmd, needsIA)
		}
		platform := string(needsIA.Platform)
		if platform == "" {
			platform = "this connection"
		}
		line, err := PromptLine(fmt.Sprintf("This connection doesn't specify who to trust as Identity Authority for %s. Enter one (hex pubkey or NIP-05): ", platform))
		if err != nil {
			return credential.ResolvedTarget{}, output.RuntimeError(cmd, err)
		}
		iaIdentity = strings.TrimSpace(line)
		if iaIdentity == "" {
			return credential.ResolvedTarget{}, output.UsageError(cmd, needsIA)
		}
	}

	resolved, err := credential.ResolveConnectionTarget(s, needsIA.Key, needsIA.Platform, iaIdentity)
	if err != nil {
		return credential.ResolvedTarget{}, output.InvalidInputError(cmd, iaIdentity, err)
	}
	return resolved, nil
}

// fetchExpiresAt best-effort fetches token's current Hub-side expiry
// (nipcash.CheckClaimResult.ExpiresAt) — nil on any failure. Mirrors
// cash_redeem.go's own fetchRedeemPreview: this only ever feeds an
// optional confirmation-prompt warning, never blocks the caller. The same
// generic Hub-side AppPermission.ExpiresAt check (nip47/permissions in
// lokihub) gates cash_transfer and cash_consolidate exactly the way it
// gates cash_redeem — NIP-CASH's wallet-level expires_at is shared across
// every recipient row and every money-moving method on that wallet, not
// something specific to redemption — so an about-to-expire token is just
// as much a "confirm, then get rejected" trap here as it is on redeem.
// Deliberately doesn't reuse cash_redeem.go's own redeemPreview/
// previewSuffix: those also carry a fee quote that has no transfer/
// consolidate analogue (only cash_redeem charges one).
func fetchExpiresAt(cmd *cobra.Command, token string) *int64 {
	tok, err := nipcash.Decode(token)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := nipcashclient.Connect(ctx, token)
	if err != nil {
		return nil
	}
	defer client.Close()
	myPubHex, _ := localPubKeyHex(cmd)
	result, err := client.CheckClaim(ctx, tok, myPubHex)
	if err != nil {
		return nil
	}
	return result.ExpiresAt
}

// earliestExpiry returns whichever of a/b expires soonest — nil (never
// expires) loses to any concrete deadline. Mirrors the Hub's own
// cash_consolidate merge rule (NIP-CASH §Consolidating Tokens: "expiry is
// the earliest among the sources").
func earliestExpiry(a, b *int64) *int64 {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if *b < *a {
		return b
	}
	return a
}

// expiryWarningSuffix renders expiresAt as additional confirmation-prompt
// text — "" if there's nothing to show, or it isn't actually close yet.
// Same 24h threshold as cash_redeem.go's previewSuffix, so every
// money-moving command warns at the same point rather than a
// command-specific one a user has to relearn. verb names the action this
// warning is attached to ("transfer", "consolidate").
func expiryWarningSuffix(expiresAt *int64, verb string) string {
	if expiresAt == nil {
		return ""
	}
	remaining := time.Until(time.Unix(*expiresAt, 0))
	const soon = 24 * time.Hour
	if remaining <= 0 {
		return "Deadline passed — may fail."
	}
	if remaining < soon {
		return fmt.Sprintf("Expires in %s — %s now.", remaining.Round(time.Minute), verb)
	}
	return ""
}

// isExpiredWalletErr reports whether err is specifically an NWC EXPIRED
// decline — lokihub's generic per-app permission-expiry gate
// (nip47/permissions.HasPermission), not a proof/amount/policy problem
// with the request itself. Used by doCashConsolidate's and
// transferWithAutoConsolidate's own retry-on-a-different-source logic: see
// their doc comments for why an EXPIRED decline specifically is worth
// retrying via a sibling source and nothing else is.
func isExpiredWalletErr(err error) bool {
	var walletErr *relayclient.WalletError
	return errors.As(err, &walletErr) && walletErr.Code == "EXPIRED"
}

// transferWithAutoConsolidate handles cash selection's case 3 (docs/
// ux-review.md Part 2): no single held token covers the target amount,
// but several from the same minter, summed, do. Consolidate-then-transfer
// is one nipcash/client.TransferFromSources call (docs/private/
// nipcash-sdk-plan.md Finding 2) rather than two separately-saved steps —
// the interim, consolidated-to-self wallet is transient (spent again by
// the same call before it ever needs a ledger entry of its own), so
// nothing gets saved between the two wire calls on the success path.
// myPub is queried, not group[0]'s own identity: the merged token is
// always consolidated to the caller's own identity first, then
// transferred onward to target. Fully handles and returns cashctl's own
// output/exit — the caller should return whatever this returns without
// falling through to the plain single-source path.
func transferWithAutoConsolidate(cmd *cobra.Command, l *ledger.Ledger, group []ledger.Entry, sendAmount uint64, toValue string, target credential.ResolvedTarget) error {
	if len(group) == 0 {
		// Not reachable via runCashTransfer's own call site today —
		// ledger.SelectForAmount's ConsolidateFirst is only ever a
		// non-empty subset (smallestCoveringSubset appends at least one
		// entry before ever checking sum >= target) — but guarded the same
		// defensive way doCashConsolidate's own empty-dial-candidates case
		// is (cash_consolidate.go): falling through would build a
		// nonsensical zero-source params and, worse, silently succeed with
		// nothing transferred.
		return output.RuntimeError(cmd, fmt.Errorf("transferWithAutoConsolidate: no sources given"))
	}
	// Resolved before confirming, not after: a missing/misconfigured local
	// identity should fail fast, the same way runCashConsolidate's own
	// target resolution happens before its confirm prompt.
	myPub, err := localPubKeyHex(cmd)
	if err != nil {
		return output.RuntimeError(cmd, err)
	}

	jsonMode, _ := cmd.Flags().GetBool("json")
	yesFlag, _ := cmd.Flags().GetBool("yes")

	displayTarget := toValue
	if _, ok := target.Target.(*nipcash.BearerTarget); ok {
		displayTarget = "whoever holds this"
	}

	sum := ledger.SumAmounts(group)
	message := fmt.Sprintf("Consolidate %d tokens (%s) then send %s to %s?",
		len(group), output.FormatAmount(int64(sum)), output.FormatAmount(int64(sendAmount)), displayTarget)
	if !jsonMode && !yesFlag {
		// Best-effort, interactive-only — see fetchExpiresAt's own doc
		// comment on why this applies here too, not just to cash_redeem.
		// Earliest across the whole group: the interim cash_consolidate
		// merges these into one wallet whose own expiry is the earliest of
		// its sources (NIP-CASH §Consolidating Tokens) — confirmed live
		// that this isn't just informational here: if that earliest
		// figure is ALREADY past, the interim consolidate itself still
		// succeeds (doCashConsolidate's sibling-retry logic, shared via
		// isExpiredWalletErr, applies here too — see
		// attemptTransferFromSources' own retry loop below) but the
		// SECOND leg — the actual transfer onward to target — then always
		// fails, because it has to act through that just-created,
		// already-expired wallet specifically (no sibling to retry
		// through for that leg). The funds aren't lost (the
		// PartialProgressError branch below records them as a new held
		// token either way), but the transfer itself will not go through
		// — worth knowing before committing, not after.
		var earliest *int64
		var anyExpired bool
		_ = WithSpinner(jsonMode, "Checking expiry...", func() error {
			for i := range group {
				ea := fetchExpiresAt(cmd, group[i].Token)
				earliest = earliestExpiry(earliest, ea)
				if ea != nil && time.Until(time.Unix(*ea, 0)) <= 0 {
					anyExpired = true
				}
			}
			return nil
		})
		if anyExpired {
			fmt.Println("One source is expired — merge succeeds but the transfer onward will fail; funds land in a new held token instead.")
		} else if w := expiryWarningSuffix(earliest, "transfer"); w != "" {
			fmt.Println(w)
		}
	}
	// defaultYes=false: moves real money — never accept on a bare Enter.
	if !Confirm(cmd, false, message) {
		fmt.Println("Cancelled.")
		return nil
	}

	var sources []nipcash.Source
	sourceIDs := make([]string, len(group))
	err = WithSpinner(jsonMode, "Preparing sources...", func() error {
		for i := range group {
			src, sErr := sourceFromEntry(cmd, l, &group[i])
			if sErr != nil {
				return sErr
			}
			sources = append(sources, src)
			sourceIDs[i] = group[i].ID
		}
		return nil
	})
	if err != nil {
		return err
	}

	interimCred, err := localCashCredential(cmd)
	if err != nil {
		return output.RuntimeError(cmd, err)
	}

	params := nipcashclient.TransferFromSourcesParams{
		Sources:           sources,
		Amount:            sendAmount,
		To:                target.Target,
		InterimIdentity:   nipcash.Pubkey(myPub),
		InterimCredential: interimCred,
	}

	// Dial candidates in group order, but not committed to group[0]: NIP-
	// CASH's cash_consolidate accepts "any connected source's client" to
	// place the interim call (sources are authorized per-source by their
	// own proof, not by the calling connection) — but lokihub's generic
	// per-app permission-expiry gate (nip47/permissions.HasPermission)
	// checks ONLY the connection actually dialed, independent of every
	// other source's own expiry (confirmed live:
	// cash_consolidate_controller.go never re-checks a non-dialed
	// source's own wallet expiry). So a batch that happens to include one
	// already-expired member, dialed first, must not fail to even PLACE
	// the interim consolidate when a sibling member's own connection
	// would have carried the exact same request just fine — same
	// reasoning as doCashConsolidate's own retry loop in
	// cash_consolidate.go. This does NOT make the overall transfer
	// succeed when a member is already expired, though (see this
	// function's own pre-confirm warning above): the interim merge
	// inherits that member's already-past deadline (NIP-CASH's
	// earliest-wins merge rule), so the SECOND leg below — transferring
	// onward from the just-merged wallet — still fails; there's no
	// sibling to retry through for that leg, since only the merged wallet
	// itself holds the combined funds. What retrying the interim call
	// does buy: the funds still land somewhere real and traceable (the
	// PartialProgressError branch below records it), rather than the
	// whole attempt failing before anything happens at all, on a pick
	// that had nothing to do with the actual request.
	dialCandidates := make([]string, len(group))
	for i := range group {
		dialCandidates[i] = group[i].Token
	}
	var result *nipcashclient.TransferFromSourcesResult
	var ffsErr error
	_ = WithSpinner(jsonMode, "Transferring...", func() error {
		result, ffsErr = attemptTransferFromSourcesWithRetry(dialCandidates, params)
		return nil
	})
	if ffsErr == nil {
		// Full success: the interim wallet was spent again by the very
		// same call, within milliseconds of existing — it never needs
		// a ledger entry of its own (see this function's own doc
		// comment). If the final leg turned out to be an exact-match
		// reassignment of that interim wallet rather than a real
		// split, nipcash/client.TransferFromSources itself already
		// patches Transfer.NewWalletToken/NewWalletPubkey in from the
		// consolidate result before returning — see its own comment
		// for why that's needed (a real fund-inaccessibility gap this
		// integration suite's own multi-party tests caught, since no
		// prior test ever tried to actually receive an
		// auto-consolidated transfer's result on the other end).
		markSourcesConsolidated(l, sourceIDs, result.ConsolidatedFirst.AmountMillis)
		return printAndSaveTransferResult(cmd, l, result.Transfer, sendAmount, toValue, target, sourceIDs)
	}

	var partial *nipcashclient.PartialProgressError
	if errors.As(ffsErr, &partial) && partial.Consolidated != nil {
		// The interim consolidate landed for real, even though the
		// final transfer then failed — unlike RekeyBearerSlice's own
		// interim step (an in-place transfer, same wallet pubkey
		// throughout), cash_consolidate always spins off a genuinely
		// NEW wallet. That wallet's funds are still sitting there,
		// under the caller's own identity, real and spendable — it
		// must get its own ledger entry now, or those funds go
		// untracked entirely (the same fund-safety rule
		// receive_secure.go's own PartialProgressError handling
		// follows, just for a spun-off wallet instead of an
		// in-place reassignment). Never worth retrying: the sources
		// are already consumed by the consolidate that just landed.
		markSourcesConsolidated(l, sourceIDs, partial.Consolidated.AmountMillis)
		_, _ = l.Add(ledger.Entry{
			Token:        partial.Consolidated.NewWalletToken,
			WalletPubkey: partial.Consolidated.NewWalletPubkey,
			AmountMillis: &partial.Consolidated.AmountMillis,
			Verified:     true,
		})
		_ = l.Save()
		return classifyNWCErr(cmd, ffsErr)
	}

	return classifyNWCErr(cmd, ffsErr)
}

// attemptTransferFromSourcesWithRetry places the interim-consolidate-then-
// transfer call once per dialCandidate in order, applying the identical
// retry policy as doCashConsolidate's own loop (cash_consolidate.go — see
// its doc comment for the full reasoning): stop at the first success, the
// first *nipcashclient.PartialProgressError (the interim consolidate
// already landed for real and the sources are consumed — never worth
// trying a different dial after that), or the first non-EXPIRED failure;
// otherwise retry the next candidate. Factored out of
// transferWithAutoConsolidate so this policy is unit-testable without a
// network call — see attemptTransferFromSourcesFn's own doc comment for how
// a test swaps out the actual dial, and cash_transfer_test.go for coverage.
func attemptTransferFromSourcesWithRetry(dialCandidates []string, params nipcashclient.TransferFromSourcesParams) (*nipcashclient.TransferFromSourcesResult, error) {
	if len(dialCandidates) == 0 {
		// See transferWithAutoConsolidate's own len(group)==0 guard — not
		// reachable from there today, guarded here too so this function
		// can't silently report a nil-error, nil-result "success" on its
		// own if some future caller ever passes an empty list directly.
		return nil, fmt.Errorf("attemptTransferFromSourcesWithRetry: no dial candidates given")
	}
	var lastErr error
	for i, dialToken := range dialCandidates {
		result, err := attemptTransferFromSourcesFn(dialToken, params)
		if err == nil {
			return result, nil
		}

		var partial *nipcashclient.PartialProgressError
		if errors.As(err, &partial) && partial.Consolidated != nil {
			return nil, err
		}

		lastErr = err
		if !isExpiredWalletErr(err) || i == len(dialCandidates)-1 {
			return nil, err
		}
		// This group member's own connection is what's expired, not
		// necessarily the whole batch — try the next one before giving up.
	}
	return nil, lastErr
}

// attemptTransferFromSources places one TransferFromSources call, dialed
// through dialToken specifically — the one piece of
// attemptTransferFromSourcesWithRetry's own retry-on-EXPIRED loop needs to
// vary per attempt. Returns the raw (unclassified) error so that loop can
// inspect the real NWC code (via isExpiredWalletErr) before deciding
// whether to retry; classifying here would erase which underlying failure
// this even was.
func attemptTransferFromSources(dialToken string, params nipcashclient.TransferFromSourcesParams) (*nipcashclient.TransferFromSourcesResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := nipcashclient.Connect(ctx, dialToken)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	return client.TransferFromSources(ctx, params)
}

// attemptTransferFromSourcesFn is attemptTransferFromSources by default —
// swapped out in cash_transfer_test.go (same package-var-swap pattern
// prompt.go's own stdin uses, and cash_consolidate.go's own
// attemptCashConsolidateFn) so attemptTransferFromSourcesWithRetry's retry
// policy can be exercised deterministically, with no real dial.
var attemptTransferFromSourcesFn = attemptTransferFromSources

// markSourcesConsolidated marks every source ID as consolidated and
// records the merge in history.
func markSourcesConsolidated(l *ledger.Ledger, sourceIDs []string, amountMillis uint64) {
	for _, id := range sourceIDs {
		_ = l.SetStatus(id, ledger.StatusConsolidated)
	}
	l.AppendHistory("consolidate", fmt.Sprintf("consolidated %d tokens into one %s note", len(sourceIDs), output.FormatAmount(int64(amountMillis))))
}

// printAndSaveTransferResult applies a completed cash_transfer call's
// ledger effects and renders cashctl's own output — shared by the plain
// single-source path and transferWithAutoConsolidate, since both end in
// exactly the same "one CashTransferResult, maybe a remainder" shape.
// target is accepted (not just its Resolved string) so a bearer target's
// combined <token>#<secret> string — the actual thing the recipient
// needs — can be assembled here, since only this function ever sees
// transferResult.NewWalletToken (the token half; the secret half was
// already known at confirm time via target.Target's own BearerTarget).
func printAndSaveTransferResult(cmd *cobra.Command, l *ledger.Ledger, transferResult *nipcash.CashTransferResult, sentAmount uint64, toValue string, target credential.ResolvedTarget, consolidatedFrom []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	l.AppendHistory("transfer", fmt.Sprintf("transferred %s to %s", output.FormatAmount(int64(sentAmount)), toValue))

	var remainderEntry *ledger.Entry
	if transferResult.RemainderWalletToken != "" {
		remainderEntry, _ = l.Add(ledger.Entry{Token: transferResult.RemainderWalletToken, WalletPubkey: transferResult.RemainderWalletPubkey})
		if transferResult.RemainingAmountMillis != nil {
			remainderEntry.AmountMillis = transferResult.RemainingAmountMillis
		}
		if remainderEntry.AmountMillis != nil {
			l.AppendHistory("transfer", fmt.Sprintf("kept remainder of %s as a new token", output.FormatAmount(int64(*remainderEntry.AmountMillis))))
		} else {
			l.AppendHistory("transfer", "kept remainder as a new token")
		}
	}
	if err := l.Save(); err != nil {
		return output.RuntimeError(cmd, err)
	}

	// For a bearer target there's no recipient identity — the recipient
	// is whoever holds the combined token#secret string, so that's what
	// gets displayed/returned in its place. toValue itself (the literal
	// "bearer-target"/user input) still goes to AppendHistory above
	// unchanged — only this display/--json substitution uses the
	// assembled string.
	displayTo := toValue
	var cashToSend string
	if bt, ok := target.Target.(*nipcash.BearerTarget); ok && transferResult.NewWalletToken != "" {
		cashToSend = fmt.Sprintf("%s#%s", transferResult.NewWalletToken, bt.Secret())
		displayTo = cashToSend
	}

	if jsonMode {
		// nipcash.CashTransferResult has no JSON tags of its own (an
		// internal SDK type, not a wire DTO) — built explicitly here so
		// --json output stays snake_case like every other cashctl command's,
		// instead of leaking Go field names (see cash_inspect.go's decode
		// command for the same fix).
		out := map[string]any{
			"amount_millis":           transferResult.AmountMillis,
			"identity_type":           transferResult.IdentityType,
			"identity_value":          transferResult.IdentityValue,
			"remaining_amount_millis": transferResult.RemainingAmountMillis,
			"new_wallet_pubkey":       transferResult.NewWalletPubkey,
			"new_wallet_token":        transferResult.NewWalletToken,
			"remainder_entry":         remainderEntry,
			"target_resolved":         target.Resolved,
			"consolidated_from":       consolidatedFrom,
		}
		if cashToSend != "" {
			out["cash_to_send"] = cashToSend
		}
		output.PrintJSON(out)
		return nil
	}
	// A remainder left over from a split is saved above regardless — never
	// narrated here. Same reasoning as the pre-transfer confirm message:
	// this is wallet mechanism, not something the user decided, and it's
	// always visible afterward via `cashctl wallet show`/`wallet balance`.
	fmt.Printf("Transferred %s to %s.\n", output.FormatAmount(int64(sentAmount)), displayTo)
	return nil
}

// disambiguateTransferArgs decides which of transfer's up-to-2 positional
// args is the target and which is the amount — sniffed by shape, not
// position: every real target (hex pubkey, npub1..., a name@domain,
// nconnection1..., "bearer-target", or the pubkey:/connection: forms)
// fails output.ParseAmount, so anything that parses as a loki amount
// (whole or fractional, e.g. "500" or "0.5") can only ever be the amount.
// This is what lets `cashctl transfer 500` (no target at all) work instead
// of failing to parse "500" as a target, and — with two args — lets both
// `cashctl transfer 5000 <target>` (the documented order) and `cashctl
// transfer <target> 5000` (the old order) resolve the same way, since
// whichever side parses as an amount is the amount. Pure and cobra-free
// so it's unit-testable directly, mirroring resolveConsolidateSources's
// own reasoning in cash_consolidate.go.
func disambiguateTransferArgs(args []string) (positionalTo, positionalAmount string) {
	switch len(args) {
	case 1:
		if _, numErr := output.ParseAmount(args[0]); numErr == nil {
			positionalAmount = args[0]
		} else {
			positionalTo = args[0]
		}
	case 2:
		if _, numErr := output.ParseAmount(args[0]); numErr == nil {
			positionalAmount, positionalTo = args[0], args[1]
		} else {
			positionalTo, positionalAmount = args[0], args[1]
		}
	}
	return positionalTo, positionalAmount
}

func runCashTransfer(cmd *cobra.Command, args []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	toFlagValue, _ := cmd.Flags().GetString("to")
	amountFlagValue, _ := cmd.Flags().GetString("amount")

	positionalTo, positionalAmount := disambiguateTransferArgs(args)

	toValue, err := resolvePositionalOrFlag(cmd, positionalTo, "to", toFlagValue)
	if err != nil {
		return err
	}
	amountFlag, err := resolvePositionalOrFlagAmount(cmd, positionalAmount, "amount", amountFlagValue)
	if err != nil {
		return err
	}
	// No target given at all, but an amount was: default to a bearer note
	// instead of erroring — "just an amount, share the result with
	// whoever" is a complete, valid request, not a usage mistake. An
	// explicit --to/positional target still always wins when given.
	if toValue == "" {
		if amountFlag == 0 {
			return output.UsageError(cmd, fmt.Errorf("a destination is required — pass it directly (cashctl transfer <target>) or via --to"))
		}
		toValue = "bearer-target"
	}

	target, err := resolveTarget(cmd, toValue)
	if err != nil {
		return err
	}
	if target.Resolved != "" {
		output.Linef(jsonMode, "  resolves to: %s", target.Resolved)
	}

	l, err := ledger.Load()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}

	tokenFlag, _ := cmd.Flags().GetString("token")
	var entry *ledger.Entry
	if tokenFlag == "" && amountFlag > 0 && len(l.Held()) > 0 {
		// A target amount was given and no specific token was pinned —
		// cash selection decides which held token(s) reach it exactly,
		// instead of resolveHeldToken's plain single-entry pick (see
		// docs/ux-review.md Part 2). Only once something is actually
		// held: with nothing held at all, resolveHeldToken's own "receive
		// one first" error below is the clearer message — cash selection
		// would otherwise report a confusing "0 total, funds fragmented."
		plan, err := ledger.SelectForAmount(l.Held(), amountFlag)
		if err != nil {
			return output.UsageError(cmd, err)
		}
		if len(plan.ConsolidateFirst) > 0 {
			return transferWithAutoConsolidate(cmd, l, plan.ConsolidateFirst, amountFlag, toValue, target)
		}
		liveEntry, err := resolveLiveEntry(l, plan.Entry.ID)
		if err != nil {
			return output.RuntimeError(cmd, err)
		}
		entry = liveEntry
		if !plan.Split {
			amountFlag = 0
		}
	} else {
		e, err := resolveHeldToken(cmd, l)
		if err != nil {
			return err
		}
		entry = e
	}

	cred, err := resolveCredential(cmd, entry)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var client *nipcashclient.Client
	err = WithSpinner(jsonMode, "Connecting...", func() error {
		c, cErr := nipcashclient.Connect(ctx, entry.Token)
		if cErr != nil {
			return cErr
		}
		client = c
		return nil
	})
	if err != nil {
		return output.NetworkError(cmd, err)
	}
	defer client.Close()

	var amount uint64
	err = WithSpinner(jsonMode, "Checking amount...", func() error {
		a, aErr := resolveAmount(cmd, l, entry, client)
		if aErr != nil {
			return aErr
		}
		amount = a
		return nil
	})
	if err != nil {
		return err
	}

	yesFlag, _ := cmd.Flags().GetBool("yes")
	var expiresAt *int64
	if !jsonMode && !yesFlag {
		// Best-effort, never blocks — see fetchExpiresAt's own doc comment.
		_ = WithSpinner(jsonMode, "Checking expiry...", func() error {
			expiresAt = fetchExpiresAt(cmd, entry.Token)
			return nil
		})
	}

	displayTarget := toValue
	if _, ok := target.Target.(*nipcash.BearerTarget); ok {
		displayTarget = "whoever holds this"
	}

	var splitAmount *uint64
	sentAmount := amount
	sendAmount := amount
	if amountFlag > 0 {
		if amountFlag > amount {
			return output.UsageError(cmd, fmt.Errorf("can't send %s to %s — held token only has %s", output.FormatAmount(int64(amountFlag)), displayTarget, output.FormatAmount(int64(amount))))
		}
		splitAmount = &amountFlag
		sentAmount = amountFlag
		sendAmount = amountFlag
	}
	// Splitting off a partial amount and keeping the rest as a new token is
	// mechanism, not a decision — same as a Bitcoin wallet not asking
	// "keep the change?" before spending a UTXO bigger than the payment.
	// The confirmation only ever names what's actually being sent.
	message := fmt.Sprintf("Transfer %s to %s?", output.FormatAmount(int64(sendAmount)), displayTarget)
	if w := expiryWarningSuffix(expiresAt, "transfer"); w != "" {
		fmt.Println(w)
	}
	// defaultYes=false: moves real money — never accept on a bare Enter.
	if !Confirm(cmd, false, message) {
		fmt.Println("Cancelled.")
		return nil
	}

	var result *nipcash.CashTransferResult
	err = WithSpinner(jsonMode, "Transferring...", func() error {
		r, cErr := client.CashTransfer(ctx, nipcash.CashTransferParams{
			Credential: cred, To: target.Target, CurrentAmount: amount, SplitAmount: splitAmount,
		})
		if cErr != nil {
			return cErr
		}
		result = r
		return nil
	})
	if err != nil {
		return classifyNWCErr(cmd, err)
	}

	_ = l.SetStatus(entry.ID, ledger.StatusTransferred)
	return printAndSaveTransferResult(cmd, l, result, sentAmount, toValue, target, nil)
}

// resolveLiveEntry re-resolves id against l itself and returns the result —
// cash selection's ledger.SelectForAmount returns a SelectionPlan.Entry
// pointing into l.Held()'s own copy slice (Ledger.Held's own doc comment:
// "returns copies"), not l.Entries, so a caller that treats it as live and
// later mutates a field directly (the way resolveAmount does when it
// discovers an uncached amount) would have that write silently lost — the
// exact bug found and fixed in resolveHeldToken for the identical shape
// (cmd/cash_redeem.go; see its own doc comment and
// docs/private/audit-round3-redeem-fee-boundaries.md). Not reachable via
// this call site today — SelectForAmount's cases 1-2 only ever pick an
// entry with AmountMillis already known, so resolveAmount's mutating
// branch never runs on it — but this makes that guarantee's absence
// harmless instead of silent, for whatever calls this next.
func resolveLiveEntry(l *ledger.Ledger, id string) (*ledger.Entry, error) {
	e, ok := l.Find(id)
	if !ok {
		return nil, fmt.Errorf("internal: cash-selected entry %q vanished from the ledger", id)
	}
	return e, nil
}
