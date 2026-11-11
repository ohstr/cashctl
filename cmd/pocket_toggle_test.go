package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/pocket"
	"github.com/ohstr/cashctl/internal/store"
)

// testPocketKey is a fixed, valid secp256k1 private key used to encrypt-to-self
// the pocket's sealed tables in tests. The old cashctl.db ledger needs no key;
// the pocket does (its faithful Entry/Call/Handoff blobs are NIP-44-sealed), so
// the toggle-on test path supplies this one instead of a real local identity.
const testPocketKey = "1111111111111111111111111111111111111111111111111111111111111111"

// testLoad returns the store under test: the old cashctl.db ledger by default,
// or the pocket-backed ledger.Store when CASHCTL_POCKET is set. It lets the
// recovery/write-ahead suite run byte-for-byte the SAME assertions on both
// stores (the P4 gate): seed + inspect go through testLoad, the command path
// goes through loadStore, and both honour the same toggle, so a test's seed and
// the command it drives share one store. The pocket store is closed on cleanup
// (its sqlite handle must not leak across repeated in-process command runs).
func testLoad(t *testing.T) ledger.Store {
	t.Helper()
	if os.Getenv(pocketToggleEnv) == "" {
		l, err := ledger.Load()
		if err != nil {
			t.Fatalf("ledger.Load(): %v", err)
		}
		return l
	}
	dbPath, err := store.Path()
	if err != nil {
		t.Fatalf("store.Path(): %v", err)
	}
	ls, err := pocket.OpenLedgerStore(context.Background(), filepath.Dir(dbPath), testPocketKey, ledger.LokiHRP)
	if err != nil {
		t.Fatalf("pocket.OpenLedgerStore(): %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })
	return ls
}
