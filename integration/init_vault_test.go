//go:build integration

package integration

import (
	"strings"
	"testing"

	ncli "github.com/ohstr/ncli/client"
)

// seedNcliVault puts a real ncli vault holding one entry under f's private
// XDG_CONFIG_HOME (where the compiled binary looks for it) and returns that
// entry's npub. Uses t.Setenv, so callers must not be t.Parallel.
func seedNcliVault(t *testing.T, f *fixture, label string) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", f.xdgHome)
	_, vaultPriv, err := ncli.CreateVaultIdentity("vault-password-for-test")
	if err != nil {
		t.Fatalf("creating a test ncli vault: %v", err)
	}
	id, err := ncli.GenerateIdentity()
	if err != nil {
		t.Fatalf("generating a test identity: %v", err)
	}
	entry, err := ncli.AddVaultEntry(vaultPriv, label, id.PrivKeyHex)
	if err != nil {
		t.Fatalf("adding a test vault entry: %v", err)
	}
	return entry.Npub
}

// README: "under --json, init generates a fresh local identity". It used to
// adopt the first ncli vault entry with no prompt at all (`use := jsonMode ||
// Confirm(...)`) — binding the user's personal Nostr key to a money tool for
// exactly the caller (a script or agent) least able to notice or decline.
func TestInit_JSON_DoesNotAdoptNcliVaultIdentity(t *testing.T) {
	f := newFixture(t)
	vaultNpub := seedNcliVault(t, f, "personal")

	out := f.mustJSON("init")
	if got := out["npub"]; got == vaultNpub {
		t.Errorf("init --json adopted the ncli vault identity %v — must generate its own", got)
	}
	if src, _ := out["identity_source"].(string); src != "cashctl-local" {
		t.Errorf("init --json identity_source = %q, want cashctl-local", src)
	}
}

// The documented interactive behavior stays: a vault identity is OFFERED, and
// accepting it adopts it.
func TestInit_Interactive_AcceptingAdoptsNcliVaultIdentity(t *testing.T) {
	f := newFixture(t)
	vaultNpub := seedNcliVault(t, f, "personal")

	res := f.runInteractive("y\n", "init")
	if res.ExitCode != 0 {
		t.Fatalf("init: exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	// The question is narration (stderr); the identity it settled on is the
	// command's result (stdout).
	if !strings.Contains(res.Stderr, `Use existing identity "personal"?`) {
		t.Errorf("init never offered the vault identity:\n%s", res.Stderr)
	}
	if strings.Contains(res.Stdout, "Use existing identity") {
		t.Errorf("the prompt leaked onto stdout:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, vaultNpub) {
		t.Errorf("init reported a different identity than the vault entry %s:\n%s", vaultNpub, res.Stdout)
	}
}

func TestInit_Interactive_DecliningGeneratesLocalIdentity(t *testing.T) {
	f := newFixture(t)
	vaultNpub := seedNcliVault(t, f, "personal")

	res := f.runInteractive("n\n", "init")
	if res.ExitCode != 0 {
		t.Fatalf("init: exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if strings.Contains(res.Stdout, vaultNpub) {
		t.Errorf("declined the vault identity but init used it anyway:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "cashctl-local") {
		t.Errorf("declining should fall back to a local identity:\n%s", res.Stdout)
	}
}
