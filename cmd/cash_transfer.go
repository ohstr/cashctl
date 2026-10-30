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
		Long: `Sends value from a held cash token. An amount with no target makes a cash
note: a <token>#<secret> string to hand anyone.

--json fields:
  amount_millis            what the recipient's new bill holds
  identity_type/_value     who the new bill is bound to: pubkey + hex key, or
                           cash + the SHA-256 of the note's secret (not the secret)
  cash_to_send             a cash note to hand over; the only copy, also kept in
                           ` + "`wallet history --show-secrets`" + ` until claimed
  recipient_token          what a pubkey/npub/connection recipient receives
  new_wallet_pubkey/_token the new bill itself
  remaining_amount_millis  what stays with you, as remainder_entry (saved
                           already; verified: false until its Hub confirms the
                           amount on its next use)
  target_resolved          what a NIP-05 or nconnection target resolved to
  consolidated_from        held tokens merged first to reach the amount`,
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
	cmd.Flags().Bool("no-wait", false, "exit 8 at once when the Hub gives no clear answer, instead of waiting (up to 62 s) to look the call up")
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
		line, err := PromptLine(fmt.Sprintf("This connection doesn't specify who to trust as Identity Authority for %s. Enter one (hex pubkey or name@domain): ", platform))
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
// sentence: "as cash" for a cash-mode target (no destination was ever given,
// so the result is redeemable by whoever ends up holding it), or "to
// <toValue> (pubkey)" / "to <toValue> as web identity cash" for a
// resolved recipient — matching whichever Kind credential.ParseTarget/
// ResolveConnectionTarget assigned. "Cash"/"(pubkey)"/"web identity
// cash" deliberately avoid the word "token" (or any other developer-facing
// jargon) in every user-facing message: the string this all builds up to
// (recipientToken, or the combined cash-mode <token>#<secret>) is real,
// spendable money, not a code — see printAndSaveTransferResult's own
// framing of it. Shared by the plain transfer path and
// transferWithAutoConsolidate so both name the destination identically.
// historyTargetClause is targetClause for the PERSISTED action log.
//
// targetClause interpolates the raw --to value, which is right for the confirm
// prompt and the success line — showing someone the value they just typed is what
// makes a confirmation mean anything. It is wrong for the action log, which keeps
// that text on disk and reprints it on every `wallet history`: an nconnection1...
// target carries its own dialing secret as a single TLV-packed blob with no
// substring that is safe to reveal, which is exactly why RedactSecretInput strips
// those wholesale (D-CLI-5). The user typing it is not new exposure; persisting
// and redisplaying it is.
//
// ResolvedTarget.Resolved is the public description for that case — platform plus
// Identity Authority — and is what belongs in a durable record. Where it is empty,
// which is the explicit connection:<platform>:<external-id>:<ia-pubkey> form and
// carries no secret, the value goes through RedactSecretInput so the one existing
// policy decides rather than a second rule being invented here.
//
// Deliberately NOT a blanket use of Resolved, and deliberately NOT redaction
// applied to every kind:
//
//   - The cash branch of targetClause interpolates no value at all and must stay
//     that way: whatever a cash target carries, a spending secret never belongs
//     in the log.
//   - RedactSecretInput blanks a bare 64-hex string entirely, and a pubkey target
//     IS bare 64-hex. Applying it outside the connection branch would erase a
//     perfectly public destination from the history line.
func historyTargetClause(toValue string, target credential.ResolvedTarget) string {
	if target.Kind != credential.TargetKindConnection {
		return targetClause(toValue, target.Kind)
	}
	if target.Resolved != "" {
		return fmt.Sprintf("to %s as web identity cash", target.Resolved)
	}
	return targetClause(output.RedactSecretInput(toValue), target.Kind)
}

func targetClause(toValue string, kind credential.TargetKind) string {
	switch kind {
	case credential.TargetKindConnection:
		return fmt.Sprintf("to %s as web identity cash", toValue)
	case credential.TargetKindCash:
		return "as cash"
	default:
		return fmt.Sprintf("to %s (pubkey)", toValue)
	}
}

// transferConfirmMessage renders the plain (non-auto-consolidate)
// transfer's own confirm prompt. Only the cash-mode case spells out what "as
// cash" actually means (anyone who ends up holding it can redeem it): a
// named destination is self-explanatory ("to alice@example.com"), but a
// destination-less transfer is the one place a first-time user could
// otherwise agree to "Transfer 55 loki as cash?" without realizing that
// creates a single, one-shot, unrecoverable value they alone are
// responsible for handing off (see printAndSaveTransferResult's own
// "save this now" framing on the result side of the same transfer).
func transferConfirmMessage(sendAmount uint64, toValue string, kind credential.TargetKind) string {
	amt := output.FormatAmount(int64(sendAmount))
	if kind == credential.TargetKindCash {
		return fmt.Sprintf("Transfer %s as cash — anyone holding it can redeem it. Continue?", amt)
	}
	return fmt.Sprintf("Transfer %s %s?", amt, targetClause(toValue, kind))
}

// shouldPrintResolvedTarget decides whether a resolved target's Resolved
// string is worth showing before confirming anything: a NIP-05 lookup or an
// nconnection's IA benefits from review. A cash target has none (its secret is
// never in Resolved). Pure so it's unit-testable without a cobra.Command.
func shouldPrintResolvedTarget(target credential.ResolvedTarget) bool {
	return target.Resolved != ""
}

// fetchExpiresAt best-effort fetches token's current Hub-side expiry
// (nipcash.CheckClaimResult.ExpiresAt) — nil on any failure. Mirrors
// cash_redeem.go's own fetchRedeemPreview. The same generic Hub-side
// AppPermission.ExpiresAt check (nip47/permissions in lokihub) gates
// cash_transfer and cash_consolidate exactly the way it gates cash_redeem
// — NIP-CASH's wallet-level expires_at is shared across every recipient
// row and every money-moving method on that wallet, not something
// specific to redemption — so an about-to-expire token is just as much a
// "confirm, then get rejected" trap here as it is on redeem. Deliberately
// doesn't reuse cash_redeem.go's own redeemPreview/previewSuffix: those
// also carry a fee quote that has no transfer/consolidate analogue (only
// cash_redeem charges one).
//
// Feeds two different uses in its callers: an optional confirmation-prompt
// warning (unchanged), and — for a multi-source merge — the signal that
// decides whether a batch gets refused outright for mixing an expired
// source with a healthy one (cash_consolidate.go's consolidateItems,
// this file's transferWithAutoConsolidate). The function itself doesn't
// know or care which use its caller puts the answer to.
func fetchExpiresAt(cmd *cobra.Command, token string) *int64 {
	// Derived from the token string itself: this runs before any ledger entry
	// exists, and bill methods authorize per item now.
	_, embeddedSecret := nipcash.SplitCashSliceString(token)
	cred, credErr := credentialForToken(cmd, embeddedSecret)
	if credErr != nil {
		return nil
	}
	myPub, _ := signerPubKeyHex(cmd, nil)
	return fetchExpiresAtWithCredential(cmd, token, cred, myPub)
}

// fetchExpiresAtWithCredential is fetchExpiresAt with cred supplied by the
// caller instead of derived from the token's own embedded secret / local
// identity fallback — needed by a caller that already resolved the real
// credential itself, since that derivation is only correct for a local
// pubkey-mode entry. consolidateItems' verbose <token>:<amount>:<credential>
// source form is the one caller whose credential can be
// pubkey/connection-key/cash, never just "local identity" — probing it
// with fetchExpiresAt's own derivation would silently ask with the wrong
// credential and read as permanently "unknown".
//
// matchPubHex is the pubkey cred signs as, which the Hub's reply is matched
// against; a mismatch reads as "no slice" and the expiry as unknown.
func fetchExpiresAtWithCredential(cmd *cobra.Command, token string, cred nipcash.Credential, matchPubHex string) *int64 {
	tok, err := nipcash.Decode(token)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := dialBill(ctx, token)
	if err != nil {
		return nil
	}
	defer client.Close()
	result, err := client.CheckClaim(ctx, cred, tok)
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
	if !result.Claimable && result.Reason == "expired" {
		passed := time.Now().Unix() - 1
		return &passed
	}
	return expiryPtr(result.Row.ExpiresAt)
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
	//
	// The sources merge under the local identity — or, when a source is a cash
	// note or there is no identity, under a throwaway key made for this merge
	// alone, which then spends any remainder (I-26, cash_merge.go).
	hasCashNote := false
	for i := range group {
		if isCashNote(&group[i]) {
			hasCashNote = true
		}
	}
	myPub, idErr := localPubKeyHex(cmd)
	interimPub, throwawayPriv := myPub, ""
	if hasCashNote || idErr != nil {
		priv, pub, kErr := newThrowawayKey()
		if kErr != nil {
			return output.RuntimeError(cmd, kErr)
		}
		throwawayPriv, interimPub = priv, pub
	}

	jsonMode, _ := cmd.Flags().GetBool("json")
	yesFlag, _ := cmd.Flags().GetBool("yes")

	sum := ledger.SumAmounts(group)
	message := fmt.Sprintf("Consolidate %d tokens (%s), telling their Hub they share an owner, then send %s %s?",
		len(group), output.FormatAmount(int64(sum)), output.FormatAmount(int64(sendAmount)), targetClause(toValue, target.Kind))
	// Unconditional, not gated on !jsonMode && !yesFlag: this decides a
	// hard refusal below, which must fire under --json/--yes too — exactly
	// the mode a scripted auto-consolidate-then-transfer would otherwise
	// poison a healthy source in with zero signal. Every entry in group is
	// pubkey-mode by construction (ledger.SelectForAmount's
	// ConsolidateFirst only draws from GroupableForConsolidation-eligible
	// entries), so fetchExpiresAt's own local-identity credential
	// derivation is already correct here.
	var earliest *int64
	var expiredCount, healthyCount int
	var expiredIDs []string
	_ = WithSpinner(jsonMode, "Checking expiry...", func() error {
		for i := range group {
			// Asked with each source's own credential: a cash note answers only
			// to its secret.
			var ea *int64
			if cred, cErr := resolveCredential(cmd, &group[i]); cErr == nil {
				myPub, _ := signerPubKeyHex(cmd, &group[i])
				ea = fetchExpiresAtWithCredential(cmd, group[i].Token, cred, myPub)
			}
			earliest = earliestExpiry(earliest, ea)
			if ea == nil {
				continue
			}
			if time.Until(time.Unix(*ea, 0)) <= 0 {
				expiredCount++
				expiredIDs = append(expiredIDs, group[i].ID)
			} else {
				healthyCount++
			}
		}
		return nil
	})
	if expiredCount > 0 && healthyCount > 0 {
		// Hard refusal, not just a warning: the interim cash_consolidate
		// merges these into one wallet whose own expiry is the earliest of
		// its sources (NIP-CASH §Consolidating Tokens) — a doomed source
		// mixed with a healthy one doesn't just fail, it kills the healthy
		// source's own good deadline too, the instant the merge lands.
		// Before Confirm, before any wire call: nothing has moved yet.
		// Same reasoning as cash_consolidate.go's own refusal; deliberately
		// doesn't fire when every source is expired (healthyCount == 0) —
		// that case is unaffected, still the Hub's own exit-7/auth decline.
		return output.InvalidInputError(cmd, strings.Join(expiredIDs, ","), fmt.Errorf(
			"%s already expired — auto-consolidating it to cover this amount would make the "+
				"whole result unusable too. Nothing was changed",
			strings.Join(expiredIDs, ",")))
	}
	// Cosmetic-only "expires soon, not yet" notice keeps its original
	// interactive-only gate — the probe above now always runs, but this
	// particular print shouldn't start appearing under --yes too.
	if !jsonMode && !yesFlag {
		if w := expiryWarningSuffix(earliest, "transfer"); w != "" {
			output.Notef(jsonMode, "%s", w)
		}
	}
	// defaultYes=false: moves real money — never accept on a bare Enter.
	if !Confirm(cmd, false, message) {
		output.Println("Cancelled.")
		return nil
	}

	// cash_consolidate takes no cash-mode source: move the cash notes onto the
	// throwaway key first; their entries then sign with it.
	var movedIDs []string
	if hasCashNote {
		ids := make([]string, len(group))
		for i := range group {
			ids[i] = group[i].ID
		}
		err := WithSpinner(jsonMode, "Moving cash notes for the merge...", func() error {
			var mErr error
			movedIDs, mErr = moveCashNotesToKey(cmd, l, ids, throwawayPriv, interimPub)
			return mErr
		})
		if err != nil {
			return err
		}
		for i := range group {
			if e, ok := l.Find(group[i].ID); ok {
				group[i] = *e
			}
		}
	}

	var sources []nipcash.Source
	sourceIDs := make([]string, len(group))
	err := WithSpinner(jsonMode, "Preparing sources...", func() error {
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

	// The merge and the send are separate, saved steps (mergeThenSend): the merged
	// bill is in the ledger before the send goes out, so a crash or lost reply
	// between the two never leaves it only in this process's memory (#96).
	tokens := make([]string, len(group))
	for i := range group {
		tokens[i] = group[i].Token
	}
	err = mergeThenSend(cmd, l, tokens, sources, sourceIDs, sum, sendAmount, throwawayPriv, interimPub, toValue, target)
	if err != nil && len(movedIDs) > 0 && output.AsCLIError(err).Code != output.CodeIndeterminate {
		// only bills still held: never ones a landed merge consumed
		if uErr := undoMoves(cmd, l, movedIDs); uErr != nil {
			err = withRecovery(err, uErr)
		}
	}
	if err != nil && isCrossHubDeclineClassified(err) {
		// The sources turned out to live under different Cash Hubs. cashctl cannot
		// know that locally: a bill's recovered minter identifies the NODE that
		// signed it, and one node can run many Cash Hubs. Reported as fragmentation,
		// which is what it is from the holder's side and the answer they can act on.
		return output.UsageError(cmd, fmt.Errorf("%w: you hold %s total, but these tokens come from different Cash Hubs and only same-hub tokens can be merged into the %s you're sending",
			ledger.ErrFundsFragmented,
			output.FormatAmount(int64(ledger.SumAmounts(l.Held()))),
			output.FormatAmount(int64(sendAmount))))
	}
	return err
}

// isCrossHubSourcesDecline reports whether err is a Hub refusing a consolidate
// because its sources belong to different Cash Hubs.
//
// Matched on the Hub's own message, which is not ideal and is done deliberately:
// NIP-CASH gives this no distinct error code (it arrives as BAD_REQUEST, shared with
// every other malformed request), and the alternative is showing a holder an internal
// rule about grouping when what they need to know is that their funds are split. If
// the Hub ever gains a specific code, this should key off that instead.
func isCrossHubSourcesDecline(err error) bool {
	var walletErr *relayclient.WalletError
	if !errors.As(err, &walletErr) {
		return false
	}
	return strings.Contains(strings.ToLower(walletErr.Message), "same cash hub")
}

// printAndSaveTransferResult applies a completed cash_transfer call's
// ledger effects and renders cashctl's own output — shared by the plain
// single-source path and transferWithAutoConsolidate, since both end in
// exactly the same "one CashTransferResult, maybe a remainder" shape.
// target is accepted (not just its Resolved string) so a cash-mode target's
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
// from output alone, for BOTH cash-mode and pubkey/npub/connection targets.
//
// remainderMode carries the credential-mode fields (see credentialModeOf)
// the remainder entry, if any, is saved with — cash_transfer leaves a
// split's remainder under the SAME identity the source had, so it has to
// be spendable the same way the source was.
//
// call is the call that produced the result (nil for a merge that sent straight
// to the recipient, whose call is closed already): it is closed here, in the
// same Save as the entries and hand-off.
func printAndSaveTransferResult(cmd *cobra.Command, l *ledger.Ledger, transferResult *nipcash.CashTransferResult, call *ledger.Call, sentAmount uint64, toValue string, target credential.ResolvedTarget, consolidatedFrom []string, remainderMode ledger.Entry, originalToken string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	closeCall(l, call, ledger.CallDone, "")
	l.AppendHistory("transfer", fmt.Sprintf("transferred %s %s", output.FormatAmount(int64(sentAmount)), historyTargetClause(toValue, target)))

	var remainderEntry *ledger.Entry
	if transferResult.RemainderWalletToken != "" {
		remainder := remainderMode
		remainder.Token = transferResult.RemainderWalletToken
		remainder.WalletPubkey = transferResult.RemainderWalletPubkey
		remainder.Verified, remainder.MinterPubkey = returnedBillCheck(remainder.Token, remainderMode.MinterPubkey)
		if !remainder.Verified {
			output.Notef(jsonMode, unverifiedReturnNote)
		}
		remainder.CreatedByCall = ownCallID(call)
		remainderEntry, _ = l.Add(remainder)
		// Bounded before it is recorded, like every other Hub-supplied amount that
		// reaches the ledger (validateQuotedAmounts, cash_receive.go). One figure past
		// math.MaxInt64 goes negative at the FormatAmount call three lines down and at
		// summarizeHeldTokens' running total, so it would report a negative balance for
		// the whole wallet, not just this remainder.
		//
		// Dropped rather than refused, unlike the quote paths: the transfer has ALREADY
		// happened by the time this runs, so erroring would report a failure that did
		// not occur. Leaving the amount nil is a state this code already handles — the
		// history line below and `wallet show` both degrade to naming the token without
		// a figure, which is the honest answer when the only source for it is a number
		// that cannot be true.
		//
		// The signed-provenance ceiling deliberately does NOT apply here: the remainder
		// is a NEW wallet with its own token and its own mint signature, which nothing
		// on this path has verified, and the original bill's attestation covers the
		// original wallet rather than this one.
		if transferResult.RemainingAmountMillis != nil && *transferResult.RemainingAmountMillis <= maxSaneAmountMillis {
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
	recipientToken, rtErr := transferResult.RecipientToken(originalToken)
	if rtErr != nil {
		// A spin-off the Hub named without its token: the bill exists, but the
		// reply cannot hand it over. Recorded as sent; the Hub still holds it.
		recipientToken = ""
	}

	// For a cash-mode target there's no recipient identity — the recipient
	// is whoever holds the combined token#secret string, so that's what
	// gets displayed/returned in its place. toValue itself (the literal
	// "cash"/user input) still goes to AppendHistory above unchanged —
	// only this display/--json substitution uses the assembled string.
	_, isCash := target.Target.(*nipcash.CashTarget)
	var cashToSend string
	if isCash && recipientToken != "" {
		bt := target.Target.(*nipcash.CashTarget)
		cashToSend = fmt.Sprintf("%s#%s", recipientToken, bt.Secret())
	}
	// Kept permanently, in the same save as the rest of this transfer (I-27):
	// printed once, a handoff lost before delivery was unrecoverable.
	recordHandoff(l, cashToSend, recipientToken, sentAmount, handoffTarget(toValue, target))

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
	// Change left on a merge's throwaway key becomes a cash note like any other
	// (I-26); if that cannot finish now, the next command retries it.
	var remainderRecovery string
	if remainderEntry != nil && remainderEntry.ThrowawayKey != "" {
		if _, unsaved := toCashNotes(cmd, l, []string{remainderEntry.ID}); len(unsaved) > 0 {
			remainderRecovery = strings.Join(unsaved, " ")
		}
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
		// (cash-mode-only, carries a secret) so existing cash-vs-not
		// consumers of cash_to_send never see it change shape.
		if !isCash && recipientToken != "" {
			out["recipient_token"] = recipientToken
		}
		// The remainder became a cash note on the Hub, but saving that failed:
		// this string is the only way back to it.
		if remainderRecovery != "" {
			out["recovery"] = remainderRecovery
		}
		output.PrintJSON(out)
		return nil
	}
	defer printRemainderRecovery(remainderRecovery)
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
	case isCash && cashToSend != "":
		output.Printf("Transferred %s as cash. Give this to the recipient (kept in `cashctl wallet history` until claimed):\ncashctl receive %s\n",
			output.FormatAmount(int64(sentAmount)), cashToSend)
	case isCash:
		output.Printf("Transferred %s as cash.\n", output.FormatAmount(int64(sentAmount)))
	default:
		output.Printf("Transferred %s %s.\n", output.FormatAmount(int64(sentAmount)), targetClause(toValue, target.Kind))
		if recipientToken != "" {
			output.Printf("Give this to them: cashctl receive %s\n", recipientToken)
		}
	}
	return nil
}

// credentialModeOf returns an Entry carrying only src's credential-mode
// fields — exactly what resolveCredential reads to decide how to spend it
// (cash secret, identity requirement, connection-key reference), and
// nothing else. lokihub's cash_transfer leaves a split's remainder under
// the source's SAME current identity (cash_transfer_controller.go carries
// RemainderIdentityType/Value over unchanged), so a cash-mode source's
// remainder is still spent by the very same secret. Without this, the
// remainder was saved with IdentityRequired unknown and no secret, so
// resolveCredential fell through to the local identity — which a
// cash-mode-only wallet never has — and the next spend failed with "run
// `cashctl init` first".
func credentialModeOf(src ledger.Entry) ledger.Entry {
	return ledger.Entry{
		IdentityRequired:        src.IdentityRequired,
		CashSecret:              src.CashSecret,
		CashProtection:          src.CashProtection,
		ConnectionKeyPlatform:   src.ConnectionKeyPlatform,
		ConnectionKeyExternalID: src.ConnectionKeyExternalID,
		AttestationEventID:      src.AttestationEventID,
		IAPubkey:                src.IAPubkey,
		// The remainder stays under the source's SAME identity, so it is addressed
		// to the same key: carry it so a later NOT_FOUND to that key is told from
		// one to a rotated-away key (R-CP-RECLAIM). Empty for a cash-mode source,
		// which has no addressee — the conservative default.
		IdentityPubkey: src.IdentityPubkey,
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
			return output.InvocationError(cmd, fmt.Errorf("say what to send: an amount alone makes a cash note anyone can take (cashctl transfer <amount>), or add a destination (cashctl transfer <amount> <target>, or <target> alone for the whole held token; or --amount/--to)"))
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
	// A merge interrupted before its move was confirmed is settled first (I-26).
	if err := settleBeforeSpend(cmd, l); err != nil {
		return err
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
		plan, err := ledger.SelectForAmount(unlockedHeld(l), amountFlag)
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

	if err := refuseLocked(cmd, l, []string{entry.ID}); err != nil {
		return err
	}
	cred, err := resolveCredential(cmd, entry)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var client *nipcashclient.Client
	err = WithSpinner(jsonMode, "Connecting...", func() error {
		c, cErr := dialBill(ctx, entry.Token)
		if cErr != nil {
			return cErr
		}
		client = c
		return nil
	})
	if err != nil {
		return classifyCashTokenNWCErr(cmd, err)
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
		output.Println("Cancelled.")
		return nil
	}

	// A partial split of a cash note gives the remainder a fresh secret of its
	// own (remainder_commitment), never the source's, which the sender's past
	// holders may know. Both secrets are written ahead with the call.
	var remainderTarget *nipcash.CashTarget
	if splitAmount != nil && *splitAmount < amount && isCashNote(entry) {
		if remainderTarget, err = nipcash.NewCashTarget(); err != nil {
			return output.RuntimeError(cmd, err)
		}
	}
	purpose := purposeTransfer
	if splitAmount != nil && *splitAmount < amount {
		purpose = purposeSplit
	}
	kind, authPub, authSecret := authorizerOf(cmd, entry, cred)
	spec := callSpec{Purpose: purpose, SourceEntryIDs: []string{entry.ID}, AuthorizerKind: kind,
		AuthorizerPubkey: authPub, AuthorizerSecret: authSecret, TargetCredential: entry.Token}
	entryID := entry.ID
	var result *nipcash.CashTransferResult
	var call *ledger.Call
	err = WithSpinner(jsonMode, "Transferring...", func() error {
		var cErr error
		result, call, cErr = placeTransfer(ctx, cmd, l, client, spec, nipcash.CashTransferParams{
			Credential: cred, To: target.Target, CurrentAmount: amount, SplitAmount: splitAmount,
			RemainderTarget: remainderTarget,
		})
		return cErr
	})
	if err != nil {
		return err
	}
	entry, _ = l.Find(entryID)

	_ = l.SetStatus(entry.ID, ledger.StatusTransferred)
	// The remainder is a brand-new wallet the same Hub split off entry, so
	// it inherits entry's verified minter (see sharedMinter) — without this
	// a split remainder silently fell out of cash-selection's same-minter
	// grouping, and a later spend needing it together with a sibling
	// failed as "insufficient" despite the funds being right there.
	remainderMode := credentialModeOf(*entry)
	remainderMode.MinterPubkey = entry.MinterPubkey
	if remainderTarget != nil {
		remainderMode.CashSecret = remainderTarget.Secret()
		remainderMode.CashProtection = ledger.CashProtected
	}
	return printAndSaveTransferResult(cmd, l, result, call, sentAmount, toValue, target, nil, remainderMode, entry.Token)
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

// printRemainderRecovery tells the holder about a remainder that became a cash
// note on the Hub but could not be saved, on its own line so it can be copied.
func printRemainderRecovery(recovery string) {
	if recovery == "" {
		return
	}
	output.Printf("Your remainder became a cash note, but saving it locally failed. Save this now, it will not be shown again:\n%s\n", recovery)
}
