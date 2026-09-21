package cmd

import (
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nipcash"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/config"
)

func TestValidateConnectionValue(t *testing.T) {
	walletPubkey, secret := strings.Repeat("aa", 32), strings.Repeat("bb", 32)
	cashToken, err := nipcash.Encode(nipcash.Token{
		HRP: "lokicash", WalletPubkey: walletPubkey, Secret: secret, IdentityRequired: ptrTo(false),
	})
	if err != nil {
		t.Fatalf("nipcash.Encode: %v", err)
	}
	nwcURI := "nostr+walletconnect://" + walletPubkey + "?relay=wss%3A%2F%2Frelay.example&secret=" + secret

	for _, ok := range []string{nwcURI, cashToken} {
		if err := validateConnectionValue(ok); err != nil {
			t.Errorf("validateConnectionValue(%.40q…) = %v, want accepted", ok, err)
		}
	}
	// "n"/"no" are what a person types at init's yes/no-sounding wallet
	// prompt; the rest are ordinary mis-pastes. None can ever be dialed.
	for _, bad := range []string{"", "n", "no", "garbage", "nostr+walletconnect://", "https://example.com", "npub1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq"} {
		if err := validateConnectionValue(bad); err == nil {
			t.Errorf("validateConnectionValue(%q) = nil, want rejected", bad)
		}
	}
}

func TestNoWalletMessage_DistinguishesNoWalletsFromNoDefault(t *testing.T) {
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })

	if got := noWalletMessage(); got != noWalletConfiguredMsg {
		t.Errorf("with nothing registered: %q, want the generic add/join advice", got)
	}

	s, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Add("savings", "nostr+walletconnect://x")
	_ = s.Add("work", "nostr+walletconnect://y")
	if err := s.Save(); err != nil { // registered, but neither is the default
		t.Fatal(err)
	}
	got := noWalletMessage()
	if strings.Contains(got, "no wallet configured yet") {
		t.Errorf("wallets exist but none is default; still told %q", got)
	}
	for _, want := range []string{"cashctl wallet use <name>", "savings", "work", "2 registered"} {
		if !strings.Contains(got, want) {
			t.Errorf("message %q is missing %q", got, want)
		}
	}
}
