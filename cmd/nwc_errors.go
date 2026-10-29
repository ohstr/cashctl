package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ohstr/nmilat/nipcash"
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
// classifyWalletCallErr is classifyNWCErr for an ordinary NIP-47 call on a
// registered wallet: a method the connection is not allowed (RESTRICTED) or
// the wallet does not offer (NOT_IMPLEMENTED) also says where to see what it
// does allow, so the decline is not read as a login or permission failure.
func classifyWalletCallErr(cmd *cobra.Command, err error) error {
	classified := classifyNWCErr(cmd, err)
	if ce, ok := classified.(*output.CLIError); ok && (ce.NWCCode == "RESTRICTED" || ce.NWCCode == "NOT_IMPLEMENTED") {
		ce.Hint = "`cashctl wallet get-info` lists the methods this connection allows."
	}
	return classified
}

func classifyNWCErr(cmd *cobra.Command, err error) error {
	var walletErr *relayclient.WalletError
	if errors.As(err, &walletErr) {
		return output.NWCError(cmd, walletErr)
	}
	return output.NetworkError(cmd, err)
}

// classifyCashTokenNWCErr is classifyNWCErr's counterpart for a call that
// READS a cash token's own claim (CheckClaim, cash_status — anything dialing
// entry.Token itself) rather than a registered wallet connection — see
// output.NWCErrorForCashToken's own doc comment for why EXPIRED needs
// different text there. A spend goes through classifyCashSpendErr instead:
// silence after a read is safe to retry, silence after a spend is not.
func classifyCashTokenNWCErr(cmd *cobra.Command, err error) error {
	if isProvenanceRefusal(err) {
		return output.InvalidInputError(cmd, "", err)
	}
	if errors.Is(err, nipcashclient.ErrHubUnusable) {
		return hubUnusableError(cmd, err)
	}
	if explained, ok := explainCashSecretRefusal(err); ok {
		return output.InvalidInputError(cmd, "", explained)
	}
	var walletErr *relayclient.WalletError
	if errors.As(err, &walletErr) {
		return output.NWCErrorForCashToken(cmd, walletErr)
	}
	if err, ok := explainNoAnswer(err); ok {
		return output.NetworkError(cmd, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// No answer in time says nothing about the bill: a Hub that is down
		// looks exactly like one that stopped answering about a spent bill, so
		// this must not lean either way (NIP-CASH §Cash Status: no answer is
		// indeterminate and MUST NOT be reported as spent). Retryable, because
		// every caller of this function only reads.
		return output.NetworkError(cmd, fmt.Errorf(
			"the Hub did not answer in time, which says nothing about this bill's state: "+
				"the Hub may be unreachable, or it may no longer hold the bill. "+
				"Asking again is safe: %w", err))
	}
	return output.NetworkError(cmd, err)
}

// notSentError marks a spend failure known to have happened before the
// request could reach the Hub (the dial itself failed), which is the one
// failure classifyCashSpendErr may call retryable.
type notSentError struct{ err error }

func (e *notSentError) Error() string { return e.err.Error() }
func (e *notSentError) Unwrap() error { return e.err }

// classifyCashSpendErr classifies the error of a redeem, transfer or
// consolidate. A Hub's decline is an answer and classified as usual; a
// failure to dial is network and safe to retry. Anything else came after the
// request may have gone out — a timeout, an omission, an incomplete or
// unreadable reply — and cannot be told apart from a spend that was applied
// and whose reply was lost, so it is indeterminate, never retryable
// (NIP-CASH: a client MUST NOT resend an omitted spend blind). verb names
// the operation; ids are the ledger entries to check with `cash status`.
func classifyCashSpendErr(cmd *cobra.Command, err error, verb string, ids ...string) error {
	var walletErr *relayclient.WalletError
	if errors.As(err, &walletErr) {
		return output.NWCErrorForCashToken(cmd, walletErr)
	}
	input := ""
	if len(ids) == 1 {
		input = ids[0]
	}
	if isProvenanceRefusal(err) {
		// Refused before any relay was contacted: nothing was sent.
		return output.InvalidInputError(cmd, input, err)
	}
	if errors.Is(err, nipcashclient.ErrHubUnusable) {
		return hubUnusableError(cmd, err)
	}
	if errors.Is(err, nipcashclient.ErrNoJournal) || errors.Is(err, nipcashclient.ErrNotSent) {
		return output.RuntimeError(cmd, fmt.Errorf("not placed — your money has not moved: %w", err))
	}
	var notSent *notSentError
	if errors.As(err, &notSent) || errors.Is(err, nipcashclient.ErrNoAnnouncement) {
		return output.NetworkError(cmd, err)
	}
	if isRejectedBeforeSend(err) {
		return output.InvalidInputError(cmd, input, fmt.Errorf("this %s was refused before it was sent, so nothing moved: %w", verb, err))
	}
	return output.IndeterminateError(cmd, input, fmt.Errorf(
		"the Hub gave no answer to this %s, so whether it was applied is unknown. "+
			"Do not resend it: check first with %s: %w", verb, cashStatusHint(ids), err))
}

// hubUnusableError is nmilat refusing to send anything to a Hub whose
// announcement cannot be used (NIP-CASH §Hub Announcement): nothing was sent.
func hubUnusableError(cmd *cobra.Command, err error) error {
	return output.WithReason(output.RuntimeError(cmd, fmt.Errorf(
		"this Hub's announcement can't be used (it names no request window of at least 60 seconds, or its newest announcement was refused), so cashctl sent it nothing — your money has not moved: %w", err)),
		nipcashclient.ReasonHubUnusable)
}

// rejectedBeforeSend are the SDK's own checks on a request it then never
// sends: an answer as definite as a Hub's decline.
var rejectedBeforeSend = []error{
	nipcash.ErrMixedCashAllocation,
	nipcash.ErrTooFewSources,
	nipcash.ErrCashSource,
	nipcash.ErrConsolidateTargetInvalid,
	nipcashclient.ErrItemTooLarge,
	nipcashclient.ErrInterimIdentityNotPubkey,
	nipcashclient.ErrNewTargetNotCash,
}

func isRejectedBeforeSend(err error) bool {
	for _, target := range rejectedBeforeSend {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// cashStatusHint names the command that settles an indeterminate spend.
func cashStatusHint(ids []string) string {
	if len(ids) == 0 {
		return "`cashctl cash status`"
	}
	hints := make([]string, len(ids))
	for i, id := range ids {
		hints[i] = fmt.Sprintf("`cashctl cash status --token %s`", id)
	}
	return strings.Join(hints, ", ")
}

// explainCashSecretRefusal rewrites a Hub's refusal to answer about a cash-mode
// bill sent without its cash secret (NIP-CASH §Bearer Items, a BAD_REQUEST)
// into what to do about it. A wrong secret is not explained here: the Hub
// answers it NOT_FOUND, which for a held bill means it is no longer yours
// (someone holding the note re-keyed or spent it), and stays not_found. A
// pasted `<token>#<secret>` gets its own message through CheckClaim's
// ErrClaimNotFound.
func explainCashSecretRefusal(err error) (error, bool) {
	var walletErr *relayclient.WalletError
	if !errors.As(err, &walletErr) || walletErr.Code != "BAD_REQUEST" {
		return err, false
	}
	if strings.Contains(walletErr.Message, "neither a slice proof nor a cash secret") {
		return fmt.Errorf("%w: %w", errNeedsCashSecret, err), true
	}
	return err, false
}

// errNeedsCashSecret is the answer to checking a cash-mode bill without its secret.
var errNeedsCashSecret = errors.New("this is a cash-mode bill, and its Hub answers about it only with its cash secret: " +
	"use the full `<token>#<secret>` string")

// isBareCashModeBill reports a cash-mode bill being checked without its secret. Any
// miss on it is the missing secret: the Hub answers about a cash-mode slice only
// to that secret, so a check signed by the local identity cannot match it.
func isBareCashModeBill(tok nipcash.Token, embeddedSecret string) bool {
	return embeddedSecret == "" && tok.IdentityRequired != nil && !*tok.IdentityRequired
}
