package cmd

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/output"
)

func circleBackup(t *testing.T, secret string) []byte {
	t.Helper()
	b, err := config.CircleWallet{WalletPubkey: strings.Repeat("ef", 32), Relays: []string{"wss://w.example"}, Secret: secret,
		HubPubkey: strings.Repeat("ab", 32), HubIdentity: strings.Repeat("cd", 32), Chain: "flokicoin"}.Backup()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func runConnectCmd(t *testing.T, c *cobra.Command, args []string) (string, error) {
	t.Helper()
	c.Flags().Bool("json", true, "")
	c.Flags().Bool("yes", false, "")
	c.SetArgs(args)
	var err error
	stdout, _ := captureStreams(t, func() { err = c.Execute() })
	return stdout, err
}

// WP-11: connect add reads a circle wallet's backup file (a path or stdin),
// stores it as kind circle and shows only its wallet_pubkey.
func TestConnectAdd_CircleWalletBackup(t *testing.T) {
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	secret := strings.Repeat("12", 32)
	path := filepath.Join(t.TempDir(), "wallet.json")
	if err := os.WriteFile(path, circleBackup(t, secret), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runConnectCmd(t, newConnectAddCmd(), []string{"circle:family", path})
	if err != nil {
		t.Fatalf("connect add: %v", err)
	}
	if !strings.Contains(out, `"kind": "circle"`) && !strings.Contains(out, `"kind":"circle"`) {
		t.Errorf("stdout %s", out)
	}
	s, _ := config.Load()
	c, ok := s.Find("circle:family")
	if !ok || c.Kind != config.KindCircle || c.WalletPubkey != strings.Repeat("ef", 32) {
		t.Fatalf("stored %+v", c)
	}
	if w, ok, err := config.ParseCircleWallet(c.Value); !ok || err != nil || w.Secret != secret {
		t.Errorf("stored value does not read back: %v", err)
	}
	list, _ := runConnectCmd(t, newConnectListCmd(), nil)
	if strings.Contains(list, secret) || !strings.Contains(list, strings.Repeat("ef", 32)) {
		t.Errorf("connect list: %s", list)
	}

	// From stdin, several lines.
	prev := stdin
	stdin = bufio.NewReader(strings.NewReader(string(circleBackup(t, strings.Repeat("34", 32)))))
	t.Cleanup(func() { stdin = prev })
	if _, err := runConnectCmd(t, newConnectAddCmd(), []string{"circle:two", "-"}); err != nil {
		t.Fatalf("from stdin: %v", err)
	}

	// A malformed backup is refused, its secret not echoed.
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, circleBackup(t, "nope"), 0o600)
	_, err = runConnectCmd(t, newConnectAddCmd(), []string{"circle:bad", bad})
	if output.ExitCode(err) != 3 {
		t.Errorf("malformed backup: %v", err)
	}
}
