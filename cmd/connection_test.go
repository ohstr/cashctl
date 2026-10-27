package cmd

import (
	"io"
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nipcash"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/output"

	"github.com/ohstr/cashctl/internal/testmint"
)

func TestValidateConnectionValue(t *testing.T) {
	walletPubkey, secret := strings.Repeat("aa", 32), strings.Repeat("bb", 32)
	cashToken, err := testmint.Encode(t, nipcash.Token{
		HRP: "lokicash", WalletPubkey: walletPubkey, Secret: secret, IdentityRequired: ptrTo(false),
		// Required by nipcash.Encode: a bill with no relay hint is unreachable.
		RelayURLs: []string{"wss://relay.example"},
	})
	if err != nil {
		t.Fatalf("nipcash.Encode: %v", err)
	}
	nwcURI := "nostr+walletconnect://" + walletPubkey + "?relay=wss%3A%2F%2Frelay.example&secret=" + secret

	if err := validateConnectionValue(nwcURI); err != nil {
		t.Errorf("validateConnectionValue(nwc URI) = %v, want accepted", err)
	}
	// A bill is never a wallet.
	if err := validateConnectionValue(cashToken); err == nil || !strings.Contains(err.Error(), "never a wallet") {
		t.Errorf("validateConnectionValue(cash token) = %v, want refused as a bill", err)
	}
	// "n"/"no" are what a person types at init's yes/no-sounding wallet
	// prompt; the rest are ordinary mis-pastes. None can ever be dialed.
	for _, bad := range []string{"", "n", "no", "garbage", "nostr+walletconnect://", "https://example.com", "npub1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq"} {
		if err := validateConnectionValue(bad); err == nil {
			t.Errorf("validateConnectionValue(%q) = nil, want rejected", bad)
		}
	}
}

// A Hub connection or a "<token>#<secret>" bill decodes under nipcash.Decode's
// any-HRP rule, but is never a wallet: each is refused, naming what it is.
func TestValidateConnectionValue_RefusesWhatIsNotAWallet(t *testing.T) {
	walletPubkey, secret := strings.Repeat("aa", 32), strings.Repeat("bb", 32)
	hubPriv, _ := testmint.Key(t)
	cashHub := testmint.CashHub(t, hubPriv, "")
	cashToken, err := testmint.Encode(t, nipcash.Token{
		HRP: "lokicash", WalletPubkey: walletPubkey, Secret: secret, RelayURLs: []string{"wss://relay.example"},
	})
	if err != nil {
		t.Fatalf("nipcash.Encode: %v", err)
	}
	for _, tt := range []struct{ value, want string }{
		{validCircleHub(t), "cashctl join"},
		{cashHub, "Cash Hub"},
		{cashToken + "#" + strings.Repeat("cc", 32), "cashctl receive"},
	} {
		err := validateConnectionValue(tt.value)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("validateConnectionValue(%.20q…) = %v, want a refusal naming %q", tt.value, err, tt.want)
		}
		if code := output.AsCLIError(unusableConnectionError(&cobra.Command{}, tt.value)).Code; code != output.CodeInvalidInput {
			t.Errorf("-c/--into %.20q…: code = %q, want invalid_input", tt.value, code)
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

// decode reads "<token>#<secret>" as the token plus an embedded secret: the
// combined string itself never decodes (NIP-CASH §Presenting a Cash-Mode Slice).
func TestDecode_SplitsTheCombinedCashString(t *testing.T) {
	token, err := testmint.Encode(t, nipcash.Token{
		HRP: "lokicash", WalletPubkey: strings.Repeat("aa", 32), Secret: strings.Repeat("bb", 32),
		RelayURLs: []string{"wss://relay.example"},
	})
	if err != nil {
		t.Fatalf("nipcash.Encode: %v", err)
	}
	cmd := newDecodeCmd()
	cmd.SetArgs([]string{token + "#" + strings.Repeat("cc", 32)})
	cmd.SetOut(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Errorf("decode <token>#<secret> = %v, want the token decoded", err)
	}
}
