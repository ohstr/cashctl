package cmd

import (
	"bufio"
	"strings"
	"testing"

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
