package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ohstr/cashctl/internal/identity"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/pocket"
	"github.com/ohstr/cashctl/internal/store"
)

// testPocketKey is a fixed, valid secp256k1 private key used as the local
// identity (and so the pocket's encrypt-to-self key) in the toggle-on test
// path. The old cashctl.db ledger needs no key; the pocket does (its faithful
// Entry/Call/Handoff blobs are NIP-44-sealed to the local identity), so a test
// that seeds the pocket must configure the SAME local identity the command it
// drives will resolve — see ensurePocketIdentity.
const testPocketKey = "1111111111111111111111111111111111111111111111111111111111111111"

// ensurePocketIdentity makes the pocket's encrypt-to-self key match what the
// command path (loadStore→localPrivKey) resolves: in the toggle-on path it
// configures testPocketKey as the local identity if none exists. A no-op
// toggle-off, and a no-op if a test already configured its own identity (which
// pocketPriv then resolves instead), so a test's pocket seed and the command it
// drives always open the same store. This is the fix for the toggle-on "no
// identity configured" divergence: testLoad used a fixed key while loadStore
// used the real local identity.
func ensurePocketIdentity(t *testing.T) {
	t.Helper()
	if os.Getenv(pocketToggleEnv) == "" {
		return
	}
	if _, err := identity.Load(); err == nil {
		return
	}
	if err := identity.SaveLocalKey(testPocketKey); err != nil && !errors.Is(err, identity.ErrAlreadyConfigured) {
		t.Fatalf("configure pocket identity: %v", err)
	}
}

// ledgerView returns the *ledger.Ledger backing a store-under-test, for tests
// that only INSPECT a result (.Calls/.Entries/.Handoffs/.Find) and don't mutate
// it — e.g. the snapshot settleNow returns. NEVER mutate+Save the returned
// ledger on the pocket path: the embedded ledger's own Save targets cashctl.db,
// not the pocket; a mutate+save must go through the ledger.Store (testLoad).
func ledgerView(s ledger.Store) *ledger.Ledger {
	switch v := s.(type) {
	case *ledger.Ledger:
		return v
	case *pocket.LedgerStore:
		return v.Ledger
	default:
		return nil
	}
}

// pocketPriv resolves the configured local identity's private key — the same
// key loadStore opens the pocket with, so testLoad and the command agree.
func pocketPriv(t *testing.T) string {
	t.Helper()
	priv, err := identity.Resolve(func() (string, error) { return os.Getenv("NCLI_VAULT_PASSWORD"), nil })
	if err != nil {
		t.Fatalf("resolve pocket identity: %v", err)
	}
	return priv
}

// testLoad returns the store under test: the old cashctl.db ledger by default,
// or the pocket-backed ledger.Store when CASHCTL_POCKET is set. It lets the
// recovery/write-ahead suite run byte-for-byte the SAME assertions on both
// stores (the P4 gate): seed + inspect go through testLoad, the command path
// goes through loadStore, and both honour the same toggle AND the same local
// identity, so a test's seed and the command it drives share one store. The
// pocket store is closed on cleanup (its sqlite handle must not leak across
// repeated in-process command runs).
func testLoad(t *testing.T) ledger.Store {
	t.Helper()
	if os.Getenv(pocketToggleEnv) == "" {
		l, err := ledger.Load()
		if err != nil {
			t.Fatalf("ledger.Load(): %v", err)
		}
		return l
	}
	ensurePocketIdentity(t)
	dbPath, err := store.Path()
	if err != nil {
		t.Fatalf("store.Path(): %v", err)
	}
	ls, err := pocket.OpenLedgerStore(context.Background(), filepath.Dir(dbPath), pocketPriv(t), ledger.LokiHRP)
	if err != nil {
		t.Fatalf("pocket.OpenLedgerStore(): %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })
	return ls
}

// skipOnPocket skips a test under CASHCTL_POCKET with a documented reason — for
// a genuine pocket behaviour difference that needs a production fix, not a test
// change.
func skipOnPocket(t *testing.T, why string) {
	t.Helper()
	if os.Getenv(pocketToggleEnv) != "" {
		t.Skip("pocket: " + why)
	}
}
