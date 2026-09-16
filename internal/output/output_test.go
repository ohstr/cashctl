package output

import (
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
