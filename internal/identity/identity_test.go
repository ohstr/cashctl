package identity

import (
	"errors"
	"os"
	"testing"

	"github.com/ohstr/ncli/client/vault"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/store"
)

// withTempDirs isolates both cashctl's own appdir (cashctl.db) and ncli's
// vault (client.VaultPath/PrefsPath) under one fresh temp dir per test,
// mirroring ncli's own test isolation technique (withTempConfigDir in
// ncli's client package) — same XDG_CONFIG_HOME override reaches both,
// since cashctl's appdir.Dir and ncli's AppConfigDir both resolve through
// os.UserConfigDir.
func withTempDirs(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	appdir.SetOverride("")
	t.Cleanup(func() { appdir.SetOverride("") })
}

func TestExists_FalseBeforeInit(t *testing.T) {
	withTempDirs(t)

	exists, err := Exists()
	if err != nil {
		t.Fatalf("Exists() error = %v", err)
	}
	if exists {
		t.Error("Exists() on a fresh install = true, want false")
	}
}

func TestLoad_ReturnsErrNotConfigured(t *testing.T) {
	withTempDirs(t)

	_, err := Load()
	if !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Load() error = %v, want ErrNotConfigured", err)
	}
}

func TestGenerateAndSaveLocal_RoundTrip(t *testing.T) {
	withTempDirs(t)

	npub, err := GenerateAndSaveLocal()
	if err != nil {
		t.Fatalf("GenerateAndSaveLocal() error = %v", err)
	}
	if npub == "" {
		t.Fatal("GenerateAndSaveLocal() returned empty npub")
	}

	exists, err := Exists()
	if err != nil || !exists {
		t.Fatalf("Exists() = %v, %v; want true, nil", exists, err)
	}

	s, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if s.Source != SourceLocal {
		t.Errorf("Source = %q, want %q", s.Source, SourceLocal)
	}
	if s.PrivHex == "" {
		t.Error("PrivHex is empty")
	}

	priv, err := Resolve(func() (string, error) { t.Fatal("promptPassword must not be called for SourceLocal"); return "", nil })
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if priv != s.PrivHex {
		t.Errorf("Resolve() = %q, want %q", priv, s.PrivHex)
	}
}

func TestGenerateAndSaveLocal_FilePermissions(t *testing.T) {
	withTempDirs(t)

	if _, err := GenerateAndSaveLocal(); err != nil {
		t.Fatalf("GenerateAndSaveLocal() error = %v", err)
	}
	p, err := store.Path()
	if err != nil {
		t.Fatalf("store.Path() error = %v", err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("cashctl.db permissions = %o, want 0600", perm)
	}
}

func TestNcliVaultRef_RoundTripAndResolve(t *testing.T) {
	withTempDirs(t)

	// Set up a real ncli vault with one saved entry, exactly as `ncli id
	// --save` would leave behind.
	const password = "hunter2"
	_, vaultPriv, err := vault.CreateIdentity(password)
	if err != nil {
		t.Fatalf("CreateVaultIdentity() error = %v", err)
	}
	id, err := vault.GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity() error = %v", err)
	}
	entry, err := vault.AddEntry(vaultPriv, "main", id.PrivKeyHex)
	if err != nil {
		t.Fatalf("AddVaultEntry() error = %v", err)
	}

	if err := SaveNcliVaultRef(entry.Npub, entry.Label); err != nil {
		t.Fatalf("SaveNcliVaultRef() error = %v", err)
	}

	s, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if s.Source != SourceNcliVault || s.Npub != entry.Npub || s.Label != "main" {
		t.Fatalf("Load() = %+v, want ncli-vault ref to %q/%q", s, entry.Npub, "main")
	}
	// The whole point: an ncli-vault reference never holds a copy of the privkey.
	if s.PrivHex != "" {
		t.Error("stored identity holds a copied privkey for an ncli-vault reference — it must not")
	}

	promptCalled := false
	resolved, err := Resolve(func() (string, error) {
		promptCalled = true
		return password, nil
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if !promptCalled {
		t.Error("Resolve() for SourceNcliVault did not call promptPassword")
	}
	if resolved != id.PrivKeyHex {
		t.Errorf("Resolve() = %q, want the vault entry's real privkey %q", resolved, id.PrivKeyHex)
	}
}

func TestNcliVaultRef_EntryRemovedFromVault(t *testing.T) {
	withTempDirs(t)

	// Reference an entry that was never actually saved to the vault (e.g.
	// removed since, or a stale/corrupt reference).
	if err := SaveNcliVaultRef("npub1doesnotexist", "gone"); err != nil {
		t.Fatalf("SaveNcliVaultRef() error = %v", err)
	}

	_, err := Resolve(func() (string, error) { return "unused", nil })
	if err == nil {
		t.Fatal("expected an error resolving a vault reference that no longer exists")
	}
}

func TestNcliVaultRef_WrongPassword(t *testing.T) {
	withTempDirs(t)

	_, vaultPriv, err := vault.CreateIdentity("correct-password")
	if err != nil {
		t.Fatalf("CreateVaultIdentity() error = %v", err)
	}
	id, err := vault.GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity() error = %v", err)
	}
	entry, err := vault.AddEntry(vaultPriv, "main", id.PrivKeyHex)
	if err != nil {
		t.Fatalf("AddVaultEntry() error = %v", err)
	}
	if err := SaveNcliVaultRef(entry.Npub, entry.Label); err != nil {
		t.Fatalf("SaveNcliVaultRef() error = %v", err)
	}

	_, err = Resolve(func() (string, error) { return "wrong-password", nil })
	if err == nil {
		t.Fatal("expected an error resolving with the wrong vault password")
	}
}

// N first-time `init`s racing on one fresh dir used to each generate a key,
// each "succeed", and each report its own npub, with only the last write
// surviving — every earlier caller was told about an identity that no longer
// existed. The first claim must win and every later one must be told so.
func TestGenerateAndSaveLocal_SecondClaimLosesAndLeavesTheFirstUntouched(t *testing.T) {
	withTempDirs(t)

	if _, err := GenerateAndSaveLocal(); err != nil {
		t.Fatalf("first GenerateAndSaveLocal() error = %v", err)
	}
	first, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if _, err := GenerateAndSaveLocal(); !errors.Is(err, ErrAlreadyConfigured) {
		t.Errorf("second GenerateAndSaveLocal() error = %v, want ErrAlreadyConfigured", err)
	}
	if err := SaveNcliVaultRef("npub1other", "other"); !errors.Is(err, ErrAlreadyConfigured) {
		t.Errorf("SaveNcliVaultRef() over an existing identity: error = %v, want ErrAlreadyConfigured", err)
	}

	after, err := Load()
	if err != nil {
		t.Fatalf("Load() after the losing claims error = %v", err)
	}
	if after.PrivHex != first.PrivHex || after.Source != first.Source {
		t.Error("a losing claim overwrote the identity the first one stored")
	}
}

func TestGenerateAndSaveLocal_ConcurrentClaimsLeaveExactlyOneWinner(t *testing.T) {
	withTempDirs(t)

	const n = 8
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := GenerateAndSaveLocal()
			results <- err
		}()
	}
	wins := 0
	for i := 0; i < n; i++ {
		switch err := <-results; {
		case err == nil:
			wins++
		case errors.Is(err, ErrAlreadyConfigured):
		default:
			t.Errorf("unexpected error from a concurrent claim: %v", err)
		}
	}
	if wins != 1 {
		t.Errorf("%d of %d concurrent claims reported success, want exactly 1", wins, n)
	}
}
