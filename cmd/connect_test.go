package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/output"
)

func TestWalletUse_UnknownNamePointsToConnectList(t *testing.T) {
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	cmd := &cobra.Command{}
	cmd.Flags().Bool("json", false, "")
	err := runWalletUse(cmd, []string{"nope"})
	if ce := output.AsCLIError(err); ce == nil || ce.Code != output.CodeNotFound {
		t.Fatalf("runWalletUse(nope) = %v, want not_found", err)
	}
	if !strings.Contains(err.Error(), "cashctl connect list") {
		t.Errorf("runWalletUse(nope) = %q, want it to point to `cashctl connect list`", err)
	}
}
