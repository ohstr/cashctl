package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/output"
)

// explainNoAnswer turns an omission into text a person can act on, and reports
// whether err was one.
//
// The private transport's equivalent of the silence classifyCashTokenNWCErr also
// handles, but it arrives far more often, because it is equally what a caller
// gets for a bill that simply does not name them — the single most likely mistake
// with a pasted token.
//
// The Hub genuinely cannot tell these cases apart and must not try: an omission
// is information-free by design, since an answer distinguishing "no such bill"
// from "not yours" would confirm a guessed bill exists. So this names every
// possibility and asserts none. Picking one — "expired", say — would present a
// guess as the Hub's own finding.
//
// One function because two call sites need the same words: the classified path,
// and `decode --check`, which deliberately bypasses classification to stay soft.
func explainNoAnswer(err error) (error, bool) {
	var notServed *nipcashclient.NotServedError
	if !errors.As(err, &notServed) {
		return err, false
	}
	return fmt.Errorf(
		"the Hub gave no answer for this bill. Any of these produces exactly this result, "+
			"and the Hub deliberately does not distinguish them: the bill does not name you, "+
			"it has already been spent, it has expired, or the Hub is unreachable. "+
			"If you expected it to be yours, check the identity it was addressed to; "+
			"do not resend a redeem or transfer blind, since an unanswered spend may already have happened: %w", err), true
}

// classifyNWCErr turns a wallet-call error into cashctl's own classified
// *CLIError: a *relayclient.WalletError becomes output.NWCError (plain-
// language translation, correct exit code); anything else (a dial/network
// failure) becomes output.NetworkError.
func classifyNWCErr(cmd *cobra.Command, err error) error {
	var walletErr *relayclient.WalletError
	if errors.As(err, &walletErr) {
		return output.NWCError(cmd, walletErr)
	}
	return output.NetworkError(cmd, err)
}

// classifyCashTokenNWCErr is classifyNWCErr's counterpart for a call that
// checks or spends a cash token's own claim (CheckClaim, CashRedeem,
// CashTransfer, ListRecipients — anything dialing entry.Token itself)
// rather than a registered wallet connection — see
// output.NWCErrorForCashToken's own doc comment for why EXPIRED needs
// different text there.
func classifyCashTokenNWCErr(cmd *cobra.Command, err error) error {
	var walletErr *relayclient.WalletError
	if errors.As(err, &walletErr) {
		return output.NWCErrorForCashToken(cmd, walletErr)
	}
	if err, ok := explainNoAnswer(err); ok {
		return output.NetworkError(cmd, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// A Hub deletes a bill once nothing is left on it, and answers
		// nothing at all about one it no longer has -- that silence is
		// deliberate, so a spent bill cannot be told apart from a pubkey the
		// Hub never served. The bare "network error" this used to produce
		// pointed the user at their connection when the likeliest cause is
		// that the bill is gone.
		//
		// Still classified as a network failure, not not_found: no answer is
		// genuinely ambiguous. An unreachable Hub looks identical from here,
		// and reporting "spent" on a Hub that is merely down would tell
		// someone their money is gone when it is not.
		return output.NetworkError(cmd, fmt.Errorf(
			"the Hub did not answer for this bill: it has most likely been spent or has expired, "+
				"since a Hub stops answering about a bill once nothing is left on it. "+
				"If you believe it is still good, the Hub may simply be unreachable -- try again: %w", err))
	}
	return output.NetworkError(cmd, err)
}

// isAmbiguousDeliveryErr reports whether err is the shape of a specific,
// known Hub/SDK interop failure (see docs/private's own audit of this):
// the Hub answers a cash_consolidate/mint_cash call for real, but this
// client then fails to decrypt/parse the delivery it sent back —
// "decrypt delivery" is nipcash's own error prefix for exactly that
// decode step (nipcash.CashConsolidateParams.ParseResult and its
// siblings), which only ever runs on a raw response the Hub actually
// returned. A plain decline (bad request, restricted, an ordinary
// network failure) never reaches that far, so matching on it isn't a
// guess about WHETHER the Hub executed the request — only about whether
// a caller holding sources it's about to give up on should be warned
// that it might have.
//
// There is deliberately no attempt here to go further and DECIDE that
// the sources were consumed (e.g. marking them accordingly): the SDK
// exposes no signal more specific than this message for cash_consolidate
// (unlike RekeyCashSlice/TransferFromSources' own typed
// PartialProgressError for their composite calls), so guessing either
// way is worse than saying plainly that it's unknown — guessing
// "consumed" when it wasn't would lock a caller out of genuinely
// still-spendable funds, exactly the class of bug this whole check
// exists to avoid on the other side.
func isAmbiguousDeliveryErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "decrypt delivery")
}

// warnAmbiguousDelivery wraps err with the honest, actionable uncertainty
// isAmbiguousDeliveryErr's own doc comment describes, when it applies —
// unchanged otherwise. Wraps err itself (not classifyNWCErr's output), so
// the added text survives into CLIError.Err.Error(), what EmitError
// actually prints in both modes — a naming distinct from "warn" that
// doesn't classify anything: exit code / retryable stay exactly what err
// already implied.
func warnAmbiguousDelivery(err error, sourceIDs []string) error {
	if !isAmbiguousDeliveryErr(err) {
		return err
	}
	which := "these sources"
	if len(sourceIDs) > 0 {
		which = strings.Join(sourceIDs, ", ")
	}
	return fmt.Errorf("%w — the Hub may have already merged %s even though this reply couldn't be read; check each with `cashctl cash list-recipients --token <id>` or `cashctl decode --check <token>` before assuming it's still spendable, and don't just retry", err, which)
}
