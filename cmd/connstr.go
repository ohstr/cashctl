package cmd

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ohstr/nmilat/nipcash"
	"github.com/ohstr/nmilat/nipcw"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/config"
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
	Circle   *nipcw.CircleHubConnection
	Verified bool
	// Reason says why Verified is false: chain_no_prefix | signer_mismatch.
	Reason string
}

// hubIdentity is TLV 4, x-only hex.
func (c *conn) hubIdentity() string {
	switch {
	case c.CashHub != nil:
		return c.CashHub.HubIdentity
	case c.Circle != nil:
		return c.Circle.HubIdentity
	}
	return ""
}

// chain is TLV 6.
func (c *conn) chain() string {
	switch {
	case c.CashHub != nil:
		return c.CashHub.Chain
	case c.Circle != nil:
		return c.Circle.Chain
	}
	return ""
}

// label is TLV 3, as the string carries it (never confirmed by the signature
// alone; shown as "label (not confirmed)").
func (c *conn) label() (string, bool) {
	switch {
	case c.CashHub != nil:
		return c.CashHub.Label, c.CashHub.HasLabel
	case c.Circle != nil:
		return c.Circle.Label, c.Circle.HasLabel
	}
	return "", false
}

// Reasons on a refused connection string (--json "reason").
const (
	reasonConnectionInvalid    = "connection_invalid"
	reasonMixedCase            = "mixed_case"
	reasonConnectionMismatch   = "connection_mismatch"
	reasonConnectionUnverified = "connection_unverified"
)

// readConnection decodes raw as one of want's HRPs (the cash connection strings:
// "cashhub" today). It refuses a mixed-case string, any other HRP and the
// nostr+walletconnect URI form (invalid_input, nothing dialled); an
// all-uppercase string, as a QR code carries it, reads as its lowercase form.
func readConnection(cmd *cobra.Command, raw string, want ...string) (*conn, error) {
	s := strings.TrimSpace(raw)
	invalid := func(input string, err error) error {
		return output.WithReason(output.InvalidInputError(cmd, input, err), reasonConnectionInvalid)
	}
	if strings.HasPrefix(strings.ToLower(s), "nostr+walletconnect:") {
		return nil, invalid(s, fmt.Errorf(
			"a nostr+walletconnect:// URI is not a %s connection; ask for its %s1... string", strings.Join(want, "/"), want[0]))
	}
	if s != strings.ToLower(s) && s != strings.ToUpper(s) {
		return nil, output.WithReason(output.InvalidInputError(cmd, s, fmt.Errorf("this string mixes upper and lower case, which a valid one never does: %w", nipcash.ErrMixedCase)), reasonMixedCase)
	}
	s = strings.ToLower(s)
	hrp, _, ok := strings.Cut(s, "1")
	if !ok || !slices.Contains(want, hrp) {
		return nil, invalid(s, fmt.Errorf("not a %s connection string (expected %s1...)", strings.Join(want, "/"), want[0]))
	}
	c := &conn{Raw: s, HRP: hrp}
	var verifyErr error
	switch hrp {
	case "cashhub":
		decoded, err := nipcash.DecodeCashHubConnection(s)
		if err != nil {
			return nil, invalid(s, fmt.Errorf("not a valid cashhub1 connection: %w", err))
		}
		c.CashHub = &decoded
		verifyErr = decoded.Verify()
	case nipcw.CircleHubConnectionHRP:
		decoded, err := nipcw.DecodeCircleHubConnection(s)
		if err != nil {
			return nil, invalid(s, fmt.Errorf("not a valid circlehub1 connection: %w", err))
		}
		c.Circle = &decoded
		verifyErr = decoded.Verify()
	default:
		return nil, invalid(s, fmt.Errorf("cashctl can't read %s1 connections yet", hrp))
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

// warnUnverified tells the user, on stderr, that a string's Hub is not
// confirmed, and returns the --json warnings entry.
func warnUnverified(jsonMode bool, c *conn) string {
	w := "Hub not confirmed: " + unverifiedReasonText(c)
	output.Notef(jsonMode, "Warning: %s.", w)
	return w
}

// confirmUnverified asks once per string before cashctl acts on one whose
// signature did not verify (XQ-37, LS-10). It runs only after the online check
// passed. The answer is kept per string, so a later command over the same
// string only warns. --allow-unverified counts as the answer; --yes and --json
// never do: without the flag a non-interactive caller is refused
// (connection_unverified, usage exit 2) and nothing is written or sent.
// proceed is false when the user declined at the prompt.
func confirmUnverified(cmd *cobra.Command, c *conn) (proceed bool, err error) {
	if c.Verified {
		return true, nil
	}
	confirmed, err := config.UnverifiedConfirmed(c.Raw)
	if err != nil {
		return false, output.RuntimeError(cmd, err)
	}
	if confirmed {
		return true, nil
	}
	record := func(via string) error {
		if err := config.ConfirmUnverified(c.Raw, config.UnverifiedConfirmation{
			HRP: c.HRP, HubIdentity: c.hubIdentity(), Reason: c.Reason, Via: via,
		}); err != nil {
			return output.RuntimeError(cmd, err)
		}
		return nil
	}
	if allow, _ := cmd.Flags().GetBool("allow-unverified"); allow {
		return true, record("flag")
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	yes, _ := cmd.Flags().GetBool("yes")
	if jsonMode || yes {
		return false, output.WithReason(output.UsageError(cmd, fmt.Errorf(
			"this Hub is not confirmed (%s); nothing was sent. Pass --allow-unverified only if you trust this string", unverifiedReasonText(c))),
			reasonConnectionUnverified)
	}
	if !Confirm(cmd, false, fmt.Sprintf("This Hub is not confirmed: %s. The Hub answered as the identity the string names. Continue anyway?", unverifiedReasonText(c))) {
		return false, nil
	}
	return true, record("prompt")
}

// connectionMismatchErr is a failed online check: the string names one Hub and
// the Hub answered as another (or on another chain). Nothing was sent.
func connectionMismatchErr(cmd *cobra.Command, err error) error {
	return output.WithReason(output.AuthError(cmd, fmt.Errorf(
		"this string names one Hub and the Hub answered as another; nothing was sent (%w)", err)), reasonConnectionMismatch)
}

// isConnectionMismatch reports a failed online check.
func isConnectionMismatch(err error) bool {
	return errors.Is(err, nipcash.ErrHubIdentityMismatch) || errors.Is(err, nipcash.ErrChainMismatch)
}

// testNetwork reports whether network is not mainnet (a missing or unknown one
// reads as a test network, XQ-37) and says so on stderr.
func testNetwork(jsonMode bool, network string) bool {
	if network == "mainnet" {
		return false
	}
	if network == "" {
		output.Notef(jsonMode, "network: unknown — treat it as a test network")
	} else {
		output.Notef(jsonMode, "network: %s — a test network, not real money", output.Sanitize(network))
	}
	return true
}
