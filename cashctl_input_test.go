package main

import (
	"errors"
	"testing"

	"github.com/ohstr/cashctl/cmd"
	"github.com/ohstr/cashctl/internal/output"
)

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

// classifyRootErr carries that value through as the usage error's input.
func TestClassifyRootErr_SetsInput(t *testing.T) {
	err := classifyRootErr(cmd.RootCmd, errors.New("unknown flag: --bogus"))
	if ce := output.AsCLIError(err); ce.Code != output.CodeUsage || ce.Input != "--bogus" {
		t.Errorf("code %q input %q, want usage, --bogus", ce.Code, ce.Input)
	}
}
