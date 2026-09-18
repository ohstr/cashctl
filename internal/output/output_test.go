package output

import (
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

func TestIsColorTerminal_NoColorEnvAlwaysFalse(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	// os.Stderr itself may or may not be a TTY in the test runner — NO_COLOR
	// must win either way, so this doesn't need a real terminal to prove.
	if isColorTerminal(os.Stderr) {
		t.Error("isColorTerminal() = true with NO_COLOR set, want false")
	}
}
