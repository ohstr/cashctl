package cmd

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ohstr/nmilat/nipcash"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/output"
)

// Signed connection strings (NIP-CASH §Connection Signatures): a cashhub1
// carries the Hub's identity (TLV 4), a node signature over the string (TLV 5)
// and the chain that signature follows (TLV 6). readConnection is the one place
// cashctl reads one. An unverified string still decodes; it is never shown as
// confirmed, and a caller that acts on it asks the user first.

// conn is a decoded, signature-checked connection string. The secret stays in
// Raw and in the decoded struct; nothing here prints it.
type conn struct {
	Raw      string // lowercased
	HRP      string
	CashHub  *nipcash.CashHubConnection
	Verified bool
	// Reason says why Verified is false: chain_no_prefix | signer_mismatch.
	Reason string
}

// hubIdentity is TLV 4, x-only hex.
func (c *conn) hubIdentity() string {
	if c.CashHub != nil {
		return c.CashHub.HubIdentity
	}
	return ""
}

// chain is TLV 6.
func (c *conn) chain() string {
	if c.CashHub != nil {
		return c.CashHub.Chain
	}
	return ""
}

// readConnection decodes raw as one of want's HRPs (the cash connection strings:
// "cashhub" today). It refuses a mixed-case string, any other HRP and the
// nostr+walletconnect URI form (invalid_input, nothing dialled); an
// all-uppercase string, as a QR code carries it, reads as its lowercase form.
func readConnection(cmd *cobra.Command, raw string, want ...string) (*conn, error) {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(strings.ToLower(s), "nostr+walletconnect:") {
		return nil, output.InvalidInputError(cmd, s, fmt.Errorf(
			"a nostr+walletconnect:// URI is not a %s connection; ask for its %s1... string", strings.Join(want, "/"), want[0]))
	}
	if s != strings.ToLower(s) && s != strings.ToUpper(s) {
		return nil, output.InvalidInputError(cmd, s, fmt.Errorf("this string mixes upper and lower case, which a valid one never does: %w", nipcash.ErrMixedCase))
	}
	s = strings.ToLower(s)
	hrp, _, ok := strings.Cut(s, "1")
	if !ok || !slices.Contains(want, hrp) {
		return nil, output.InvalidInputError(cmd, s, fmt.Errorf("not a %s connection string (expected %s1...)", strings.Join(want, "/"), want[0]))
	}
	c := &conn{Raw: s, HRP: hrp}
	var verifyErr error
	switch hrp {
	case "cashhub":
		decoded, err := nipcash.DecodeCashHubConnection(s)
		if err != nil {
			return nil, output.InvalidInputError(cmd, s, fmt.Errorf("not a valid cashhub1 connection: %w", err))
		}
		c.CashHub = &decoded
		verifyErr = decoded.Verify()
	default:
		return nil, output.InvalidInputError(cmd, s, fmt.Errorf("cashctl can't read %s1 connections yet", hrp))
	}
	switch {
	case verifyErr == nil:
		c.Verified = true
	case errors.Is(verifyErr, nipcash.ErrChainNoPrefix):
		c.Reason = "chain_no_prefix"
	default:
		c.Reason = "signer_mismatch"
	}
	return c, nil
}

// unverifiedReasonText is what a person is told about an unverified string.
func unverifiedReasonText(c *conn) string {
	switch c.Reason {
	case "chain_no_prefix":
		return fmt.Sprintf("cashctl doesn't know how %s signs, so it can't confirm who issued this", c.chain())
	default:
		return "this string's signature doesn't match the Hub it names"
	}
}

// signatureLabel is "verified" or "unverified".
func signatureLabel(c *conn) string {
	if c.Verified {
		return "verified"
	}
	return "unverified"
}

// shortFingerprint is the first 16 hex characters of a key, enough to tell Hubs
// apart on a screen.
func shortFingerprint(hex string) string {
	if len(hex) > 16 {
		return hex[:16]
	}
	return hex
}
