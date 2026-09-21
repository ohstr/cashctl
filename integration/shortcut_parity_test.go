//go:build integration

package integration

import (
	"strings"
	"testing"
)

// The docker-style top-level shortcuts (`transfer`) are clones of their
// canonical `group sub` form (`cash transfer`) built by cmd/shortcuts.go's
// shortcutOf. They must be interchangeable: same result, same error, same
// exit code, same flags. Each pair below is run with IDENTICAL arguments
// (offline, on deterministic failure paths) in both output modes.

type twinPair struct {
	name         string
	short, canon []string
}

var twinPairs = []twinPair{
	{"receive", []string{"receive", "not-a-token"}, []string{"cash", "receive", "not-a-token"}},
	{"redeem", []string{"redeem", "--yes"}, []string{"cash", "redeem", "--yes"}},
	{"transfer", []string{"transfer", "1"}, []string{"cash", "transfer", "1"}},
	{"consolidate", []string{"consolidate", "--yes"}, []string{"cash", "consolidate", "--yes"}},
	{"join", []string{"join", "garbage", "10"}, []string{"circle", "join", "garbage", "10"}},
	{"pay", []string{"pay", "garbage"}, []string{"wallet", "pay", "garbage"}},
	{"invoice", []string{"invoice", "1"}, []string{"wallet", "invoice", "1"}},
	{"balance", []string{"balance"}, []string{"wallet", "balance"}},
	{"use", []string{"connect", "use", "nope"}, []string{"wallet", "use", "nope"}}, // two canonical spellings of one command
}

func TestShortcutParity_SameResultAndErrors(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")
	for _, p := range twinPairs {
		t.Run(p.name, func(t *testing.T) {
			f.t = t
			a, b := f.run(p.short...), f.run(p.canon...)
			if a.ExitCode != b.ExitCode || a.Stdout != b.Stdout || a.Stderr != b.Stderr {
				t.Errorf("--json: `%s` and `%s` differ\n  exit %d vs %d\n  stdout %q vs %q\n  stderr %q vs %q",
					strings.Join(p.short, " "), strings.Join(p.canon, " "),
					a.ExitCode, b.ExitCode, a.Stdout, b.Stdout, a.Stderr, b.Stderr)
			}
			ta, tb := f.runInteractive("", p.short...), f.runInteractive("", p.canon...)
			if ta.ExitCode != tb.ExitCode || ta.Stdout != tb.Stdout || ta.Stderr != tb.Stderr {
				t.Errorf("text: `%s` and `%s` differ\n  exit %d vs %d\n  stdout %q vs %q\n  stderr %q vs %q",
					strings.Join(p.short, " "), strings.Join(p.canon, " "),
					ta.ExitCode, tb.ExitCode, ta.Stdout, tb.Stdout, ta.Stderr, tb.Stderr)
			}
		})
	}
}

// `init` is a shortcut for `wallet init`: both must set up an identity the
// same way, and both must report "already configured" identically on a
// second run.
func TestShortcutParity_Init(t *testing.T) {
	a, b := newFixture(t), newFixture(t)
	ra, rb := a.run("init"), b.run("wallet", "init")
	if ra.ExitCode != 0 || rb.ExitCode != 0 {
		t.Fatalf("init exit %d / wallet init exit %d\n%s\n%s", ra.ExitCode, rb.ExitCode, ra.Stderr, rb.Stderr)
	}
	ja, jb := mustDecodeJSON(t, "init", ra.Stdout), mustDecodeJSON(t, "wallet init", rb.Stdout)
	for k := range ja {
		if _, ok := jb[k]; !ok {
			t.Errorf("`init` returns key %q that `wallet init` does not", k)
		}
	}
	for k := range jb {
		if _, ok := ja[k]; !ok {
			t.Errorf("`wallet init` returns key %q that `init` does not", k)
		}
	}
	if s, c := a.run("init"), b.run("wallet", "init"); s.ExitCode != c.ExitCode || s.Stdout != c.Stdout || s.Stderr != c.Stderr {
		t.Errorf("second run differs: init -> (%d) %q / wallet init -> (%d) %q", s.ExitCode, s.Stdout, c.ExitCode, c.Stdout)
	}
}

// --help of a shortcut and of its canonical form must document the same
// flags (they share one flag set) and the same one-line summary.
func TestShortcutParity_HelpDocumentsSameFlags(t *testing.T) {
	f := newFixture(t)
	flagsSection := func(help string) (summary string, flags []string) {
		lines := strings.Split(help, "\n")
		if len(lines) > 0 {
			summary = strings.TrimSpace(lines[0])
		}
		in := false
		for _, l := range lines {
			switch {
			case strings.HasPrefix(l, "Flags:"):
				in = true
			case strings.HasPrefix(l, "Global Flags:") || strings.TrimSpace(l) == "":
				in = false
			case in:
				flags = append(flags, strings.TrimSpace(l))
			}
		}
		return summary, flags
	}
	for _, p := range twinPairs {
		short := append(append([]string{}, p.short[:1]...), "--help")
		if p.name == "use" {
			short = []string{"connect", "use", "--help"}
		}
		canonBase := p.canon[:2]
		canon := append(append([]string{}, canonBase...), "--help")
		a, b := f.rawRun(append([]string{"--config-dir", f.configDir}, short...)...), f.rawRun(append([]string{"--config-dir", f.configDir}, canon...)...)
		if a.ExitCode != 0 || b.ExitCode != 0 {
			t.Errorf("%s: --help exit %d / %d", p.name, a.ExitCode, b.ExitCode)
			continue
		}
		sa, fa := flagsSection(a.Stdout)
		sb, fb := flagsSection(b.Stdout)
		// `connect use` / `wallet use` are two canonical spellings, not a
		// shortcut clone, and word their summary for their own group.
		if sa != sb && p.name != "use" {
			t.Errorf("%s: summary differs: %q vs %q", p.name, sa, sb)
		}
		if strings.Join(fa, "\n") != strings.Join(fb, "\n") {
			t.Errorf("%s: documented flags differ\n--- %v\n%s\n--- %v\n%s", p.name, short, strings.Join(fa, "\n"), canon, strings.Join(fb, "\n"))
		}
	}
}
