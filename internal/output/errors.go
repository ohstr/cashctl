package output

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// ErrorCode classifies a CLIError for exit-code and --json "code" field
// purposes. Mirrors ncli's own cli/common/errors.go taxonomy exactly, so
// an agent or script that already knows ncli's contract needs to learn
// nothing new for cashctl.
type ErrorCode string

const (
	CodeUsage        ErrorCode = "usage"         // the command itself was invoked wrong
	CodeInvalidInput ErrorCode = "invalid_input" // a value cashctl was given doesn't parse/validate
	CodeNotFound     ErrorCode = "not_found"     // a referenced wallet/token/connection doesn't exist
	CodeConflict     ErrorCode = "conflict"      // a transient state conflict — retryable
	CodeNetwork      ErrorCode = "network"       // couldn't reach a relay/wallet — retryable
	CodeAuth         ErrorCode = "auth"          // not authorized, or no longer (expired/restricted)
	CodeInternal     ErrorCode = "internal"      // fallback: a wallet-side or cashctl-side failure
)

// exitCodes maps each ErrorCode to the process exit code ExitCode returns.
// CodeInternal deliberately shares exit code 1 with "no classification at
// all" (see AsCLIError) — an unclassified error is, by definition, one
// nothing more specific was known about it.
var exitCodes = map[ErrorCode]int{
	CodeInternal:     1,
	CodeUsage:        2,
	CodeInvalidInput: 3,
	CodeNotFound:     4,
	CodeConflict:     5,
	CodeNetwork:      6,
	CodeAuth:         7,
}

// retryableCodes marks which codes describe a transient condition worth an
// agent retrying without changing anything — surfaced as --json's
// "retryable" field.
var retryableCodes = map[ErrorCode]bool{
	CodeConflict: true,
	CodeNetwork:  true,
}

// CLIError is cashctl's own classified error. Err is the underlying cause
// — its Error() is what human mode prints and what --json falls back to
// when RawMessage is empty; Code drives the exit code and --json "code"
// field; Input is the specific offending value (already redacted if
// sensitive, see RedactSecretInput), omitted from output when empty;
// NWCCode, when set, is the raw NIP-47 error code a wallet returned —
// preserved verbatim in --json output alongside cashctl's own coarser
// Code, so an agent that needs finer-grained branching than cashctl's 7
// buckets still gets it (see NWCError in nwc_errors.go). RawMessage, when
// set, is the wallet's own specific error text (already Sanitized) —
// EmitError uses it as --json's whole "error" field (an agent wants the
// specific reason, e.g. the exact floor/amount a request violated, not the
// same canned sentence every request in that error's bucket produces) and
// appends it, parenthetically, after Err's translated human-mode message
// too — a human reading "The wallet hit an internal error. Try again."
// with no other detail used to have no way to tell a genuinely transient
// decline from a permanent one dumped into the same catch-all bucket.
// ShowUsage, when true, tells EmitError to append a short "run --help"
// pointer after the error line in text mode — see InvocationError's own
// doc comment for which CodeUsage errors this applies to and why not all
// of them.
type CLIError struct {
	Err        error
	Code       ErrorCode
	Input      string
	NWCCode    string
	RawMessage string
	ShowUsage  bool
}

func (e *CLIError) Error() string { return e.Err.Error() }
func (e *CLIError) Unwrap() error { return e.Err }

// wrapCLIError classifies err as code/input, unless err is already a
// *CLIError — reclassifying an already-classified error would silently
// discard whatever more-specific classification produced it further down
// the call stack.
//
// input is redacted by every caller already (RedactSecretInput), but
// err's own message text isn't — a call site building its own message
// with fmt.Errorf around a raw value (e.g. "no wallet or held token named
// %q", rather than relying on the input field to carry it) would
// otherwise bypass redaction entirely, one audit away from a leak. This
// is the last, catch-all place every such error passes through before
// ever reaching EmitError, so it's redacted here too — but only replaced
// when redaction actually changes something, so the overwhelming
// majority of ordinary (non-secret) errors keep their own Unwrap chain
// intact for anything downstream still trying errors.As/Is against them.
func wrapCLIError(code ErrorCode, input string, err error) error {
	if err == nil {
		return nil
	}
	var existing *CLIError
	if errors.As(err, &existing) {
		return err
	}
	if redacted := RedactSecretInput(err.Error()); redacted != err.Error() {
		err = errors.New(redacted)
	}
	return &CLIError{Err: err, Code: code, Input: input}
}

// silence stops cobra from printing its own usage/error text on top of
// cashctl's own EmitError output.
func silence(cmd *cobra.Command) {
	if cmd == nil {
		return
	}
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
}

// UsageError classifies err as CodeUsage — the command was invoked wrong.
// Doesn't print cmd's help text: CodeUsage covers real runtime conditions
// too (e.g. transfer's "funds are fragmented"), not just malformed
// invocations, so dumping the full --help block here was noise more often
// than it was useful — the classified error message alone (via EmitError)
// says what went wrong. A bare group command with no subcommand (e.g.
// `cashctl circle`) already gets its own help print from cobra itself,
// via a completely different path (Command.Runnable() false ->
// flag.ErrHelp, caught before any error ever reaches here) — unaffected
// by this.
func UsageError(cmd *cobra.Command, err error) error {
	silence(cmd)
	return wrapCLIError(CodeUsage, "", err)
}

// InvocationError is UsageError, but additionally marks the error so
// EmitError follows the usual "Error: ..." line with the command's own
// full --help content in text mode — Usage, Examples, Flags, all of it,
// the same as `cashctl <cmd> --help` prints. Never in --json mode (an
// agent doesn't need any of this human framing, and it would break
// --json's single-document-on-stderr contract). Reserved for cases that
// are unambiguously "you invoked this wrong" in the mechanical sense — a
// bad argument count, an unknown flag/command, a missing required flag,
// two conflicting ways of supplying the same value — never for a
// CodeUsage error that's actually a legitimate runtime refusal wearing the
// usage exit code for other reasons (transfer's "funds are fragmented",
// consolidate's "needs at least 2 sources", an invalid interactive
// numbered-list pick, ...), which stay exactly as terse as before, no help
// appended. See UsageError's own doc comment on why CodeUsage covers both
// and why a help dump on every one of them was noise more often than
// useful — this only widens that back out for the subset where it's
// actually actionable.
//
// The Code == CodeUsage guard matters when err is already a *CLIError
// classified as something else (wrapCLIError's own "don't reclassify"
// rule, see TestWrapCLIError_PreservesExistingClassification) — this must
// not flip ShowUsage on a passthrough that was never reclassified as usage
// at all.
func InvocationError(cmd *cobra.Command, err error) error {
	wrapped := UsageError(cmd, err)
	if ce, ok := wrapped.(*CLIError); ok && ce.Code == CodeUsage {
		ce.ShowUsage = true
	}
	return wrapped
}

// InvalidInputError classifies err as CodeInvalidInput — input doesn't
// parse or validate as this method requires. input is redacted before
// being attached (see RedactSecretInput).
func InvalidInputError(cmd *cobra.Command, input string, err error) error {
	silence(cmd)
	return wrapCLIError(CodeInvalidInput, RedactSecretInput(input), err)
}

// NotFoundError classifies err as CodeNotFound.
func NotFoundError(cmd *cobra.Command, input string, err error) error {
	silence(cmd)
	return wrapCLIError(CodeNotFound, RedactSecretInput(input), err)
}

// ConflictError classifies err as CodeConflict (retryable).
func ConflictError(cmd *cobra.Command, input string, err error) error {
	silence(cmd)
	return wrapCLIError(CodeConflict, RedactSecretInput(input), err)
}

// NetworkError classifies err as CodeNetwork (retryable).
func NetworkError(cmd *cobra.Command, err error) error {
	silence(cmd)
	return wrapCLIError(CodeNetwork, "", err)
}

// AuthError classifies err as CodeAuth — not authorized, or no longer.
func AuthError(cmd *cobra.Command, err error) error {
	silence(cmd)
	return wrapCLIError(CodeAuth, "", err)
}

// RuntimeError classifies err as CodeInternal — the fallback bucket for a
// wallet-side or cashctl-side failure that isn't one of the more specific
// cases above.
func RuntimeError(cmd *cobra.Command, err error) error {
	silence(cmd)
	return wrapCLIError(CodeInternal, "", err)
}

// ExitCode returns the process exit code for err (0 if err is nil).
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ce *CLIError
	if errors.As(err, &ce) {
		if code, ok := exitCodes[ce.Code]; ok {
			return code
		}
	}
	return exitCodes[CodeInternal]
}

// AsCLIError normalizes any error into a *CLIError: one that's already
// classified passes through unchanged; anything else becomes CodeInternal.
func AsCLIError(err error) *CLIError {
	var ce *CLIError
	if errors.As(err, &ce) {
		return ce
	}
	return &CLIError{Err: err, Code: CodeInternal}
}

// ExactArgs/MaximumNArgs/MinimumNArgs/NoArgs are cobra.PositionalArgs
// replacements that route a wrong argument count through UsageError,
// instead of cobra's own validators, which bypass this whole error
// contract (their errors are never a *CLIError, so they'd all report as
// undifferentiated CodeInternal / exit 1 instead of exit 2 usage errors).
func ExactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != n {
			return InvocationError(cmd, fmt.Errorf("accepts %d arg(s), received %d", n, len(args)))
		}
		return nil
	}
}

func MaximumNArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) > n {
			return InvocationError(cmd, fmt.Errorf("accepts at most %d arg(s), received %d", n, len(args)))
		}
		return nil
	}
}

func MinimumNArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < n {
			return InvocationError(cmd, fmt.Errorf("requires at least %d arg(s), received %d", n, len(args)))
		}
		return nil
	}
}

func NoArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return InvocationError(cmd, fmt.Errorf("accepts no arguments, received %d", len(args)))
	}
	return nil
}

// secretLikePattern matches cashctl's own raw-secret-shaped inputs, as a
// STANDALONE value: a bech32 nsec1... key, or a bare 64-character hex
// string (a raw privkey/secret, as used directly in
// pubkey:<privkey>/cash:<secret> credential strings — see
// internal/credential). Case-insensitive and tolerant of surrounding
// whitespace: a value copy-pasted with a stray leading space or typed in
// uppercase is exactly as secret as the canonical form, and treating it
// as unrecognized (returning it unredacted, the previous behavior) is the
// one wrong answer here. Redacting these from error output (which may be
// logged, pasted into a bug report, or echoed by --json) matters more for
// cashctl than for most CLIs: its whole domain is handling literal
// spending secrets as command arguments.
var secretLikePattern = regexp.MustCompile(`(?i)^\s*(nsec1[a-z0-9]+|[0-9a-f]{64})\s*$`)

// credentialPrefixPattern finds cashctl's own credential-string prefixes
// (cash:/pubkey:/connection-key:) ANYWHERE in a string, case/space-
// insensitively — not just as the whole string. A --sources
// <token>:<amount>:<credential> entry embeds one after two other
// colon-separated fields; matching only at position 0 (the previous
// behavior) let a bad amount there report the ENTIRE entry, private key
// included, since redaction never even looked past the first colon.
var credentialPrefixPattern = regexp.MustCompile(`(?i)\b(cash|pubkey|connection-key)\s*:\s*(\S+)`)

// secretBearingBech32Pattern matches a bech32 CONNECTION string —
// cashhub1/circlehub1/nconnection1 — deliberately NOT a cash-token HRP
// (lokicash1, satscash1, ...): a held cash token is meant to be shown
// (cashctl prints/returns it routinely, e.g. ledger.Entry.Token's own
// plain json tag, unlike Secret/CashSecret's json:"-"), whereas a Hub or
// pairing connection string is something a user only ever mis-pastes into
// the wrong command, never something they're meant to hand back out — and
// every one of these encodes its own dialing secret as a single
// TLV-packed blob with no substring that's safe to reveal (unlike an NWC
// URI's separate secret= query parameter, see nwcSecretPattern below), so
// a match here is redacted wholesale.
var secretBearingBech32Pattern = regexp.MustCompile(`(?i)\b(cashhub|circlehub|nconnection)1[a-z0-9]{20,}`)

// nwcSecretPattern matches specifically the secret= query value of a
// nostr+walletconnect:// URI — the one part of that URI that's actually
// secret (the host/pubkey and relay parameters are not, see
// nip47.ParsePairingURI).
var nwcSecretPattern = regexp.MustCompile(`(?i)([?&]secret=)[0-9a-f]+`)

// giftSecretPattern matches the "#<secret>" half of a cash gift string
// (<token>#<cash_secret>, NIP-CASH's combined presentation) — the exact
// shape internal/dial's own SplitCashSliceString parses.
var giftSecretPattern = regexp.MustCompile(`#[0-9a-fA-F]{64}\b`)

// RedactSecretInput scrubs every secret-shaped substring it recognizes out
// of s — a raw nsec1/64-hex value (redacted
// entirely), a cash:/pubkey:/connection-key: credential (its secret
// component blanked, wherever in s it appears), an NWC URI's own secret=
// value, a cash gift string's #<secret> half, or a Hub/token bech32
// string (redacted entirely, HRP kept) — so a command's own error/--json
// output never echoes spendable material back out, however it was
// embedded in what the user typed. Returns s unchanged if none apply.
func RedactSecretInput(s string) string {
	if secretLikePattern.MatchString(s) {
		return ""
	}
	out := credentialPrefixPattern.ReplaceAllStringFunc(s, func(m string) string {
		g := credentialPrefixPattern.FindStringSubmatch(m)
		prefix, rest := g[1], g[2]
		if strings.EqualFold(prefix, "connection-key") {
			// connection-key:<privkey>,<platform>,<external-id>,<attestation-file>
			// — only the leading privkey component is secret; the rest
			// stays legible in the error, same as the other two prefixes'
			// own single secret component.
			parts := strings.SplitN(rest, ",", 2)
			if len(parts) == 2 {
				return prefix + ":<redacted>," + parts[1]
			}
		}
		return prefix + ":<redacted>"
	})
	out = nwcSecretPattern.ReplaceAllString(out, "${1}<redacted>")
	out = giftSecretPattern.ReplaceAllString(out, "#<redacted>")
	out = secretBearingBech32Pattern.ReplaceAllStringFunc(out, func(m string) string {
		hrp := secretBearingBech32Pattern.FindStringSubmatch(m)[1]
		return hrp + "1<redacted>"
	})
	return out
}
