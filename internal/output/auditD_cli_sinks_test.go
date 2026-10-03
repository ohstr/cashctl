package output

// Audit D, CLI surface, finding 3 (D-CLI-3) — the output sinks sanitize nothing.
//
// Sanitize's character table is fine. The defect is that applying it is a
// per-call-site convention with no enforcement: 23 call sites, and PrintJSON and
// EmitError themselves do nothing. The proof that a convention does not hold is
// in the tree twice over — cmd/decode.go sanitizes a Hub label for human output
// and emits it raw into --json four lines later, and cmd/cash_inspect.go
// sanitizes identity_type/identity_value while cmd/cash_transfer.go puts the
// same two fields into --json raw.
//
// WHY THESE ASSERT ON PARSED VALUES, not on the raw bytes. encoding/json escapes
// ESC to the six characters \u001b, so a byte-level search for 0x1b finds nothing
// even in the unfixed code — the output is inert until something parses it. The
// real exposure is `jq -r '.label'`, which turns it straight back into a live
// escape sequence on the operator's terminal, and this CLI's output is routinely
// piped through jq -r into logs. Asserting on the decoded value is what models
// that. The raw-byte assertions below are a SEPARATE, narrower check, because Go
// escapes C0 but emits C1 and bidi raw, so those two are live with no jq step at
// all.
//
// Mutants these must fail against: removing PrintJSON's sanitizing round trip;
// removing EmitError's sanitize; dropping UseNumber from the round trip (kills
// the large-integer test); and sanitizing with the strict Sanitize instead of a
// newline-permitting variant (kills the multi-line test).

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

const (
	escClearScreen = "\x1b[2J\x1b[H" // ESC-prefixed CSI: inert in JSON bytes, live after jq -r
	c1CSI          = "\u009b2J"      // 8-bit CSI: honoured by xterm/VTE, emitted RAW by encoding/json
	bidiOverride   = "\u202e"        // reorders how the same bytes display
	unicodeLineSep = "\u2028"
)

// liveInRawJSON are the hostile runes encoding/json does NOT escape, so they
// reach a terminal straight out of stdout with nothing parsing anything.
var liveInRawJSON = []string{"\u009b", "\u202e"}

// allHostile is every rune these tests plant, checked against decoded values.
var allHostile = []string{"\x1b", "\u009b", "\u202e", "\u2028"}

// walkStrings visits every string in a decoded JSON tree, keys included — a
// hostile map KEY reaches a terminal just as readily as a hostile value.
func walkStrings(v any, visit func(string)) {
	switch t := v.(type) {
	case string:
		visit(t)
	case []any:
		for _, e := range t {
			walkStrings(e, visit)
		}
	case map[string]any:
		for k, e := range t {
			visit(k)
			walkStrings(e, visit)
		}
	}
}

func assertNoHostileInDecoded(t *testing.T, label string, raw []byte) {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("%s did not parse as JSON: %v\noutput: %s", label, err, raw)
	}
	walkStrings(decoded, func(s string) {
		for _, bad := range allHostile {
			if strings.Contains(s, bad) {
				t.Errorf("%s: decoded string %q still carries %q — `jq -r` would hand this to a terminal", label, s, bad)
			}
		}
	})
}

func TestAuditD_CLI_PrintJSONSanitizesEveryString(t *testing.T) {
	payload := map[string]any{
		"label":                        escClearScreen + "spoofed",
		"relays":                       []any{"wss://ok.example", "wss://" + c1CSI + "evil"},
		"nested":                       map[string]any{"identity_value": bidiOverride + "abc"},
		"sep":                          unicodeLineSep + "fake line",
		escClearScreen + "hostile_key": "ordinary value",
	}

	out := captureStdout(t, func() { PrintJSON(payload) })

	assertNoHostileInDecoded(t, "PrintJSON", out)
	for _, bad := range liveInRawJSON {
		if strings.Contains(string(out), bad) {
			t.Errorf("PrintJSON emitted %q raw on stdout — encoding/json does not escape C1 or bidi, so this is live with nothing parsing it", bad)
		}
	}
}

// TestAuditD_CLI_PrintJSONPreservesLargeIntegersExactly guards the fix rather
// than the defect, and it is the reason the round trip must use UseNumber():
// decoding into `any` without it yields float64, and amounts are bounded at
// MaxInt64, so every figure above 2^53 would silently lose precision. Losing a
// digit off an amount is a worse outcome than the escape sequence this change is
// meant to stop.
func TestAuditD_CLI_PrintJSONPreservesLargeIntegersExactly(t *testing.T) {
	const big = uint64(math.MaxInt64)
	out := captureStdout(t, func() {
		PrintJSON(map[string]any{"amount_millis": big, "nested": map[string]any{"fee_mloki": big - 1}})
	})
	for _, want := range []string{"9223372036854775807", "9223372036854775806"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("PrintJSON lost integer precision: %q absent from\n%s", want, out)
		}
	}
}

func TestAuditD_CLI_EmitErrorSanitizesJSONFields(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.Flags().Bool("json", true, "")
	err := &CLIError{
		Err:        &plainErr{escClearScreen + "the wallet declined"},
		Code:       CodeInternal,
		Input:      c1CSI + "some-input",
		RawMessage: bidiOverride + "hub said so",
		NWCCode:    escClearScreen + "INTERNAL",
	}

	out := captureStderr(t, func() { EmitError(cmd, err) })

	assertNoHostileInDecoded(t, "EmitError --json", out)
	for _, bad := range liveInRawJSON {
		if strings.Contains(string(out), bad) {
			t.Errorf("EmitError --json emitted %q raw", bad)
		}
	}
}

func TestAuditD_CLI_EmitErrorSanitizesHumanText(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.Flags().Bool("json", false, "")
	err := &CLIError{
		Err:        &plainErr{escClearScreen + "the wallet declined"},
		Code:       CodeInternal,
		RawMessage: c1CSI + "hub said so",
	}

	out := string(captureStderr(t, func() { EmitError(cmd, err) }))

	for _, bad := range allHostile {
		if strings.Contains(out, bad) {
			t.Errorf("EmitError human output carries %q, which can rewrite what the user is shown: %q", bad, out)
		}
	}
}

// TestAuditD_CLI_EmitErrorKeepsCashctlsOwnNewlines is the hazard guard, and it
// must keep passing. noWalletConfiguredMsg is a three-line string, so sanitizing
// composed error text with the STRICT Sanitize would turn cashctl's own newlines
// into U+FFFD and wreck legible multi-line help. A newline-permitting variant is
// required. Carriage return is deliberately not covered by that leniency — bare
// CR enables line-overwrite spoofing and nothing here emits one.
func TestAuditD_CLI_EmitErrorKeepsCashctlsOwnNewlines(t *testing.T) {
	multi := "no wallet configured yet.\n  Already have one?  cashctl connect add <name> <uri>\n  Want to join a circle instead?  cashctl join <hub>"

	for _, jsonMode := range []bool{false, true} {
		cmd := &cobra.Command{}
		cmd.Flags().Bool("json", jsonMode, "")
		out := string(captureStderr(t, func() {
			EmitError(cmd, &CLIError{Err: &plainErr{multi}, Code: CodeNotFound})
		}))

		if strings.Contains(out, "\ufffd") {
			t.Errorf("jsonMode=%v: cashctl's own newlines were replaced with U+FFFD:\n%s", jsonMode, out)
		}
		// In --json the newline is encoded as \n; in human mode it is literal.
		// Either way the three lines must still be distinguishable.
		if jsonMode {
			if !strings.Contains(out, `\n`) {
				t.Errorf("jsonMode=true: the multi-line message lost its newlines:\n%s", out)
			}
		} else if strings.Count(out, "\n") < 3 {
			t.Errorf("jsonMode=false: expected a 3-line message, got:\n%s", out)
		}
	}
}

// TestAuditD_CLI_SanitizeIsIdempotent is what licenses layering a central
// sanitize underneath the 23 call sites that already sanitize: U+FFFD is not
// itself an unsafe rune, so a second pass is a no-op and no existing call site
// has to change.
func TestAuditD_CLI_SanitizeIsIdempotent(t *testing.T) {
	for _, in := range []string{
		escClearScreen + "x", c1CSI, bidiOverride, unicodeLineSep,
		"ordinary text", "", "multi\nline\ttext",
	} {
		once := Sanitize(in)
		if twice := Sanitize(once); twice != once {
			t.Errorf("Sanitize is not idempotent for %q: once=%q twice=%q", in, once, twice)
		}
	}
}

// plainErr is an error whose message is returned verbatim, so a test can plant
// hostile bytes without any wrapping helper redacting or reformatting them.
type plainErr struct{ msg string }

func (e *plainErr) Error() string { return e.msg }
