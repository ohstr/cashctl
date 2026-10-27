// Package extwallet is the one place cashctl reads a nostr+walletconnect://
// URI: a plain NIP-47 wallet outside the cash NIPs (connect add, -c,
// redeem --into, decode). No cash credential — a bill, a cashhub1, a
// cashorder1 or a circlehub1 — relates to that URI form, and nothing in cashctl
// builds one; isolation_test.go keeps the cash, ledger and store packages from
// reaching this one.
package extwallet

import (
	"context"
	"strings"

	"github.com/ohstr/nmilat/nip47"
	relayclient "github.com/ohstr/nmilat/relay/client"
)

// IsURI reports whether s has the URI form's scheme, without parsing it.
func IsURI(s string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), "nostr+walletconnect:")
}

// Parse reads an outside wallet's URI.
func Parse(uri string) (*nip47.PairingInfo, error) {
	return nip47.ParsePairingURI(uri)
}

// Dial opens a NIP-47 client to an outside wallet's URI.
func Dial(ctx context.Context, uri string) (*relayclient.NWCClient, error) {
	pairing, err := Parse(uri)
	if err != nil {
		return nil, err
	}
	return relayclient.NewNWCClient(ctx, pairing, nip47.EncryptionNIP44V2)
}

// Label names an outside wallet without its secret: the start of its wallet
// key, or "" when the URI does not parse.
func Label(uri string) string {
	info, err := Parse(uri)
	if err != nil || len(info.WalletPubkey) < 8 {
		return ""
	}
	return "wallet " + info.WalletPubkey[:8] + "…"
}
