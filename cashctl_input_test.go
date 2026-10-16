package main

import "testing"

// A usage error from cobra/pflag names the one value that was wrong as its
// input, so an agent need not parse the message.
func TestRootErrInput(t *testing.T) {
	for msg, want := range map[string]string{
		"unknown flag: --bogus":                      "--bogus",
		"unknown shorthand flag: 'Z' in -Z":          "-Z",
		`unknown command "frob" for "cashctl"`:       "frob",
		"flag needs an argument: --amount":           "--amount",
		"flag needs an argument: 'c' in -c":          "-c",
		`invalid argument "x" for "--yes" flag: bad`: "x",
		"something else entirely":                    "",
	} {
		if got := rootErrInput(msg); got != want {
			t.Errorf("rootErrInput(%q) = %q, want %q", msg, got, want)
		}
	}
}
