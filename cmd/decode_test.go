package cmd

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	btcec "github.com/flokiorg/go-flokicoin/crypto"
	"github.com/ohstr/nmilat/nipcash"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/identity"
)

// newTestDecodeCmd builds a *cobra.Command carrying the same "json"/"yes"/
// "check" flags RootCmd and newDecodeCmd register on the real command tree
// — shouldRunCheck/shouldCheckCashToken/Confirm all read these directly off
// cmd, so a bare &cobra.Command{} without them can't exercise the real
// lookup path.
func newTestDecodeCmd() *cobra.Command {
	c := &cobra.Command{}
	c.Flags().Bool("json", false, "")
	c.Flags().Bool("yes", false, "")
	c.Flags().Bool("check", false, "")
	return c
}

// withStdin temporarily points the package's shared stdin reader at r, for
// exercising Confirm's prompt path deterministically.
func withStdin(t *testing.T, input string) {
	t.Helper()
	real := stdin
	stdin = bufio.NewReader(strings.NewReader(input))
	t.Cleanup(func() { stdin = real })
}

func TestShouldRunCheck_JSONModeIsFlagOnlyNeverPrompts(t *testing.T) {
	c := newTestDecodeCmd()
	// No stdin queued at all — if this reached Confirm's prompt it would
	// block/fail reading, proving jsonMode short-circuits before that.
	if got := shouldRunCheck(c, true, false, "check?"); got {
		t.Errorf("shouldRunCheck(jsonMode=true, checkFlag=false) = true, want false")
	}
	if got := shouldRunCheck(c, true, true, "check?"); !got {
		t.Errorf("shouldRunCheck(jsonMode=true, checkFlag=true) = false, want true")
	}
}

func TestShouldRunCheck_ExplicitFlagInTextModeSkipsThePrompt(t *testing.T) {
	c := newTestDecodeCmd()
	if err := c.Flags().Set("check", "true"); err != nil {
		t.Fatal(err)
	}
	// No stdin queued — an explicit --check must be honored without asking.
	if got := shouldRunCheck(c, false, true, "check?"); !got {
		t.Errorf("shouldRunCheck with explicit --check=true = false, want true")
	}
}

func TestShouldRunCheck_TextModeWithoutFlagPromptsAndHonorsDefault(t *testing.T) {
	c := newTestDecodeCmd()
	withStdin(t, "\n") // bare Enter
	if got := shouldRunCheck(c, false, false, "check?"); !got {
		t.Errorf("shouldRunCheck with bare Enter = false, want true (defaultYes)")
	}
}

func TestShouldRunCheck_TextModeWithoutFlagHonorsNo(t *testing.T) {
	c := newTestDecodeCmd()
	withStdin(t, "n\n")
	if got := shouldRunCheck(c, false, false, "check?"); got {
		t.Errorf("shouldRunCheck with 'n' = true, want false")
	}
}

func TestShouldCheckCashToken_BearerTokenAlwaysPromptsRegardlessOfIdentity(t *testing.T) {
	tmp := t.TempDir()
	appdir.SetOverride(tmp)
	t.Cleanup(func() { appdir.SetOverride("") })

	c := newTestDecodeCmd()
	withStdin(t, "\n")
	if got := shouldCheckCashToken(c, false, false, true); !got {
		t.Errorf("shouldCheckCashToken(bearer token, no identity, Enter) = false, want true")
	}
}

func TestShouldCheckCashToken_IdentityRequiredNoLocalIdentitySkipsPromptEntirely(t *testing.T) {
	tmp := t.TempDir()
	appdir.SetOverride(tmp)
	t.Cleanup(func() { appdir.SetOverride("") })

	c := newTestDecodeCmd()
	// No stdin queued: if this fell through to Confirm's prompt, reading
	// from an empty reader would return "" (EOF), which Confirm treats as
	// defaultYes=true — so to prove the skip actually happened (not a
	// lucky default), assert false, which only the skip path can produce.
	if got := shouldCheckCashToken(c, false, false, false); got {
		t.Errorf("shouldCheckCashToken(identity-required, no local identity) = true, want false (skipped)")
	}
}

func TestShouldCheckCashToken_IdentityRequiredWithLocalIdentityPrompts(t *testing.T) {
	tmp := t.TempDir()
	appdir.SetOverride(tmp)
	t.Cleanup(func() { appdir.SetOverride("") })
	if err := identity.SaveNcliVaultRef("npub1test", "test"); err != nil {
		t.Fatal(err)
	}

	c := newTestDecodeCmd()
	withStdin(t, "\n")
	if got := shouldCheckCashToken(c, false, false, false); !got {
		t.Errorf("shouldCheckCashToken(identity-required, local identity configured, Enter) = false, want true")
	}
}

func TestShouldCheckCashToken_ExplicitFlagSkipsTheIdentityGuardEntirely(t *testing.T) {
	tmp := t.TempDir()
	appdir.SetOverride(tmp)
	t.Cleanup(func() { appdir.SetOverride("") })

	c := newTestDecodeCmd()
	if err := c.Flags().Set("check", "true"); err != nil {
		t.Fatal(err)
	}
	// No local identity configured, and no stdin queued — an explicit
	// --check must still run rather than being silently skipped by the
	// identity guard, which only applies to the interactive-prompt path.
	if got := shouldCheckCashToken(c, false, true, false); !got {
		t.Errorf("shouldCheckCashToken with explicit --check=true = false, want true")
	}
}

// --- formatMinterStatus: the shared, VERIFYING (not just presence-check)
// minter-status helper both `receive` and `decode` render — the
// regression test for the accuracy bug it fixes (a forged/corrupt
// signature must never be reported as trustworthy just because the
// mint-provenance fields are present) and for the friendlier "unknown"
// wording replacing the old "none (unsigned token)".

// signMintProvenance mirrors nipcash's own unexported signProvenance test
// helper (nipcash/provenance_test.go) — reproduces its exact double-SHA256
// LN-signed-message digest convention, since doubleSHA256 itself isn't
// exported for this package to call directly.
func signMintProvenance(t *testing.T, priv *btcec.PrivateKey, hrp, walletPubkeyHex string, amountMillis uint64) []byte {
	t.Helper()
	payload := nipcash.MintPayload(hrp, walletPubkeyHex, amountMillis)
	first := sha256.Sum256([]byte(nipcash.LNSignedMessagePrefix + payload))
	second := sha256.Sum256(first[:])
	return ecdsa.SignCompact(priv, second[:], true)
}

func TestFormatMinterStatus_NoProvenanceIsUnknown(t *testing.T) {
	tok := nipcash.Token{HRP: "lokicash", WalletPubkey: strings.Repeat("a1", 32)}
	minterLine, amountLine := formatMinterStatus(tok)
	if minterLine != "minter: unknown" {
		t.Errorf("minterLine = %q, want %q", minterLine, "minter: unknown")
	}
	if amountLine != "" {
		t.Errorf("amountLine = %q, want empty (nothing to print for a token with no provenance)", amountLine)
	}
}

func TestFormatMinterStatus_InvalidSignatureNeverReportedTrustworthy(t *testing.T) {
	amount := uint64(5000)
	// Garbage bytes of the wrong length: VerifyProvenance fails outright
	// (never recovers a pubkey at all) rather than recovering a wrong one
	// — either way formatMinterStatus must not call this trustworthy.
	tok := nipcash.Token{HRP: "lokicash", WalletPubkey: strings.Repeat("a1", 32), MintSignature: []byte{1, 2, 3}, AttestedAmountMillis: &amount}
	minterLine, amountLine := formatMinterStatus(tok)
	if minterLine != "minter: INVALID SIGNATURE — don't trust attested_amount" {
		t.Errorf("minterLine = %q, want the INVALID SIGNATURE warning", minterLine)
	}
	if !strings.Contains(amountLine, "unverified") {
		t.Errorf("amountLine = %q, want it to say (unverified)", amountLine)
	}
}

func TestFormatMinterStatus_ValidSignatureReportsMinter(t *testing.T) {
	priv, pub := btcec.PrivKeyFromBytes([]byte{
		1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
		17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32,
	})
	walletPubkey := strings.Repeat("b2", 32)
	amount := uint64(40000)
	sig := signMintProvenance(t, priv, "lokicash", walletPubkey, amount)
	tok := nipcash.Token{HRP: "lokicash", WalletPubkey: walletPubkey, MintSignature: sig, AttestedAmountMillis: &amount}

	minterLine, amountLine := formatMinterStatus(tok)
	serialized := btcec.ToSerialized(pub)
	wantMinter := "minter: " + hex.EncodeToString(serialized[:])
	if minterLine != wantMinter {
		t.Errorf("minterLine = %q, want %q", minterLine, wantMinter)
	}
	if strings.Contains(amountLine, "unverified") {
		t.Errorf("amountLine = %q, want no (unverified) qualifier for a genuinely valid signature", amountLine)
	}
}
