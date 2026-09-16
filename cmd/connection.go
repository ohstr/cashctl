package cmd

import (
	"context"
	"fmt"

	"github.com/ohstr/nmilat/nip47"
	"github.com/ohstr/nmilat/nipcash"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/config"
)

// noWalletConfiguredMsg is shown whenever a command needs a wallet to act
// on/into and none is configured — the exact remediation text from
// cashctl-plan.md's walkthrough #3.
const noWalletConfiguredMsg = `no wallet configured yet.
  Already have one?  cashctl connect add <name> <connection-uri>
  Want to join a circle instead?  cashctl join <hub-connection>`

// ResolveConnectionValue returns the raw connection string to dial: -c/
// --connection wins if given (resolved by store name, or used directly as
// a raw value if it isn't a known name — lets a script pass a raw URI/
// token inline without first running `connect add`), otherwise the
// store's default wallet. ok=false with a nil error means "no wallet
// configured" — most callers should treat that as noWalletConfiguredMsg,
// but redeem's own auto-invoice path has a third option (--invoice) so it
// builds a slightly different message itself.
func ResolveConnectionValue(cmd *cobra.Command) (value string, ok bool, err error) {
	explicit, _ := cmd.Flags().GetString("connection")
	s, err := config.Load()
	if err != nil {
		return "", false, err
	}
	if explicit != "" {
		if c, found := s.Find(explicit); found {
			return c.Value, true, nil
		}
		return explicit, true, nil
	}
	c, found := s.DefaultConnection()
	if !found {
		return "", false, nil
	}
	return c.Value, true, nil
}

// DialGeneric connects to any NWC-capable connection string — a plain
// nostr+walletconnect:// URI, or a cash-token-family bech32 string (which
// carries the identical pairing data, just packaged differently — see
// `connect add`'s own "pairing-uri-or-cash-token" acceptance).
func DialGeneric(ctx context.Context, value string) (*relayclient.NWCClient, error) {
	if pairing, err := nip47.ParsePairingURI(value); err == nil {
		return relayclient.NewNWCClient(ctx, pairing, nip47.EncryptionNIP44V2)
	}
	tok, err := nipcash.Decode(value)
	if err != nil {
		return nil, fmt.Errorf("not a valid connection string")
	}
	pairing := &nip47.PairingInfo{WalletPubkey: tok.WalletPubkey, RelayURLs: tok.RelayURLs, Secret: tok.Secret}
	return relayclient.NewNWCClient(ctx, pairing, nip47.EncryptionNIP44V2)
}
