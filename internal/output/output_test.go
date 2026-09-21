package output

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestSanitize_StripsANSIEscapeSequences(t *testing.T) {
	// "\x1b[2J\x1b[H" (clear screen, home cursor) prefixed to a label —
	// the exact shape of a terminal-spoofing attempt via a Circle Hub's
	// own connection label or a relay URL.
	in := "\x1b[2J\x1b[Hlegit-looking-label"
	got := Sanitize(in)
	if strings.ContainsRune(got, '\x1b') {
		t.Errorf("Sanitize(%q) = %q, still contains ESC (0x1B)", in, got)
	}
	if !strings.Contains(got, "legit-looking-label") {
		t.Errorf("Sanitize(%q) = %q, want the printable suffix preserved", in, got)
	}
}

func TestSanitize_ReplacesControlCharsVisibly_NeverSilentlyDeletes(t *testing.T) {
	// A silent strip could be used to shrink a string into looking like a
	// different, shorter one — replacing with a visible marker instead
	// keeps tampering visible.
	got := Sanitize("a\x00b\x07c\x1bd\x7fe")
	if strings.Count(got, "�") != 4 {
		t.Errorf("Sanitize() = %q, want each of the 4 control bytes replaced with a visible marker, not deleted", got)
	}
}

func TestSanitize_OrdinaryTextUnchanged(t *testing.T) {
	in := "Alice's Family Circle — v2 (日本語 OK)"
	if got := Sanitize(in); got != in {
		t.Errorf("Sanitize(%q) = %q, want unchanged (no control characters present)", in, got)
	}
}

func TestSanitize_EmptyString(t *testing.T) {
	if got := Sanitize(""); got != "" {
		t.Errorf("Sanitize(\"\") = %q, want empty", got)
	}
}

func TestFormatAmount(t *testing.T) {
	cases := []struct {
		mloki int64
		want  string
	}{
		{0, "0 loki"},
		{1000, "1 loki"},
		{1500, "1.5 loki"},
		{1005, "1.005 loki"},
		{1050, "1.05 loki"},
		{999, "0.999 loki"},
		{1, "0.001 loki"},
		{-1500, "-1.5 loki"},
		{10_000_000, "10000 loki"},
	}
	for _, c := range cases {
		if got := FormatAmount(c.mloki); got != c.want {
			t.Errorf("FormatAmount(%d) = %q, want %q", c.mloki, got, c.want)
		}
	}
}

func TestParseAmount(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
	}{
		{"0", 0},
		{"5", 5000},
		{"0.5", 500},
		{"1.234", 1234},
		{"0.001", 1},
		{"10000", 10_000_000},
		{"1.5", 1500},
		{"1.05", 1050},
		{"  5  ", 5000}, // surrounding whitespace trimmed, matching resolvePositionalOrFlag's own convention
	}
	for _, c := range cases {
		got, err := ParseAmount(c.in)
		if err != nil {
			t.Errorf("ParseAmount(%q) error = %v, want nil", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseAmount(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseAmount_RejectsInvalid(t *testing.T) {
	for _, in := range []string{
		"", "   ", "-5", "abc", "1.2.3", "1.", "1.2345", "5 loki", "5,000",
	} {
		if _, err := ParseAmount(in); err == nil {
			t.Errorf("ParseAmount(%q) = nil error, want an error", in)
		}
	}
}

func TestParseAmount_LeadingDotIsHalfALoki(t *testing.T) {
	// ".5" (no leading "0") is a common shorthand — accepted the same as
	// "0.5", not treated as a missing whole part.
	got, err := ParseAmount(".5")
	if err != nil {
		t.Fatalf("ParseAmount(\".5\") error = %v", err)
	}
	if got != 500 {
		t.Errorf("ParseAmount(\".5\") = %d, want 500", got)
	}
}

// TestParseAmount_RejectsOverflow is the regression test for a real bug:
// `transfer <target> 18446744073709552` used to overflow uint64 during the
// loki -> mloki multiply and silently become 384 mloki (0.384 loki) — the
// call then succeeded and moved that (wrong) small amount for real.
// Neighbouring values separately went negative the moment any downstream
// int64(...) cast touched them (FormatAmount's own signature, the invoice
// amount cast in cmd/wallet_ops.go, ...) even where the uint64 multiply
// itself didn't wrap. Every case here must be rejected outright, not
// silently reinterpreted as a valid, wildly different amount.
func TestParseAmount_RejectsOverflow(t *testing.T) {
	for _, in := range []string{
		"18446744073709552",                // wraps uint64 in the *1000 multiply -> used to become 384
		"18446744073709551",                // wraps uint64 differently -> used to become a huge positive mloki value
		"9223372036854776",                 // fits uint64 but negative once cast to int64 downstream
		"9223372036854775.9",               // same boundary, exercised via the fractional-part addition
		"99999999999999999999999999999999", // absurdly large, not even close to fitting any integer type
	} {
		if got, err := ParseAmount(in); err == nil {
			t.Errorf("ParseAmount(%q) = %d, nil error — want it rejected as too large", in, got)
		}
	}
}

// TestParseAmount_AcceptsTheExactBoundary confirms the fix is a boundary,
// not an overcorrection: the largest amount that fits in an int64 mloki
// value must still parse, and one loki more must not.
func TestParseAmount_AcceptsTheExactBoundary(t *testing.T) {
	const maxInt64 = 1<<63 - 1
	maxLoki := maxInt64 / 1000 // whole loki that fits with room for .000-.999 mloki of slack under the cap
	got, err := ParseAmount(fmt.Sprintf("%d", maxLoki))
	if err != nil {
		t.Fatalf("ParseAmount(%d) (just under the int64 mloki boundary) error = %v", maxLoki, err)
	}
	if got != uint64(maxLoki)*1000 {
		t.Errorf("ParseAmount(%d) = %d, want %d", maxLoki, got, uint64(maxLoki)*1000)
	}
	if _, err := ParseAmount(fmt.Sprintf("%d", maxLoki+1_000_000)); err == nil {
		t.Errorf("ParseAmount(%d) (well past the boundary) = nil error, want rejected", maxLoki+1_000_000)
	}
}

func TestParseAmount_RoundTripsWithFormatAmount(t *testing.T) {
	for _, mloki := range []int64{0, 1, 5, 999, 1000, 1234, 10_000_000} {
		formatted := FormatAmount(mloki)
		loki := strings.TrimSuffix(formatted, " loki")
		got, err := ParseAmount(loki)
		if err != nil {
			t.Fatalf("ParseAmount(%q) (from FormatAmount(%d)) error = %v", loki, mloki, err)
		}
		if int64(got) != mloki {
			t.Errorf("ParseAmount(FormatAmount(%d)) = %d, want %d", mloki, got, mloki)
		}
	}
}

// --- errorPrefix: the red "Error:" wrapping, factored out of EmitError so
// it's testable independent of real terminal detection (a captured pipe
// in a test is never a TTY, so exercising EmitError itself could only
// ever hit the uncolored branch — see errors_test.go's own captureStderr).

func TestErrorPrefix_ColoredWrapsInRed(t *testing.T) {
	got := errorPrefix(true)
	if !strings.Contains(got, "\x1b[31m") || !strings.Contains(got, "\x1b[0m") {
		t.Errorf("errorPrefix(true) = %q, want it wrapped in ANSI red (\\x1b[31m...\\x1b[0m)", got)
	}
	if !strings.Contains(got, "Error:") {
		t.Errorf("errorPrefix(true) = %q, want it to still contain the literal text \"Error:\"", got)
	}
}

func TestErrorPrefix_UncoloredIsPlainText(t *testing.T) {
	got := errorPrefix(false)
	if got != "Error:" {
		t.Errorf("errorPrefix(false) = %q, want exactly \"Error:\" — no ANSI bytes for a script/agent parsing piped/non-TTY stderr", got)
	}
}

// TestSanitize_StripsC1ControlChars is the regression test for a real
// terminal-spoofing gap: Sanitize's original check (r < 0x20 || r == 0x7f)
// only covered the C0 range — a terminal that honors 8-bit C1 controls in
// UTF-8 mode (xterm, some VTE builds) executes CSI (U+009B), OSC (U+009D)
// and ST (U+009C) from the C1 range exactly like their ESC-prefixed C0
// equivalents, so a hostile relay URL embedded in a token/hub string could
// clear the screen or set the window title at decode/receive time with no
// ESC byte anywhere in it.
func TestSanitize_StripsC1ControlChars(t *testing.T) {
	in := "\u009b31m\u009b2J\u009d0;PWNED-TITLE\u009c"
	got := Sanitize(in)
	for _, r := range []rune{0x9b, 0x9c, 0x9d} {
		if strings.ContainsRune(got, r) {
			t.Errorf("Sanitize(%q) = %q, still contains C1 control U+%04X", in, got, r)
		}
	}
}

// TestSanitize_StripsBidiAndLineSeparatorChars covers the other two
// non-printable-but-not-caught-by-the-old-check families: bidi format
// controls (can reorder how the same bytes visually display) and the
// Unicode line/paragraph separators (some terminals treat them as a hard
// line break, injecting a fake extra line).
func TestSanitize_StripsBidiAndLineSeparatorChars(t *testing.T) {
	for _, r := range []rune{0x2028, 0x2029, 0x202a, 0x202e, 0x2066, 0x2069} {
		in := "before" + string(r) + "after"
		if got := Sanitize(in); strings.ContainsRune(got, r) {
			t.Errorf("Sanitize(%q) = %q, still contains U+%04X", in, got, r)
		}
	}
}

// encoding/json renders a nil slice as null: a --json consumer iterating a
// collection (`jq '.[]'`, a typed decoder) needs [] for "nothing here".
func TestNonNil_EncodesEmptyCollectionsAsArrays(t *testing.T) {
	var none []string
	b, err := json.Marshal(map[string]any{"raw": none, "wrapped": NonNil(none), "empty": NonNil([]string{})})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"empty":[],"raw":null,"wrapped":[]}`; got != want {
		t.Errorf("encoded = %s, want %s", got, want)
	}
}

func TestNonNil_LeavesAPopulatedSliceAlone(t *testing.T) {
	in := []int{1, 2, 3}
	out := NonNil(in)
	if len(out) != 3 || &out[0] != &in[0] {
		t.Errorf("NonNil(%v) = %v, want the same slice back", in, out)
	}
}

func TestIsColorTerminal_NoColorEnvAlwaysFalse(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	// os.Stderr itself may or may not be a TTY in the test runner — NO_COLOR
	// must win either way, so this doesn't need a real terminal to prove.
	if isColorTerminal(os.Stderr) {
		t.Error("isColorTerminal() = true with NO_COLOR set, want false")
	}
}
