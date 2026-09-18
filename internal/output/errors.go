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
// EmitError prefers it for --json's "error" field over Err's translated,
// deliberately-generic human message, since an agent parsing --json wants
// the specific reason (e.g. the exact floor/amount a request violated),
// not the same canned sentence every request in that error's bucket
// produces.
type CLIError struct {
	Err        error
	Code       ErrorCode
	Input      string
	NWCCode    string
	RawMessage string
}

func (e *CLIError) Error() string { return e.Err.Error() }
func (e *CLIError) Unwrap() error { return e.Err }

// wrapCLIError classifies err as code/input, unless err is already a
// *CLIError — reclassifying an already-classified error would silently
// discard whatever more-specific classification produced it further down
// the call stack.
func wrapCLIError(code ErrorCode, input string, err error) error {
	if err == nil {
		return nil
	}
	var existing *CLIError
	if errors.As(err, &existing) {
		return err
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
			return UsageError(cmd, fmt.Errorf("accepts %d arg(s), received %d", n, len(args)))
		}
		return nil
	}
}

func MaximumNArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) > n {
			return UsageError(cmd, fmt.Errorf("accepts at most %d arg(s), received %d", n, len(args)))
		}
		return nil
	}
}

func MinimumNArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < n {
			return UsageError(cmd, fmt.Errorf("requires at least %d arg(s), received %d", n, len(args)))
		}
		return nil
	}
}

func NoArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return UsageError(cmd, fmt.Errorf("accepts no arguments, received %d", len(args)))
	}
	return nil
}

// secretLikePattern matches cashctl's own raw-secret-shaped inputs: a bech32
// nsec1... key, or a bare 64-character hex string (a raw privkey/secret,
// as used directly in pubkey:<privkey>/bearer:<secret> credential
// strings — see internal/credential). Redacting these from error output
// (which may be logged, pasted into a bug report, or echoed by --json)
// matters more for cashctl than for most CLIs: its whole domain is handling
// literal spending secrets as command arguments.
var secretLikePattern = regexp.MustCompile(`^(nsec1[a-z0-9]+|[0-9a-fA-F]{64})$`)

// RedactSecretInput returns "" if s looks like a raw secret (see
// secretLikePattern) or contains one after a credential-string prefix
// (bearer:<secret>, or the <privkey> component of pubkey:<privkey> /
// connection-key:<privkey>,...), so a command's own error/JSON output
// never echoes spendable material back out. Returns s unchanged otherwise.
func RedactSecretInput(s string) string {
	if secretLikePattern.MatchString(s) {
		return ""
	}
	if prefix, rest, ok := strings.Cut(s, ":"); ok {
		switch prefix {
		case "bearer":
			return prefix + ":<redacted>"
		case "pubkey":
			return prefix + ":<redacted>"
		case "connection-key":
			// connection-key:<privkey>,<platform>,<external-id>,<attestation-file>
			// — only the leading privkey component is secret.
			parts := strings.SplitN(rest, ",", 2)
			if len(parts) == 2 {
				return prefix + ":<redacted>," + parts[1]
			}
			return prefix + ":<redacted>"
		}
	}
	return s
}
