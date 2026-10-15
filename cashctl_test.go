package main

import (
	"errors"
	"testing"

	"github.com/spf13/cobra"
)

func TestJSONRequested(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"cash", "recieve"}, false},
		{[]string{"--json", "bogus"}, true},
		{[]string{"--json=true", "bogus"}, true},
		{[]string{"--json=1", "bogus"}, true},
		{[]string{"--json=TRUE", "bogus"}, true},
		{[]string{"--json=false", "bogus"}, false},
		{[]string{"--json", "--json=false"}, false}, // last one wins, as in cobra
		{[]string{"--json=false", "--json"}, true},
		{[]string{"--json=maybe", "bogus"}, false}, // not a bool: ignored, not "on"
		{[]string{"bogus", "--", "--json"}, false}, // past the terminator it's a positional
		{[]string{"--jsonx"}, false},
	}
	for _, c := range cases {
		if got := jsonRequested(c.args); got != c.want {
			t.Errorf("jsonRequested(%q) = %v, want %v", c.args, got, c.want)
		}
	}
}

func TestWithHelpHint(t *testing.T) {
	c := &cobra.Command{Use: "receive"}
	root := &cobra.Command{Use: "cashctl"}
	root.AddCommand(c)
	c.Flags().String("token", "", "")
	c.Flags().Bool("check", false, "")
	tests := []struct{ in, want string }{
		{"unknown flag: --tokn", "unknown flag: --tokn; did you mean --token?\nSee `cashctl receive --help`."},
		{"unknown flag: --tok", "unknown flag: --tok; did you mean --token?\nSee `cashctl receive --help`."},
		{"unknown flag: --zzzzzz", "unknown flag: --zzzzzz\nSee `cashctl receive --help`."},
		{"unknown shorthand flag: 'q' in -q", "unknown shorthand flag: 'q' in -q\nSee `cashctl receive --help`."},
		{"accepts 1 arg(s), received 2", "accepts 1 arg(s), received 2"},
	}
	for _, tt := range tests {
		if got := withHelpHint(c, errors.New(tt.in)).Error(); got != tt.want {
			t.Errorf("withHelpHint(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
