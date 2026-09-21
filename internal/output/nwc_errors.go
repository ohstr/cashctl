package output

import (
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"
)

// nwcErrorCode maps NIP-47's generic error codes (nip47.Err* in nmilat) to
// one of cashctl's own 7 ErrorCodes. Deliberately coarse — ncli's own
// discipline is not to invent new codes casually, so a wallet decline that
// doesn't obviously fit a more specific bucket falls back to CodeInternal
// rather than growing the taxonomy. The raw NWC code is never lost even
// then: NWCError attaches it as CLIError.NWCCode, surfaced verbatim in
// --json output (see EmitError) for an agent that needs finer-grained
// branching than cashctl's own exit codes provide.
var nwcErrorCode = map[string]ErrorCode{
	"BAD_REQUEST":  CodeInvalidInput,
	"NOT_FOUND":    CodeNotFound,
	"RESTRICTED":   CodeAuth,
	"UNAUTHORIZED": CodeAuth,
	"EXPIRED":      CodeAuth,
	"RATE_LIMITED": CodeConflict,
	// INSUFFICIENT_BALANCE, QUOTA_EXCEEDED, NOT_IMPLEMENTED,
	// UNSUPPORTED_ENCRYPTION, PAYMENT_FAILED, INTERNAL, OTHER: no bucket
	// above fits (they're legitimate wallet-side declines, not an input
	// mistake, an auth problem, or a retryable conflict) — CodeInternal.
}

// nwcErrorMessages translates NIP-47's error codes into plain language for
// human/table mode. --json mode never uses this for its "error" field —
// it preserves the wallet's own raw message instead, via
// CLIError.RawMessage (see cashctl-plan.md's "Human errors, not protocol
// errors" principle) — plus NWCCode, for an agent that wants to branch on
// the code rather than parse text.
var nwcErrorMessages = map[string]string{
	"RATE_LIMITED":           "You're making requests too quickly. Wait a moment and try again.",
	"NOT_IMPLEMENTED":        "This wallet doesn't support that operation.",
	"INSUFFICIENT_BALANCE":   "Not enough balance to do that.",
	"QUOTA_EXCEEDED":         "You've hit this wallet's spending limit for this period.",
	"RESTRICTED":             "This wallet isn't allowed to do that.",
	"UNAUTHORIZED":           "Not authorized for that.",
	"INTERNAL":               "The wallet hit an internal error. Try again.",
	"UNSUPPORTED_ENCRYPTION": "This wallet uses an encryption scheme cashctl doesn't support.",
	"OTHER":                  "The wallet declined this request.",
	"PAYMENT_FAILED":         "The payment failed.",
	"NOT_FOUND":              "Couldn't find that.",
	"EXPIRED":                "This wallet has expired and can no longer send payments.",
	"BAD_REQUEST":            "That request wasn't valid.",
}

// cashTokenNWCErrorMessages overrides nwcErrorMessages' wallet-oriented
// text for codes whose meaning is different when the declining party is a
// cash token's own Hub-side check (CheckClaim, CashRedeem, CashTransfer,
// ListRecipients — anything dialing entry.Token itself) rather than a
// registered wallet connection. EXPIRED above all: on a cash token this is
// the token's own redemption deadline, not "this wallet" — cashctl used to
// print the wallet-oriented text here regardless, which read as if the
// user's *wallet* had expired when it was the token they were trying to
// receive/spend.
var cashTokenNWCErrorMessages = map[string]string{
	"EXPIRED": "This cash token has expired and can no longer be claimed or spent.",
}

// NWCError classifies a wallet's NWC error response (nmilat's
// relay/client.WalletError, returned by every relay/client.NWCClient and
// nipcash/nipcw client call on a wallet decline) into a *CLIError: cashctl's
// own coarse ErrorCode (for the exit code and --json "code" field), a
// plain-language Message translated from the table above (human mode) or
// the wallet's own message (unrecognized code), the raw NWC code preserved
// as CLIError.NWCCode either way, and the wallet's own raw message
// preserved as CLIError.RawMessage either way too — a translated code's
// specific detail (an exact amount, a specific floor violated, ...) would
// otherwise be discarded entirely, human mode and --json alike, even
// though --json exists precisely for a consumer that wants more than the
// generic bucket text.
func NWCError(cmd *cobra.Command, err *relayclient.WalletError) error {
	return nwcError(cmd, err, nwcErrorMessages)
}

// NWCErrorForCashToken is NWCError's counterpart for a decline received
// while checking or spending a cash token's own claim (not a registered
// wallet) — see cashTokenNWCErrorMessages' own doc comment. Classification
// (ErrorCode, exit code, NWCCode, RawMessage) is identical to NWCError;
// only the human-mode friendly text for overridden codes differs, so
// --json output — which always carries RawMessage, never this table — is
// unaffected either way.
func NWCErrorForCashToken(cmd *cobra.Command, err *relayclient.WalletError) error {
	return nwcError(cmd, err, cashTokenNWCErrorMessages)
}

func nwcError(cmd *cobra.Command, err *relayclient.WalletError, overrides map[string]string) error {
	silence(cmd)
	code := nwcErrorCode[err.Code]
	if code == "" {
		code = CodeInternal
	}
	// Sanitized: err.Message is the wallet's own raw text, not cashctl's.
	rawMessage := Sanitize(err.Message)
	message := rawMessage
	if friendly, ok := overrides[err.Code]; ok {
		message = friendly
	} else if friendly, ok := nwcErrorMessages[err.Code]; ok {
		message = friendly
	}
	return &CLIError{
		Err:        &plainError{message},
		Code:       code,
		NWCCode:    err.Code,
		RawMessage: rawMessage,
	}
}

type plainError struct{ s string }

func (e *plainError) Error() string { return e.s }
