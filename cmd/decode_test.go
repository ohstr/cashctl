package cmd

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	btcec "github.com/flokiorg/go-flokicoin/crypto"
	"github.com/ohstr/nmilat/nip19"
	"github.com/ohstr/nmilat/nipIC"
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

// TestShouldRunCheck_TextModeWithoutFlagDefaultsToNo is the regression
// guard for the bug found auditing decode: a bare Enter (and a closed/
// empty stdin — a script piping decode with neither --check nor
// --yes/--json — hits the exact same defaultYes branch) used to default to
// YES, silently going online despite decode's own documented "no network
// call by default" contract (a token's embedded relay URL fully controls
// where cashctl connects — see docs/private's own security notes on
// relay-based fingerprinting). Both bare Enter and true EOF must now
// decline.
func TestShouldRunCheck_TextModeWithoutFlagDefaultsToNo(t *testing.T) {
	c := newTestDecodeCmd()
	withStdin(t, "\n") // bare Enter
	if got := shouldRunCheck(c, false, false, "check?"); got {
		t.Errorf("shouldRunCheck with bare Enter = true, want false (safe default: no network by default)")
	}
}

func TestShouldRunCheck_EOFDefaultsToNo(t *testing.T) {
	c := newTestDecodeCmd()
	withStdin(t, "") // true EOF, not even a newline — the piped-script case
	if got := shouldRunCheck(c, false, false, "check?"); got {
		t.Errorf("shouldRunCheck on EOF = true, want false (safe default: no network by default)")
	}
}

func TestShouldRunCheck_TextModeWithoutFlagHonorsNo(t *testing.T) {
	c := newTestDecodeCmd()
	withStdin(t, "n\n")
	if got := shouldRunCheck(c, false, false, "check?"); got {
		t.Errorf("shouldRunCheck with 'n' = true, want false")
	}
}

// TestShouldRunCheck_TextModeWithoutFlagHonorsExplicitYes proves the
// prompt still genuinely offers the option — only its unattended default
// changed, not its ability to opt in.
func TestShouldRunCheck_TextModeWithoutFlagHonorsExplicitYes(t *testing.T) {
	c := newTestDecodeCmd()
	withStdin(t, "y\n")
	if got := shouldRunCheck(c, false, false, "check?"); !got {
		t.Errorf("shouldRunCheck with 'y' = false, want true")
	}
}

func TestShouldCheckCashToken_CashTokenAlwaysPromptsRegardlessOfIdentity(t *testing.T) {
	tmp := t.TempDir()
	appdir.SetOverride(tmp)
	t.Cleanup(func() { appdir.SetOverride("") })

	c := newTestDecodeCmd()
	// Explicit "y", not a bare Enter: now that the prompt's own default is
	// no, a bare Enter can't distinguish "it prompted and defaulted no"
	// from "it skipped the prompt entirely" — both read false. Queuing "y"
	// only comes back true if shouldRunCheck's Confirm() actually ran.
	withStdin(t, "y\n")
	if got := shouldCheckCashToken(c, false, false, true); !got {
		t.Errorf("shouldCheckCashToken(cash-mode token, no identity, explicit y) = false, want true (it must actually prompt, not skip)")
	}
}

func TestShouldCheckCashToken_IdentityRequiredNoLocalIdentitySkipsPromptEntirely(t *testing.T) {
	tmp := t.TempDir()
	appdir.SetOverride(tmp)
	t.Cleanup(func() { appdir.SetOverride("") })

	c := newTestDecodeCmd()
	// "y" queued but must still come back false: if the identity guard
	// failed to skip, Confirm() would read this "y" and return true — so
	// false here is only possible if the skip genuinely fired before ever
	// touching stdin.
	withStdin(t, "y\n")
	if got := shouldCheckCashToken(c, false, false, false); got {
		t.Errorf("shouldCheckCashToken(identity-required, no local identity, queued 'y') = true, want false (skipped, never reached the prompt)")
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
	withStdin(t, "y\n") // see BearerTokenAlwaysPrompts... for why not a bare Enter
	if got := shouldCheckCashToken(c, false, false, false); !got {
		t.Errorf("shouldCheckCashToken(identity-required, local identity configured, explicit y) = false, want true (it must actually prompt, not skip)")
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

// TestDecode_NostrEntityMisPaste_FriendlyError guards against the bug
// found auditing decode: an npub/nsec/nconnection paste used to fall
// through to nipcash.Decode (which accepts any HRP — see its own doc
// comment) and fail with a cryptic "nipcash: truncated TLV entry at
// offset N" that named neither what was pasted nor why it was wrong.
// nsec gets its own check that the raw key never appears in the error at
// all (RedactSecretInput's secretLikePattern already blanked the `input`
// field before this fix; this pins the message text itself never embeds
// it either).
func TestDecode_NostrEntityMisPaste_FriendlyError(t *testing.T) {
	npub, err := nip19.EncodePublicKey(strings.Repeat("aa", 32))
	if err != nil {
		t.Fatalf("nip19.EncodePublicKey: %v", err)
	}
	nsec, err := nip19.EncodePrivateKey(strings.Repeat("bb", 32))
	if err != nil {
		t.Fatalf("nip19.EncodePrivateKey: %v", err)
	}
	nconn, err := nipIC.EncodeNConnection(
		nipIC.ConnectionKey(strings.Repeat("cc", 32)), []string{"wss://relay.example"}, "discord")
	if err != nil {
		t.Fatalf("nipIC.EncodeNConnection: %v", err)
	}

	cases := []struct {
		name    string
		value   string
		wantSub string
	}{
		{"npub", npub, "public key"},
		{"nsec", nsec, "PRIVATE KEY"},
		{"nconnection", nconn, "nconnection"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := newDecodeCmd()
			err := cmd.RunE(cmd, []string{c.value})
			if err == nil {
				t.Fatalf("decode(%s) = nil error, want rejected", c.name)
			}
			if strings.Contains(err.Error(), "nipcash") {
				t.Errorf("decode(%s) error = %q, still leaks the cryptic nipcash decode error", c.name, err)
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("decode(%s) error = %q, want it to mention %q", c.name, err, c.wantSub)
			}
			if c.name == "nsec" && strings.Contains(err.Error(), nsec) {
				t.Errorf("decode(nsec) error = %q, echoes the raw private key", err)
			}
		})
	}
}

// TestRunCashReceive_NostrEntityMisPaste_FriendlyError is receive's own
// counterpart — same underlying dial.Sniff/nostrEntityMessage fix, wired
// in at a second call site.
func TestRunCashReceive_NostrEntityMisPaste_FriendlyError(t *testing.T) {
	npub, err := nip19.EncodePublicKey(strings.Repeat("aa", 32))
	if err != nil {
		t.Fatalf("nip19.EncodePublicKey: %v", err)
	}

	cmd := &cobra.Command{}
	cmd.Flags().Bool("json", false, "")
	cmd.Flags().String("connection", "", "")

	err = runCashReceive(cmd, []string{npub})
	if err == nil {
		t.Fatal("runCashReceive(npub) = nil error, want rejected")
	}
	if strings.Contains(err.Error(), "nipcash") {
		t.Errorf("runCashReceive(npub) error = %q, still leaks the cryptic nipcash decode error", err)
	}
	if !strings.Contains(err.Error(), "public key") {
		t.Errorf("runCashReceive(npub) error = %q, want it to mention \"public key\"", err)
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
	if !strings.HasPrefix(minterLine, wantMinter) {
		t.Errorf("minterLine = %q, want it to start with %q", minterLine, wantMinter)
	}
	// The bug found auditing this: a "valid" mint signature only proves
	// self-consistency (VerifyProvenance is signature RECOVERY, not
	// verification against a known key) — anyone can mint-sign their own
	// token with a throwaway key and get this exact same "valid" result
	// and a made-up-but-real minter pubkey. The line must say so, not
	// print a bare pubkey that reads as cashctl vouching for it.
	if !strings.Contains(minterLine, "valid signature") || !strings.Contains(minterLine, "trusted mint") {
		t.Errorf("minterLine = %q, want it to caveat that a valid signature isn't a trusted mint", minterLine)
	}
	if strings.Contains(amountLine, "unverified") {
		t.Errorf("amountLine = %q, want no (unverified) qualifier for a genuinely valid signature", amountLine)
	}
}
