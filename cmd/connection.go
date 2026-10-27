package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/flokiorg/go-flokicoin/chainutil/bech32"
	"github.com/ohstr/nmilat/nip47"
	"github.com/ohstr/nmilat/nipcash"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/dial"
	"github.com/ohstr/cashctl/internal/extwallet"
	"github.com/ohstr/cashctl/internal/output"
)

// noWalletConfiguredMsg is shown whenever a command needs a wallet to act
// on/into and none is configured — the exact remediation text from
// cashctl-plan.md's walkthrough #3.
const noWalletConfiguredMsg = `no wallet configured yet.
  Already have one?  cashctl connect add <name> <connection-uri>
  Want to join a circle instead?  cashctl join <hub-connection>`

// unusableConnectionError is the not_found error for a wallet reference that
// is neither a registered name nor a dialable connection string — the
// caller's own mistake, so never `network`/retryable (see
// ResolveConnectionValue). Shared with redeem's --into/-c handling.
func unusableConnectionError(cmd *cobra.Command, value string) error {
	// A string recognizably meant for something else is invalid_input naming what
	// it is, not "no wallet named <a whole Hub string>".
	var wrong *wrongConnectionKindError
	if err := validateConnectionValue(value); errors.As(err, &wrong) {
		return output.InvalidInputError(cmd, value, err)
	}
	return output.NotFoundError(cmd, value, fmt.Errorf(
		"no wallet named %q, and it isn't a connection string either — see `cashctl connect list` for registered names", output.Sanitize(value)))
}

// nostrEntityMessage describes what a dial.KindNostrEntity-classified
// value actually is, for `decode`/`receive`'s own mis-paste error — see
// dial.KindNostrEntity's own doc comment for the bug this replaces: a
// pasted npub/nsec/nconnection/... used to fall straight through to
// nipcash.Decode (which accepts any HRP) and fail with a cryptic
// "nipcash: truncated TLV entry at offset N" that named neither what was
// pasted nor why it was wrong. nsec gets its own explicit warning — the
// actual bytes are already redacted from any error by RedactSecretInput's
// secretLikePattern, but the generic message alone gave no hint the user
// had just pasted a private key into the wrong place.
// lightningMessage names a pasted Lightning invoice (with its amount, when it
// encodes one) or LNURL, and says what to do with it instead.
func lightningMessage(value string) string {
	if hrp, _, err := bech32.DecodeNoLimit(strings.TrimSpace(value)); err == nil && strings.EqualFold(hrp, "lnurl") {
		return "that's an LNURL, not a cash token or connection — cashctl doesn't resolve LNURLs"
	}
	what := "a Lightning invoice"
	if amt, err := bolt11AmountMloki(value); err == nil && amt != nil {
		what = "a Lightning invoice for " + output.FormatAmount(*amt)
	}
	return "that's " + what + ", not a cash token or connection — pay it with `cashctl pay <invoice>`, or redeem a held token into it with `cashctl redeem --invoice <invoice>`"
}

func nostrEntityMessage(hrp string) string {
	switch hrp {
	case "nsec":
		return "that's a Nostr PRIVATE KEY (nsec1...), not a cash token — never paste it here or anywhere else; there's no way to know if it's already been exposed"
	case "npub":
		return "that's a Nostr public key (npub1...), not a cash token — there's nothing to decode/receive directly from an identity, only from a token addressed to one"
	case "nconnection":
		return "that's an nconnection (a connection-key target for `transfer --to`), not a cash token"
	default:
		return fmt.Sprintf("that's a Nostr %s reference, not a cash token", hrp)
	}
}

// rejectConnectionFlag fails a command that never dials a registered wallet
// when -c/--connection was passed anyway. The flag is global, so cobra
// accepts it everywhere — but on `transfer`, `consolidate`, `receive`,
// `decode`, `cash status` and `wallet protect` it used to do nothing at all,
// silently: a script that names a wallet expects that wallet to be the one
// involved, and nothing here is (cash tokens are spent through their own
// Hub). Failing fast, before anything runs, keeps that expectation from
// going unnoticed; nothing has moved when this fires.
func rejectConnectionFlag(cmd *cobra.Command) error {
	if c, _ := cmd.Flags().GetString("connection"); c != "" {
		return output.InvocationErrorInput(cmd, "--connection", fmt.Errorf(
			"-c/--connection doesn't apply to `%s` — it never uses a registered wallet (cash tokens are spent through their own Hub). Remove the flag",
			cmd.CommandPath()))
	}
	return nil
}

// noWalletMessage is the "you can't do this yet" text for a command that
// needs a wallet and has none to use. Two different situations share that
// symptom and need different remedies: no wallet registered at all
// (noWalletConfiguredMsg's own add/join advice), and wallets that exist but
// none of them is the default — after declining a "set as default?" prompt,
// or removing the default one. The second used to be told "no wallet
// configured yet" too, sending someone who has wallets to add another
// instead of pointing at `wallet use`. Reads the store only on this error
// path; if it can't, falls back to the generic text.
func noWalletMessage() string {
	s, err := config.Load()
	if err != nil || s.IsEmpty() {
		return noWalletConfiguredMsg
	}
	names := make([]string, len(s.Connections))
	for i, c := range s.Connections {
		names[i] = output.Sanitize(c.Name)
	}
	return fmt.Sprintf("no default wallet set — you have %d registered: %s.\n  Pick one:  cashctl wallet use <name>\n  Or use one just this once:  add -c <name>",
		len(names), strings.Join(names, ", "))
}

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
		// Not a registered name, so it has to be a raw connection string —
		// and if it isn't one of those either it can never work. Say so here,
		// as the caller's own mistake: falling through to dial it used to
		// surface as `network`, retryable:true, so a script or agent that
		// backs off and retries on that would loop on a typo forever.
		if err := validateConnectionValue(explicit); err != nil {
			return "", false, unusableConnectionError(cmd, explicit)
		}
		return explicit, true, nil
	}
	c, found := s.DefaultConnection()
	if !found {
		return "", false, nil
	}
	return c.Value, true, nil
}

// validateConnectionValue reports whether value is something DialGeneric can
// actually use — the same two forms it accepts, checked without a network
// call. Registering anything else (a stray "n" typed at init's wallet
// prompt, a mis-paste, a Hub string) used to succeed silently and only fail
// later, on the first command that needed the wallet, as a confusing
// "not a valid connection string" far from where the mistake was made.
func validateConnectionValue(value string) error {
	if _, err := extwallet.Parse(value); err == nil {
		return nil
	}
	// Classified before decoding: nipcash.Decode accepts any HRP, so a Hub
	// string would otherwise register as a wallet that never works.
	if tok, secret := nipcash.SplitCashSliceString(value); secret != "" && dial.Sniff(tok) == dial.KindCashToken {
		return &wrongConnectionKindError{"that's a cash bill with its secret (<token>#<secret>), not a wallet — take it with `cashctl receive`"}
	}
	switch dial.Sniff(value) {
	case dial.KindCircleHub:
		return &wrongConnectionKindError{"that's a Circle Hub connection, not a wallet — join the circle with `cashctl join <hub-connection> <max-amount>`"}
	case dial.KindCashHub:
		return &wrongConnectionKindError{"that's a Cash Hub connection (for minting cash), not a wallet — cashctl can't use it"}
	case dial.KindLightningInvoice:
		return &wrongConnectionKindError{lightningMessage(value)}
	case dial.KindCashToken:
		if _, err := nipcash.Decode(value); err == nil {
			return nil
		}
	}
	return fmt.Errorf("not a valid wallet connection — expected a nostr+walletconnect:// URI (or a cash-token-family connection string)")
}

// wrongConnectionKindError is validateConnectionValue's refusal of a string
// that is recognizably something other than a wallet connection.
type wrongConnectionKindError struct{ msg string }

func (e *wrongConnectionKindError) Error() string { return e.msg }

// DialGeneric connects to any NWC-capable connection string — a plain
// nostr+walletconnect:// URI, or a cash-token-family bech32 string (which
// carries the identical pairing data, just packaged differently — see
// `connect add`'s own "pairing-uri-or-cash-token" acceptance).
func DialGeneric(ctx context.Context, value string) (*relayclient.NWCClient, error) {
	if extwallet.IsURI(value) {
		return extwallet.Dial(ctx, value)
	}
	tok, err := nipcash.Decode(value)
	if err != nil {
		return nil, fmt.Errorf("not a valid connection string")
	}
	pairing := &nip47.PairingInfo{WalletPubkey: tok.WalletPubkey, RelayURLs: tok.RelayURLs, Secret: tok.Secret}
	return relayclient.NewNWCClient(ctx, pairing, nip47.EncryptionNIP44V2)
}

// connectionKindOf is what a validated `connect add` value is: a
// cash-token-family string used as a wallet, or a plain NWC URI.
func connectionKindOf(value string) string {
	if dial.Sniff(value) == dial.KindCashToken {
		return config.KindCashToken
	}
	return config.KindNWC
}

func safeDestinationLabel(raw string) string {
	switch dial.Sniff(raw) {
	case dial.KindNWCURI:
		if label := extwallet.Label(raw); label != "" {
			return label
		}
		return "a wallet connection"
	case dial.KindCashHub:
		return "a Cash Hub connection"
	case dial.KindCircleHub:
		return "a Circle Hub connection"
	default:
		return "the given connection"
	}
}
