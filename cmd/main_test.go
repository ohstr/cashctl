package cmd

import (
	"context"
	"os"
	"testing"
)

// TestMain keeps unit tests off real relays where a command reads a bill's
// network: a test that cares installs its own billNetworkFn.
func TestMain(m *testing.M) {
	billNetworkFn = func(context.Context, string) (string, error) { return "mainnet", nil }
	os.Exit(m.Run())
}
