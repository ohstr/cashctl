package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/ohstr/nmilat/utils"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

func TestArgOrStdin_PassesAnOrdinaryArgThrough(t *testing.T) {
	stubStdin(t, "unread\n")
	got, err := argOrStdin(testCmdWithFlags(true, false), "lokicash1abc")
	if err != nil || got != "lokicash1abc" {
		t.Fatalf("argOrStdin = %q, %v; want the argument itself", got, err)
	}
	if line, _ := stdin.ReadString('\n'); line != "unread\n" {
		t.Errorf("stdin was consumed: next line = %q", line)
	}
}

// The value comes off the first line only, so a confirmation prompt later in
// the same command still reads its own answer.
func TestArgOrStdin_DashReadsOneLineAndLeavesTheRest(t *testing.T) {
	stubStdin(t, "  lokicash1abc#secret \ny\n")
	got, err := argOrStdin(testCmdWithFlags(false, false), "-")
	if err != nil || got != "lokicash1abc#secret" {
		t.Fatalf("argOrStdin = %q, %v; want the trimmed first line", got, err)
	}
	if !Confirm(testCmdWithFlags(false, false), false, "Continue?") {
		t.Error("the prompt after it did not get the next line's y")
	}
}

func TestArgOrStdin_LastLineWithoutNewline(t *testing.T) {
	stubStdin(t, "nostr+walletconnect://x")
	if got, err := argOrStdin(testCmdWithFlags(true, false), "-"); err != nil || got != "nostr+walletconnect://x" {
		t.Fatalf("argOrStdin = %q, %v", got, err)
	}
}

func TestArgOrStdin_EmptyStdinIsAUsageError(t *testing.T) {
	stubStdin(t, "")
	_, err := argOrStdin(testCmdWithFlags(true, false), "-")
	var ce *output.CLIError
	if !errors.As(err, &ce) || ce.Code != output.CodeUsage {
		t.Fatalf("err = %v, want a usage error", err)
	}
}

// A pubkey:<privkey> --as signs the run's requests, so the Hub's reply is
// matched against its pubkey, not the stored identity's.
func TestSignerPubKeyHex_FollowsAPubkeyAsCredential(t *testing.T) {
	priv := strings.Repeat("11", 32)
	want, err := utils.GetPublicKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	c := testCmdWithFlags(true, false)
	c.Flags().String("as", "pubkey:"+priv, "")
	if got, err := signerPubKeyHex(c, nil); err != nil || got != want {
		t.Fatalf("signerPubKeyHex = %q, %v; want %q", got, err, want)
	}
}

// A cash note moved onto a merge's throwaway key is spent by that key, so
// the Hub's reply about it is matched against the throwaway key's pubkey.
func TestSignerPubKeyHex_FollowsAThrowawayKey(t *testing.T) {
	throwaway := strings.Repeat("22", 32)
	want, err := utils.GetPublicKey(throwaway)
	if err != nil {
		t.Fatal(err)
	}
	c := testCmdWithFlags(true, false)
	c.Flags().String("as", "", "")
	if got, err := signerPubKeyHex(c, &ledger.Entry{ThrowawayKey: throwaway}); err != nil || got != want {
		t.Fatalf("signerPubKeyHex = %q, %v; want the throwaway key's %q", got, err, want)
	}
}

func TestCredentialPubKeyHex_PubkeyCredential(t *testing.T) {
	priv := strings.Repeat("33", 32)
	want, _ := utils.GetPublicKey(priv)
	if got, err := credentialPubKeyHex(testCmdWithFlags(true, false), "pubkey:"+priv); err != nil || got != want {
		t.Fatalf("credentialPubKeyHex = %q, %v; want %q", got, err, want)
	}
}
