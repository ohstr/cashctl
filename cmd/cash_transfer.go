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
		Short: "Send a held cash token",
		Example: `  cashctl transfer 5
  cashctl transfer 5 <pubkey>
  cashctl transfer alice@example.com --amount 2`,
		Args: output.MaximumNArgs(2),
		RunE: runCashTransfer,
	}
	cmd.Flags().String("token", "", "which held token to transfer (auto-picked if you only hold one)")
	cmd.Flags().String("to", "", "recipient — hex pubkey, npub1..., name@domain, nconnection1..., cash, or pubkey:/connection: forms")
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
			return credential.ResolvedTarget{}, output.InvocationError(cmd, needsIA)
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
			return credential.ResolvedTarget{}, output.InvocationError(cmd, needsIA)
		}
	}

	resolved, err := credential.ResolveConnectionTarget(s, needsIA.Key, needsIA.Platform, iaIdentity)
	if err != nil {
		return credential.ResolvedTarget{}, output.InvalidInputError(cmd, iaIdentity, err)
	}
	return resolved, nil
}

// targetClause renders how a transfer's destination reads inside a
// sentence: "as cash" for a bearer target (no destination was ever given,
// so the result is redeemable by whoever ends up holding it), or "to
// <toValue> as identity cash" / "to <toValue> as web identity cash" for a
// resolved recipient — matching whichever Kind credential.ParseTarget/
// ResolveConnectionTarget assigned. "Cash"/"identity cash"/"web identity
// cash" deliberately avoid the word "token" (or any other developer-facing
// jargon) in every user-facing message: the string this all builds up to
// (recipientToken, or the combined bearer <token>#<secret>) is real,
// spendable money, not a code — see printAndSaveTransferResult's own
// framing of it. Shared by the plain transfer path and
// transferWithAutoConsolidate so both name the destination identically.
func targetClause(toValue string, kind credential.TargetKind) string {
	switch kind {
	case credential.TargetKindConnection:
		return fmt.Sprintf("to %s as web identity cash", toValue)
	case credential.TargetKindBearer:
		return "as cash"
	default:
		return fmt.Sprintf("to %s as identity cash", toValue)
	}
}

// transferConfirmMessage renders the plain (non-auto-consolidate)
// transfer's own confirm prompt. Only the bearer case spells out what "as
// cash" actually means (anyone who ends up holding it can redeem it): a
// named destination is self-explanatory ("to alice@example.com"), but a
// destination-less transfer is the one place a first-time user could
// otherwise agree to "Transfer 55 loki as cash?" without realizing that
// creates a single, one-shot, unrecoverable value they alone are
// responsible for handing off (see printAndSaveTransferResult's own
// "save this now" framing on the result side of the same transfer).
func transferConfirmMessage(sendAmount uint64, toValue string, kind credential.TargetKind) string {
	amt := output.FormatAmount(int64(sendAmount))
	if kind == credential.TargetKindBearer {
		return fmt.Sprintf("Transfer %s as cash — anyone holding it can redeem it. Continue?", amt)
	}
	return fmt.Sprintf("Transfer %s %s?", amt, targetClause(toValue, kind))
}

// shouldPrintResolvedTarget decides whether a resolved target's Resolved
// string is worth showing before confirming anything. Deliberately false
// for a bearer target specifically: its Resolved carries the freshly
// generated secret (needed intact for --json's target_resolved field —
// see credential.go's own ParseTarget), but printing it here, before
// anything is even confirmed, is pure noise — nothing is actionable with
// the bare secret alone, and it's shown again anyway, combined with the
// resulting token, once the transfer actually completes
// (printAndSaveTransferResult's cashToSend). Every other resolution (a
// NIP-05 lookup, an nconnection's IA) genuinely benefits from review
// before confirming, so only the bearer case is skipped. Pure so it's
// unit-testable without a cobra.Command.
func shouldPrintResolvedTarget(target credential.ResolvedTarget) bool {
	if target.Resolved == "" {
		return false
	}
	_, isBearer := target.Target.(*nipcash.BearerTarget)
	return !isBearer
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
	return expiresAtFromCheck(result, err)
}

// expiresAtFromCheck turns a CheckClaim outcome into fetchExpiresAt's
// answer. Any failure is nil (no deadline known) EXCEPT an EXPIRED decline,
// which is itself the answer: an already-expired wallet rejects
// list_recipients — the very call CheckClaim makes — so the check can never
// succeed on precisely the token whose expiry matters most. Collapsing that
// to nil meant the "already expired" warnings never fired (transfer's and
// consolidate's own anyExpired/expiredSources logic only ever saw wallets
// that hadn't expired yet): the user confirmed, then hit exit 7 — or, for a
// consolidate mixing a dead source with healthy ones, silently stranded the
// healthy part inside the merged, now-dead token. The real deadline is
// unknowable from a rejection, so this reports one that has certainly
// passed.
func expiresAtFromCheck(result *nipcash.CheckClaimResult, err error) *int64 {
	if err != nil {
		if isExpiredWalletErr(err) {
			passed := time.Now().Unix() - 1
			return &passed
		}
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

	sum := ledger.SumAmounts(group)
	message := fmt.Sprintf("Consolidate %d tokens (%s) then send %s %s?",
		len(group), output.FormatAmount(int64(sum)), output.FormatAmount(int64(sendAmount)), targetClause(toValue, target.Kind))
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
			output.Notef(jsonMode, "One source is expired — merge succeeds but the transfer onward will fail; funds land in a new held token instead.")
		} else if w := expiryWarningSuffix(earliest, "transfer"); w != "" {
			output.Notef(jsonMode, "%s", w)
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
		// The transfer's actual source here is the interim wallet, which
		// TransferFromSources minted under the caller's own pubkey
		// (InterimIdentity above) — so any remainder is pubkey-mode too.
		// "" for originalToken: the interim wallet was never shown to the
		// user or saved, so there is nothing to fall back to — and
		// nothing to fall back to is ever needed here, since an in-place
		// reassignment of the interim wallet always arrives with
		// NewWalletToken already populated (see this function's own
		// comment above on TransferFromSources' own patching).
		return printAndSaveTransferResult(cmd, l, result.Transfer, sendAmount, toValue, target, sourceIDs, ledger.Entry{IdentityRequired: ptrTo(true), MinterPubkey: sharedMinter(group)}, "")
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
		newEntry, addErr := l.Add(ledger.Entry{
			Token:        partial.Consolidated.NewWalletToken,
			WalletPubkey: partial.Consolidated.NewWalletPubkey,
			AmountMillis: &partial.Consolidated.AmountMillis,
			Verified:     true,
			MinterPubkey: sharedMinter(group),
		})
		if addErr == nil {
			addErr = l.Save()
		}
		if addErr != nil {
			// Both the interim consolidate AND the transfer onward from it
			// are done for — the interim wallet is the only place this
			// money is now reachable, and this save failing means even
			// cashctl's own in-memory record of it dies with this process.
			return reportUnsavedResult(cmd, addErr, "Transfer (interim consolidate)", entryRecoveryHint(newEntry))
		}
		return classifyCashTokenNWCErr(cmd, ffsErr)
	}

	return classifyCashTokenNWCErr(cmd, ffsErr)
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
// needs — can be assembled here.
//
// originalToken is the token this transfer actually acted on (the held
// entry's own token for the plain path; "" for the auto-consolidate path,
// where it's never needed — see that call site's own comment). It's the
// fallback half of transferResult.RecipientToken(originalToken): an EXACT
// full-amount transfer (SplitAmount == CurrentAmount, or omitted entirely)
// reassigns the source wallet IN PLACE — no new wallet is minted, so
// NewWalletToken comes back "" — but the recipient still needs a copy of
// SOME token string to ever reach that wallet again. The only such string
// that still exists is originalToken itself, still valid, now registered
// to the new identity (NIP-CASH §Transferring and Splitting a Slice).
// Getting this wrong is a fund-loss bug, not a display nit: printing
// nothing here for that case means the transfer's whole result — the
// wallet's new owner has no way to ever spend it — cannot be recovered
// from output alone, for BOTH bearer and pubkey/npub/connection targets.
//
// remainderMode carries the credential-mode fields (see credentialModeOf)
// the remainder entry, if any, is saved with — cash_transfer leaves a
// split's remainder under the SAME identity the source had, so it has to
// be spendable the same way the source was.
func printAndSaveTransferResult(cmd *cobra.Command, l *ledger.Ledger, transferResult *nipcash.CashTransferResult, sentAmount uint64, toValue string, target credential.ResolvedTarget, consolidatedFrom []string, remainderMode ledger.Entry, originalToken string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	l.AppendHistory("transfer", fmt.Sprintf("transferred %s %s", output.FormatAmount(int64(sentAmount)), targetClause(toValue, target.Kind)))

	var remainderEntry *ledger.Entry
	if transferResult.RemainderWalletToken != "" {
		remainder := remainderMode
		remainder.Token = transferResult.RemainderWalletToken
		remainder.WalletPubkey = transferResult.RemainderWalletPubkey
		remainderEntry, _ = l.Add(remainder)
		if transferResult.RemainingAmountMillis != nil {
			remainderEntry.AmountMillis = transferResult.RemainingAmountMillis
		}
		if remainderEntry.AmountMillis != nil {
			l.AppendHistory("transfer", fmt.Sprintf("kept remainder of %s as a new token", output.FormatAmount(int64(*remainderEntry.AmountMillis))))
		} else {
			l.AppendHistory("transfer", "kept remainder as a new token")
		}
	}

	// recipientToken resolves the "what does the recipient actually need"
	// question uniformly for a split (transferResult.NewWalletToken, a
	// genuinely new wallet) and an in-place full-amount reassignment
	// (originalToken, still valid, now under the new identity) — see this
	// function's own doc comment. "" only when originalToken itself was
	// never given (the auto-consolidate call site). Computed BEFORE
	// Save() below so a save failure can still report it — the Hub-side
	// transfer is already done by this point regardless of whether l.Save
	// succeeds, and this string is the only way that result is ever
	// recoverable once the process exits.
	recipientToken := transferResult.RecipientToken(originalToken)

	// For a bearer target there's no recipient identity — the recipient
	// is whoever holds the combined token#secret string, so that's what
	// gets displayed/returned in its place. toValue itself (the literal
	// "cash"/user input) still goes to AppendHistory above unchanged —
	// only this display/--json substitution uses the assembled string.
	_, isBearer := target.Target.(*nipcash.BearerTarget)
	var cashToSend string
	if isBearer && recipientToken != "" {
		bt := target.Target.(*nipcash.BearerTarget)
		cashToSend = fmt.Sprintf("%s#%s", recipientToken, bt.Secret())
	}

	if err := l.Save(); err != nil {
		handoff := cashToSend
		if handoff == "" {
			handoff = recipientToken
		}
		var parts []string
		if handoff != "" {
			parts = append(parts, "the recipient's own — save this now, it will not be shown again: "+handoff)
		}
		if remainder := entryRecoveryString(remainderEntry); remainder != "" {
			parts = append(parts, "your own remainder: "+remainder)
		}
		recovery := ""
		if len(parts) > 0 {
			recovery = "Save these: " + strings.Join(parts, "; ")
		}
		return reportUnsavedResult(cmd, err, "Transfer", recovery)
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
		// recipient_token: the exact string a pubkey/npub/connection-key
		// recipient needs to `receive` this — distinct from cash_to_send
		// (bearer-only, carries a secret) so existing bearer-vs-not
		// consumers of cash_to_send never see it change shape.
		if !isBearer && recipientToken != "" {
			out["recipient_token"] = recipientToken
		}
		output.PrintJSON(out)
		return nil
	}
	// A remainder left over from a split is saved above regardless — never
	// narrated here. Same reasoning as the pre-transfer confirm message:
	// this is wallet mechanism, not something the user decided, and it's
	// always visible afterward via `cashctl wallet show`/`wallet balance`.
	//
	// The handoff value — cashToSend or recipientToken — always gets its
	// own line, never appended to a sentence: it's what `cashctl receive`
	// takes verbatim, and anything trailing it directly (a period, in an
	// earlier version of this message) risks getting copied along and
	// corrupting it.
	switch {
	case isBearer && cashToSend != "":
		fmt.Printf("Transferred %s as cash. Save this now, it won't be shown again:\ncashctl receive %s\n",
			output.FormatAmount(int64(sentAmount)), cashToSend)
	case isBearer:
		fmt.Printf("Transferred %s as cash.\n", output.FormatAmount(int64(sentAmount)))
	default:
		fmt.Printf("Transferred %s %s.\n", output.FormatAmount(int64(sentAmount)), targetClause(toValue, target.Kind))
		if recipientToken != "" {
			fmt.Printf("Give this to them: cashctl receive %s\n", recipientToken)
		}
	}
	return nil
}

// credentialModeOf returns an Entry carrying only src's credential-mode
// fields — exactly what resolveCredential reads to decide how to spend it
// (bearer secret, identity requirement, connection-key reference), and
// nothing else. lokihub's cash_transfer leaves a split's remainder under
// the source's SAME current identity (cash_transfer_controller.go carries
// RemainderIdentityType/Value over unchanged), so a bearer source's
// remainder is still spent by the very same secret. Without this, the
// remainder was saved with IdentityRequired unknown and no secret, so
// resolveCredential fell through to the local identity — which a
// bearer-only wallet never has — and the next spend failed with "run
// `cashctl init` first".
func credentialModeOf(src ledger.Entry) ledger.Entry {
	return ledger.Entry{
		IdentityRequired:        src.IdentityRequired,
		BearerSecret:            src.BearerSecret,
		ConnectionKeyPlatform:   src.ConnectionKeyPlatform,
		ConnectionKeyExternalID: src.ConnectionKeyExternalID,
		AttestationEventID:      src.AttestationEventID,
		IAPubkey:                src.IAPubkey,
	}
}

// disambiguateTransferArgs decides which of transfer's up-to-2 positional
// args is the target and which is the amount — sniffed by shape, not
// position: every real target (hex pubkey, npub1..., a name@domain,
// nconnection1..., "cash", or the pubkey:/connection: forms)
// fails output.ParseAmount, so anything that parses as a loki amount
// (whole or fractional, e.g. "500" or "0.5") can only ever be the amount.
// This is what lets `cashctl transfer 500` (no target at all) work instead
// of failing to parse "500" as a target, and — with two args — lets both
// `cashctl transfer 5000 <target>` (the documented order) and `cashctl
// transfer <target> 5000` (the old order) resolve the same way, since
// whichever side parses as an amount is the amount.
//
// When NEITHER side parses as an amount (a malformed amount, most often —
// "1.5x", "5 loki" — typo'd or over-precise), credential.LooksLikeTarget
// breaks the tie: exactly one side looking like a real target shape means
// the OTHER side is the (malformed) amount, so resolvePositionalOrFlagAmount
// reports THAT one as invalid — not the npub that happened to land in the
// amount slot under the old position-only fallback (confirmed live: the
// old code blamed the recipient, "npub1uem7...3dfp9 is not a valid amount
// in loki", for a typo in the OTHER argument). Genuinely ambiguous input
// (both or neither look like a target) falls back to the documented
// amount-first order, same as before.
//
// Pure and cobra-free so it's unit-testable directly, mirroring
// resolveConsolidateSources's own reasoning in cash_consolidate.go.
func disambiguateTransferArgs(args []string) (positionalTo, positionalAmount string) {
	looksLikeAmount := func(s string) bool {
		_, err := output.ParseAmount(s)
		return err == nil
	}
	switch len(args) {
	case 1:
		if looksLikeAmount(args[0]) {
			positionalAmount = args[0]
		} else {
			positionalTo = args[0]
		}
	case 2:
		amount0, amount1 := looksLikeAmount(args[0]), looksLikeAmount(args[1])
		target0, target1 := credential.LooksLikeTarget(args[0]), credential.LooksLikeTarget(args[1])
		switch {
		case amount0 && !amount1:
			positionalAmount, positionalTo = args[0], args[1]
		case amount1 && !amount0:
			positionalTo, positionalAmount = args[0], args[1]
		case target0 && !target1:
			positionalTo, positionalAmount = args[0], args[1]
		case target1 && !target0:
			positionalTo, positionalAmount = args[1], args[0]
		default:
			// Genuinely ambiguous (both or neither parse as an amount, AND
			// both or neither look like a target) — nothing left to sniff,
			// so fall back to the documented amount-first order.
			positionalAmount, positionalTo = args[0], args[1]
		}
	}
	return positionalTo, positionalAmount
}

func runCashTransfer(cmd *cobra.Command, args []string) error {
	if err := rejectConnectionFlag(cmd); err != nil {
		return err
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	toFlagValue, _ := cmd.Flags().GetString("to")
	amountFlagValue, _ := cmd.Flags().GetString("amount")

	positionalTo, positionalAmount := disambiguateTransferArgs(args)

	toValue, err := resolvePositionalOrFlag(cmd, positionalTo, "to", toFlagValue)
	if err != nil {
		return err
	}
	// An amount string was actually typed, as opposed to omitted — needed
	// below to tell "transfer 0 <target>"/"--amount 0" (a real, explicit
	// request for nothing) apart from a bare "transfer <target>" (amount
	// omitted, sentinel 0 meaning "the whole token" everywhere else in
	// this function): resolvePositionalOrFlagAmount itself can't make
	// that distinction — ParseAmount("0") succeeds same as an empty
	// string collapsing to its own "nothing given" 0 return.
	amountGiven := strings.TrimSpace(positionalAmount) != "" || strings.TrimSpace(amountFlagValue) != ""
	amountFlag, err := resolvePositionalOrFlagAmount(cmd, positionalAmount, "amount", amountFlagValue)
	if err != nil {
		return err
	}
	if amountGiven && amountFlag == 0 {
		// Confirmed live: without this, a computed amount of 0 (a rounding
		// bug, an unset variable defaulting to "0") silently transferred
		// the ENTIRE held token instead of failing — real money moved on
		// an input that was never a meaningful request to send anything.
		return output.UsageError(cmd, fmt.Errorf("amount must be greater than 0 (omit it entirely to send the whole held token)"))
	}
	// No target given at all, but an amount was: default to cash instead
	// of erroring — "just an amount, share the result with whoever" is a
	// complete, valid request, not a usage mistake. An explicit --to/
	// positional target still always wins when given.
	if toValue == "" {
		if amountFlag == 0 {
			return output.InvocationError(cmd, fmt.Errorf("a destination is required — pass it directly (cashctl transfer <target>) or via --to"))
		}
		toValue = "cash"
	}

	target, err := resolveTarget(cmd, toValue)
	if err != nil {
		return err
	}
	if shouldPrintResolvedTarget(target) {
		output.Notef(jsonMode, "  resolves to: %s", target.Resolved)
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
		// would otherwise report a confusing "not enough funds: you hold 0."
		plan, err := ledger.SelectForAmount(l.Held(), amountFlag)
		if err != nil {
			// Both error types' own Error() states their two amounts as
			// bare unlabeled numbers (see FundsFragmentedError's own doc
			// comment: ledger stays presentation-agnostic) — re-rendered
			// here with real units so "you hold 3 loki, need 5 loki" /
			// "you hold 45 loki total... the 5 loki you're sending" reads
			// as an amount, not meaningless integers. Insufficient funds
			// (the total itself falls short) is also a genuinely different
			// diagnosis from fragmentation (the total covers it, but no
			// single minter's tokens do) — conflating them used to call a
			// plain overdraft "fragmented across separate Hubs", which is
			// simply false when only one Hub was ever involved. invalid_input,
			// not usage: the amount requested is what's actually wrong here,
			// not how the command was invoked.
			var insuf *ledger.InsufficientFundsError
			if errors.As(err, &insuf) {
				return output.InvalidInputError(cmd, "", fmt.Errorf("not enough funds: you hold %s, need %s",
					output.FormatAmount(int64(insuf.TotalHeld)), output.FormatAmount(int64(insuf.Target))))
			}
			var frag *ledger.FundsFragmentedError
			if errors.As(err, &frag) {
				return output.UsageError(cmd, fmt.Errorf("%w: you hold %s total, but no single minter's tokens sum to the %s you're sending",
					ledger.ErrFundsFragmented, output.FormatAmount(int64(frag.TotalHeld)), output.FormatAmount(int64(frag.Target))))
			}
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

	var splitAmount *uint64
	sentAmount := amount
	sendAmount := amount
	if amountFlag > 0 {
		if amountFlag > amount {
			return output.UsageError(cmd, fmt.Errorf("can't send %s %s — held token only has %s", output.FormatAmount(int64(amountFlag)), targetClause(toValue, target.Kind), output.FormatAmount(int64(amount))))
		}
		splitAmount = &amountFlag
		sentAmount = amountFlag
		sendAmount = amountFlag
	}
	// Splitting off a partial amount and keeping the rest as a new token is
	// mechanism, not a decision — same as a Bitcoin wallet not asking
	// "keep the change?" before spending a UTXO bigger than the payment.
	// The confirmation only ever names what's actually being sent.
	message := transferConfirmMessage(sendAmount, toValue, target.Kind)
	if w := expiryWarningSuffix(expiresAt, "transfer"); w != "" {
		output.Notef(jsonMode, "%s", w)
	}
	// defaultYes=false: moves real money — never accept on a bare Enter.
	if !Confirm(cmd, false, message) {
		fmt.Println("Cancelled.")
		return nil
	}

	var result *nipcash.CashTransferResult
	err = WithSpinner(jsonMode, "Transferring...", func() error {
		r, cErr := spendBearerEntry(entry, cred, func(c nipcash.Credential) (*nipcash.CashTransferResult, error) {
			return client.CashTransfer(ctx, nipcash.CashTransferParams{
				Credential: c, To: target.Target, CurrentAmount: amount, SplitAmount: splitAmount,
			})
		})
		if cErr != nil {
			return cErr
		}
		result = r
		return nil
	})
	if err != nil {
		return classifyCashTokenNWCErr(cmd, err)
	}

	_ = l.SetStatus(entry.ID, ledger.StatusTransferred)
	// The remainder is a brand-new wallet the same Hub split off entry, so
	// it inherits entry's verified minter (see sharedMinter) — without this
	// a split remainder silently fell out of cash-selection's same-minter
	// grouping, and a later spend needing it together with a sibling
	// failed as "insufficient" despite the funds being right there.
	remainderMode := credentialModeOf(*entry)
	remainderMode.MinterPubkey = entry.MinterPubkey
	return printAndSaveTransferResult(cmd, l, result, sentAmount, toValue, target, nil, remainderMode, entry.Token)
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
