package cmd

import (
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nipcw"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/output"
)

// A join refused because you already hold a wallet from that circle names the
// wallet it is registered as here, keeping the exists code.
func TestWithHeldWalletHint_NamesTheCircleWallet(t *testing.T) {
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	s, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add("circle:friends", "nostr+walletconnect://"+strings.Repeat("a", 64)+"?relay=wss://r.example&secret="+strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	refusal := classifyNWCErr(&cobra.Command{}, &relayclient.WalletError{Code: nipcw.ErrWalletExists, Message: "exists"})
	err = withHeldWalletHint(refusal, "Friends")
	if ce := output.AsCLIError(err); ce.Code != output.CodeExists {
		t.Errorf("code = %q, want exists", ce.Code)
	}
	if !strings.Contains(err.Error(), "-c circle:friends") {
		t.Errorf("%v does not name the held wallet", err)
	}

	other := withHeldWalletHint(classifyNWCErr(&cobra.Command{}, &relayclient.WalletError{Code: "RESTRICTED", Message: "no"}), "Friends")
	if strings.Contains(other.Error(), "circle:friends") {
		t.Errorf("a non-exists refusal got the held-wallet hint: %v", other)
	}
}
