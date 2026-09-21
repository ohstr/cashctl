package main

import "testing"

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
