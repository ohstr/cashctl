package output

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"
)

func newTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().Bool("json", false, "")
	return cmd
}

func TestExitCode_EachClassifierMapsToItsOwnCode(t *testing.T) {
	cmd := newTestCmd()
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"usage", UsageError(cmd, errors.New("bad")), 2},
		{"invalid_input", InvalidInputError(cmd, "x", errors.New("bad")), 3},
		{"not_found", NotFoundError(cmd, "x", errors.New("bad")), 4},
		{"conflict", ConflictError(cmd, "x", errors.New("bad")), 5},
		{"network", NetworkError(cmd, errors.New("bad")), 6},
		{"auth", AuthError(cmd, errors.New("bad")), 7},
		{"internal/runtime", RuntimeError(cmd, errors.New("bad")), 1},
		{"unclassified plain error", errors.New("bad"), 1},
		{"nil", nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExitCode(tt.err); got != tt.want {
				t.Errorf("ExitCode() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestWrapCLIError_PreservesExistingClassification(t *testing.T) {
	cmd := newTestCmd()
	original := NotFoundError(cmd, "abc", errors.New("no such wallet"))

	// Re-wrapping an already-classified error (e.g. a lower-level helper
	// that already called NotFoundError, whose result bubbles up through a
	// caller that would otherwise wrap it as RuntimeError) must not
	// silently downgrade its classification.
	rewrapped := RuntimeError(cmd, original)

	if ExitCode(rewrapped) != ExitCode(original) {
		t.Errorf("ExitCode(rewrapped) = %d, want unchanged %d", ExitCode(rewrapped), ExitCode(original))
	}
	ce := AsCLIError(rewrapped)
	if ce.Code != CodeNotFound {
		t.Errorf("Code = %q, want %q (original classification preserved)", ce.Code, CodeNotFound)
	}
}

func TestAsCLIError_UnclassifiedFallsBackToInternal(t *testing.T) {
	ce := AsCLIError(errors.New("boom"))
	if ce.Code != CodeInternal {
		t.Errorf("Code = %q, want %q", ce.Code, CodeInternal)
	}
	if ce.Error() != "boom" {
		t.Errorf("Error() = %q, want %q", ce.Error(), "boom")
	}
}

func TestRetryableCodes(t *testing.T) {
	tests := []struct {
		code ErrorCode
		want bool
	}{
		{CodeConflict, true},
		{CodeNetwork, true},
		{CodeUsage, false},
		{CodeInvalidInput, false},
		{CodeNotFound, false},
		{CodeAuth, false},
		{CodeInternal, false},
	}
	for _, tt := range tests {
		if got := retryableCodes[tt.code]; got != tt.want {
			t.Errorf("retryableCodes[%q] = %v, want %v", tt.code, got, tt.want)
		}
	}
}

func TestRedactSecretInput(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "bare 64-hex secret",
			input: "bb7a99ee8fc7ac5529e0747fc12f438f8e3c7765cd0a887e33d16dee8c0ba7a6",
			want:  "",
		},
		{
			name:  "nsec",
			input: "nsec1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq",
			want:  "",
		},
		{
			name:  "pubkey credential redacts the privkey",
			input: "pubkey:bb7a99ee8fc7ac5529e0747fc12f438f8e3c7765cd0a887e33d16dee8c0ba7a6",
			want:  "pubkey:<redacted>",
		},
		{
			name:  "bearer credential redacts the secret",
			input: "bearer:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			want:  "bearer:<redacted>",
		},
		{
			name:  "connection-key credential redacts only the leading privkey",
			input: "connection-key:bb7a99ee8fc7ac5529e0747fc12f438f8e3c7765cd0a887e33d16dee8c0ba7a6,discord,482910,./attestation.json",
			want:  "connection-key:<redacted>,discord,482910,./attestation.json",
		},
		{
			name:  "an ordinary non-secret value passes through unchanged",
			input: "circle:family",
			want:  "circle:family",
		},
		{
			name:  "a cash token passes through unchanged (not secret-shaped)",
			input: "lokicash1qypqxpq9qcrsszg2pvxq6rs0zqg3zyg3zygs9qypqxpq",
			want:  "lokicash1qypqxpq9qcrsszg2pvxq6rs0zqg3zyg3zygs9qypqxpq",
		},
		// The following cases fix a real bug (--sources <token>:<amount>:
		// pubkey:<privkey> echoed the private key back on a bad amount)
		// and its documented near-misses: leading/trailing whitespace and
		// mixed case defeated the old whole-string-only match entirely.
		{
			name:  "uppercase nsec is still redacted",
			input: "NSEC1QQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQ",
			want:  "",
		},
		{
			name:  "whitespace-padded hex is still redacted",
			input: "  bb7a99ee8fc7ac5529e0747fc12f438f8e3c7765cd0a887e33d16dee8c0ba7a6  ",
			want:  "",
		},
		{
			// The trailing quote is consumed along with the secret (\S+
			// doesn't stop at punctuation) — cosmetic only: what matters
			// is that the key itself never survives, quote or not.
			name:  "uppercase PUBKEY: prefix and a leading space are still caught",
			input: " 'PUBKEY:bb7a99ee8fc7ac5529e0747fc12f438f8e3c7765cd0a887e33d16dee8c0ba7a6'",
			want:  " 'PUBKEY:<redacted>",
		},
		{
			name:  "a credential embedded after other fields is redacted, the rest stays legible",
			input: "tok-c12d:notanumber:pubkey:bb7a99ee8fc7ac5529e0747fc12f438f8e3c7765cd0a887e33d16dee8c0ba7a6",
			want:  "tok-c12d:notanumber:pubkey:<redacted>",
		},
		{
			name:  "an NWC URI's secret= value is redacted, the rest (pubkey, relay) is not",
			input: "nostr+walletconnect://" + strings.Repeat("a1", 32) + "?relay=wss%3A%2F%2Fx.invalid&secret=" + strings.Repeat("b2", 32),
			want:  "nostr+walletconnect://" + strings.Repeat("a1", 32) + "?relay=wss%3A%2F%2Fx.invalid&secret=<redacted>",
		},
		{
			name:  "a bearer gift string's #secret half is redacted, the token half is not",
			input: "lokicash1qypqxpq9qcrsszg2pvxq6rs0zqg3zyg3zygs9qypqxpq#" + strings.Repeat("c3", 32),
			want:  "lokicash1qypqxpq9qcrsszg2pvxq6rs0zqg3zyg3zygs9qypqxpq#<redacted>",
		},
		{
			name:  "a Cash Hub connection string is redacted wholesale",
			input: "cashhub1qqsxyzsomefakepayloadthatlookslikebech32qqq",
			want:  "cashhub1<redacted>",
		},
		{
			name:  "a Circle Hub connection string is redacted wholesale",
			input: "circlehub1qqsxyzsomefakepayloadthatlookslikebech32qqq",
			want:  "circlehub1<redacted>",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RedactSecretInput(tt.input); got != tt.want {
				t.Errorf("RedactSecretInput(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestArgsValidators(t *testing.T) {
	t.Run("ExactArgs", func(t *testing.T) {
		v := ExactArgs(1)
		cmd := newTestCmd()
		if err := v(cmd, []string{"a"}); err != nil {
			t.Errorf("unexpected error for correct arg count: %v", err)
		}
		cmd2 := newTestCmd()
		err := v(cmd2, []string{})
		if err == nil {
			t.Fatal("expected error for wrong arg count")
		}
		if ExitCode(err) != 2 {
			t.Errorf("ExitCode = %d, want 2 (usage)", ExitCode(err))
		}
	})
	t.Run("MaximumNArgs", func(t *testing.T) {
		v := MaximumNArgs(1)
		if err := v(newTestCmd(), []string{"a"}); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if err := v(newTestCmd(), []string{"a", "b"}); err == nil {
			t.Error("expected error for too many args")
		}
	})
	t.Run("MinimumNArgs", func(t *testing.T) {
		v := MinimumNArgs(1)
		if err := v(newTestCmd(), []string{"a"}); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if err := v(newTestCmd(), []string{}); err == nil {
			t.Error("expected error for too few args")
		}
	})
	t.Run("NoArgs", func(t *testing.T) {
		if err := NoArgs(newTestCmd(), []string{}); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if err := NoArgs(newTestCmd(), []string{"a"}); err == nil {
			t.Error("expected error for unexpected arg")
		}
	})
}

func TestNWCError_KnownCodeMapsAndTranslates(t *testing.T) {
	cmd := newTestCmd()
	err := NWCError(cmd, &relayclient.WalletError{Method: "pay_invoice", Code: "QUOTA_EXCEEDED", Message: "raw wallet message"})
	ce := AsCLIError(err)
	if ce.Code != CodeInternal {
		t.Errorf("Code = %q, want %q (QUOTA_EXCEEDED has no more specific bucket)", ce.Code, CodeInternal)
	}
	if ce.NWCCode != "QUOTA_EXCEEDED" {
		t.Errorf("NWCCode = %q, want %q", ce.NWCCode, "QUOTA_EXCEEDED")
	}
	if ce.Error() == "raw wallet message" {
		t.Error("expected the translated human message, not the raw wallet message, in human-mode Error()")
	}
}

func TestNWCError_ExpiredMapsToAuth(t *testing.T) {
	err := NWCError(newTestCmd(), &relayclient.WalletError{Code: "EXPIRED", Message: "..."})
	if AsCLIError(err).Code != CodeAuth {
		t.Errorf("Code = %q, want %q", AsCLIError(err).Code, CodeAuth)
	}
	if ExitCode(err) != 7 {
		t.Errorf("ExitCode = %d, want 7", ExitCode(err))
	}
}

// TestNWCErrorForCashToken_ExpiredDoesNotCallItAWallet guards against the
// bug found receiving/redeeming/transferring/consolidating an already-
// expired cash token: classifyNWCErr's shared EXPIRED text ("This wallet
// has expired...") is correct for a registered NWC wallet decline but
// wrong for a cash token's own Hub-side claim check — the thing that
// expired is the token, not "this wallet". Classification (code, exit
// code, NWCCode) must stay identical to NWCError; only the human-mode text
// differs.
func TestNWCErrorForCashToken_ExpiredDoesNotCallItAWallet(t *testing.T) {
	cmd := newTestCmd()
	err := NWCErrorForCashToken(cmd, &relayclient.WalletError{Code: "EXPIRED", Message: "raw"})
	ce := AsCLIError(err)
	if ce.Code != CodeAuth {
		t.Errorf("Code = %q, want %q", ce.Code, CodeAuth)
	}
	if ExitCode(err) != 7 {
		t.Errorf("ExitCode = %d, want 7", ExitCode(err))
	}
	if ce.NWCCode != "EXPIRED" {
		t.Errorf("NWCCode = %q, want %q", ce.NWCCode, "EXPIRED")
	}
	if strings.Contains(ce.Error(), "wallet") {
		t.Errorf("Error() = %q, still calls the expired cash token a wallet", ce.Error())
	}
	if !strings.Contains(ce.Error(), "token") {
		t.Errorf("Error() = %q, want it to say this is about the token", ce.Error())
	}
}

// TestNWCErrorForCashToken_OtherCodesMatchNWCError guards against
// NWCErrorForCashToken accidentally losing the shared translation table
// for every code it doesn't override.
func TestNWCErrorForCashToken_OtherCodesMatchNWCError(t *testing.T) {
	for _, code := range []string{"BAD_REQUEST", "RATE_LIMITED", "INTERNAL", "SOME_FUTURE_CODE"} {
		walletErr := &relayclient.WalletError{Code: code, Message: "raw " + code}
		got := AsCLIError(NWCErrorForCashToken(newTestCmd(), walletErr)).Error()
		want := AsCLIError(NWCError(newTestCmd(), walletErr)).Error()
		if got != want {
			t.Errorf("code %s: NWCErrorForCashToken = %q, want same as NWCError %q", code, got, want)
		}
	}
}

func TestNWCError_UnknownCodeFallsBackToWalletMessage(t *testing.T) {
	err := NWCError(newTestCmd(), &relayclient.WalletError{Code: "SOME_FUTURE_CODE", Message: "a message cashctl doesn't know how to translate"})
	ce := AsCLIError(err)
	if ce.Code != CodeInternal {
		t.Errorf("Code = %q, want %q", ce.Code, CodeInternal)
	}
	if ce.Error() != "a message cashctl doesn't know how to translate" {
		t.Errorf("Error() = %q, want the wallet's own message verbatim", ce.Error())
	}
}

// TestNWCError_TranslatedCodeStillPreservesRawMessage guards against the
// bug found auditing cash_transfer's CashMinTransferMloki floor: a
// translated code's specific detail (here, which exact amount/floor was
// violated) used to be discarded entirely — CLIError.Err carried only the
// generic bucket sentence, and EmitError's --json "error" field read from
// Err too, so a --json consumer had no way to learn the specific reason
// behind two BAD_REQUEST declines that print byte-identical error bodies.
func TestNWCError_TranslatedCodeStillPreservesRawMessage(t *testing.T) {
	err := NWCError(newTestCmd(), &relayclient.WalletError{
		Code:    "BAD_REQUEST",
		Message: "split amount 500 is below this wallet's min_transfer_mloki floor of 1000",
	})
	ce := AsCLIError(err)
	if ce.Error() != "That request wasn't valid." {
		t.Errorf("Error() = %q, want the generic human-mode translation unchanged", ce.Error())
	}
	if ce.RawMessage != "split amount 500 is below this wallet's min_transfer_mloki floor of 1000" {
		t.Errorf("RawMessage = %q, want the wallet's own specific text preserved", ce.RawMessage)
	}
}

// TestEmitError_JSONModeUsesRawMessageOverGenericTranslation is the same
// finding, proven at EmitError's own boundary (the actual --json "error"
// field a script/agent reads), not just at the CLIError level.
func TestEmitError_JSONModeUsesRawMessageOverGenericTranslation(t *testing.T) {
	cmd := newTestCmd()
	_ = cmd.Flags().Set("json", "true")
	err := NWCError(cmd, &relayclient.WalletError{
		Code:    "BAD_REQUEST",
		Message: "remainder 200 is below this wallet's min_transfer_mloki floor of 1000",
	})

	stderr := captureStderr(t, func() { EmitError(cmd, err) })

	var payload struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if jsonErr := json.Unmarshal(stderr, &payload); jsonErr != nil {
		t.Fatalf("EmitError's --json output didn't parse as JSON: %v\noutput: %s", jsonErr, stderr)
	}
	if payload.Error != "remainder 200 is below this wallet's min_transfer_mloki floor of 1000" {
		t.Errorf(`--json "error" = %q, want the wallet's own specific message, not the generic bucket text`, payload.Error)
	}
	if payload.Code != "invalid_input" {
		t.Errorf(`--json "code" = %q, want "invalid_input"`, payload.Code)
	}
}

// TestEmitError_JSONModeFallsBackToTranslationWithoutRawMessage confirms
// the fallback: a CLIError built without NWCError (no RawMessage set at
// all, e.g. UsageError/InvalidInputError/... — every other constructor in
// this package) must keep behaving exactly as before this fix — --json's
// "error" field is Err.Error(), same as human mode.
func TestEmitError_JSONModeFallsBackToTranslationWithoutRawMessage(t *testing.T) {
	cmd := newTestCmd()
	_ = cmd.Flags().Set("json", "true")
	err := InvalidInputError(cmd, "", errors.New("ordinary cashctl-side validation error"))

	stderr := captureStderr(t, func() { EmitError(cmd, err) })

	var payload struct {
		Error string `json:"error"`
	}
	if jsonErr := json.Unmarshal(stderr, &payload); jsonErr != nil {
		t.Fatalf("EmitError's --json output didn't parse as JSON: %v\noutput: %s", jsonErr, stderr)
	}
	if payload.Error != "ordinary cashctl-side validation error" {
		t.Errorf(`--json "error" = %q, want the underlying error unchanged`, payload.Error)
	}
}

// TestEmitError_HumanModeAppendsRawMessage guards against the bug found
// auditing wallet/Hub declines in text mode: EmitError used to print ONLY
// the generic bucket sentence ("The wallet hit an internal error. Try
// again.") with no way for a human to tell a genuinely transient decline
// from a permanent one dumped into the same INTERNAL/OTHER catch-all —
// unlike --json, which already carried the wallet's own specific
// RawMessage (see TestEmitError_JSONModeUsesRawMessageOverGenericTranslation).
// The generic sentence must stay first (still the primary, skimmable
// guidance), with the specific reason appended in parentheses, not
// replaced — reproduced across every command that classifies a wallet
// decline through NWCError/NWCErrorForCashToken (redeem, transfer,
// consolidate, join, invoice, pay, list-tx all share this one code path).
func TestEmitError_HumanModeAppendsRawMessage(t *testing.T) {
	cmd := newTestCmd() // --json defaults false
	err := NWCError(cmd, &relayclient.WalletError{
		Code:    "INTERNAL",
		Message: "signature verification failed for spend authorization",
	})

	stderr := string(captureStderr(t, func() { EmitError(cmd, err) }))

	if !strings.Contains(stderr, "The wallet hit an internal error. Try again.") {
		t.Errorf("stderr = %q, want the generic bucket sentence still present", stderr)
	}
	if !strings.Contains(stderr, "signature verification failed for spend authorization") {
		t.Errorf("stderr = %q, want the wallet's own specific reason appended, not dropped", stderr)
	}
}

// TestEmitError_HumanModeNoDuplicateForUnknownCode covers the code path
// where RawMessage IS the human message already (an NWC code with no
// translation — see NWCError's own fallback): appending it would just
// print the same sentence twice.
func TestEmitError_HumanModeNoDuplicateForUnknownCode(t *testing.T) {
	cmd := newTestCmd()
	err := NWCError(cmd, &relayclient.WalletError{Code: "SOME_FUTURE_CODE", Message: "a message cashctl doesn't know how to translate"})

	stderr := string(captureStderr(t, func() { EmitError(cmd, err) }))

	if n := strings.Count(stderr, "a message cashctl doesn't know how to translate"); n != 1 {
		t.Errorf("stderr = %q, want the message to appear exactly once, got %d", stderr, n)
	}
}

// TestEmitError_HumanModeUnaffectedWithoutRawMessage is human mode's
// counterpart to TestEmitError_JSONModeFallsBackToTranslationWithoutRawMessage
// — every non-NWC constructor (UsageError, InvalidInputError, ...) must
// print exactly as before this fix.
func TestEmitError_HumanModeUnaffectedWithoutRawMessage(t *testing.T) {
	cmd := newTestCmd()
	err := InvalidInputError(cmd, "", errors.New("ordinary cashctl-side validation error"))

	stderr := string(captureStderr(t, func() { EmitError(cmd, err) }))

	if strings.TrimSpace(stderr) != "Error: ordinary cashctl-side validation error" {
		t.Errorf("stderr = %q, want unchanged plain human-mode text", stderr)
	}
}

// TestUsageError_NeverPrintsHelpText is the regression guard for the
// "funds are fragmented" help-dump bug: UsageError covers real runtime
// conditions, not just malformed invocations, so it must never write
// anything on its own (cmd.Help(), cmd.Usage(), ...) — the classified
// error message alone, via EmitError, says what went wrong. A bare group
// command with no subcommand still gets cobra's own separate help print
// (Command.Runnable() false -> flag.ErrHelp), which never reaches this
// function at all — unrelated, and not what this guards.
func TestUsageError_NeverPrintsHelpText(t *testing.T) {
	cmd := &cobra.Command{
		Use:     "test",
		Short:   "a test command",
		Example: "  cashctl test",
	}
	cmd.Flags().Bool("json", false, "")

	printed := captureStdout(t, func() {
		_ = UsageError(cmd, errors.New("funds are fragmented across separate Hubs"))
	})
	if len(printed) != 0 {
		t.Errorf("UsageError() wrote %q to stdout, want nothing", printed)
	}
}

// captureStdout mirrors captureStderr below, for the one case (UsageError)
// that historically wrote to stdout via cmd.Help() — everything else in
// this package targets stderr exclusively.
func captureStdout(t *testing.T, fn func()) []byte {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	_ = w.Close()
	out, readErr := io.ReadAll(r)
	if readErr != nil {
		t.Fatalf("reading captured stdout: %v", readErr)
	}
	return out
}

// captureStderr redirects os.Stderr for the duration of fn and returns
// whatever it wrote — EmitError has no injectable writer, it always
// targets os.Stderr directly (see its own doc comment on why: a script
// parsing --json's stdout result must never see errors on the same
// stream).
func captureStderr(t *testing.T, fn func()) []byte {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	fn()

	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading captured stderr: %v", err)
	}
	return out
}
