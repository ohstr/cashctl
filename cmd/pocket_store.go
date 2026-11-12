package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ohstr/ncli/client/vault"

	"github.com/ohstr/cashctl/internal/identity"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/pocket"
	"github.com/ohstr/cashctl/internal/store"
)

// pocketToggleEnv opts OUT of the pocket-backed ledger.Store back to the old
// cashctl.db ledger. The pocket is now the DEFAULT (the migration is complete —
// the plan's §8); set CASHCTL_POCKET=0 to use the old ledger, kept for A/B
// comparison and rollback until internal/store is removed. See usePocket.
const pocketToggleEnv = "CASHCTL_POCKET"

// usePocket reports whether the pocket-backed ledger.Store is in use — the
// DEFAULT now (migration complete); CASHCTL_POCKET=0 opts back to the old
// cashctl.db ledger.
func usePocket() bool { return os.Getenv(pocketToggleEnv) != "0" }

// loadStore returns the ledger.Store the command layer should use: the existing
// cashctl.db ledger by default, or the pocket-backed store when the toggle is
// set. It is the single construction seam a family migration flips a command
// onto the pocket through (replacing that command's direct ledger.Load()).
//
// Stage 2 adds this mechanism; no command calls it yet (the 32 ledger.Load()
// sites are untouched), so it is dead until stage 3 — which keeps the default
// trivially unchanged.
func loadStore(cmd *cobra.Command) (ledger.Store, error) {
	if !usePocket() {
		return ledger.Load()
	}
	dbPath, err := store.Path()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(dbPath)
	// loki is cashctl's chain; a per-chain default is wired when multi-chain
	// lands. Context is the command's lifetime; the pocket's reads are bounded.
	priv, err := localPrivKey(cmd)
	if err != nil {
		// No NIP-CASH identity configured. The encrypt-to-self pocket still needs a
		// key to seal, because cashctl legitimately holds value WITHOUT a published
		// identity on the identity-less cash paths: bearer notes (`cash mint --to
		// cash`, `cash receive <note>`) and connection-key bills (`--to
		// connection:...`). The old cashctl.db ledger persisted those in plaintext
		// (no key); the pocket must not regress them. So seal under a stable
		// per-wallet DEVICE key — at-rest encryption only: it is never published,
		// never binds a bill, and does not enable sync (which needs the real
		// identity). Commands that genuinely require an identity enforce it
		// themselves (requireIdentity, e.g. `cash zap send`), so this weakens no
		// identity gate; it only lets the keyless storage path seal instead of
		// erroring with "invalid private key length".
		if errors.Is(err, identity.ErrNotConfigured) {
			devPriv, derr := pocketDeviceKey(dir)
			if derr != nil {
				return nil, derr
			}
			dialPocketPriv = devPriv
			return pocket.OpenLedgerStore(context.Background(), dir, devPriv, ledger.LokiHRP)
		}
		return nil, err
	}
	dialPocketPriv = priv
	return pocket.OpenLedgerStore(context.Background(), dir, priv, ledger.LokiHRP)
}

// pocketDeviceKey returns a stable per-wallet device key used ONLY to encrypt the
// local pocket at rest when no NIP-CASH identity is configured — the identity-less
// cash paths (bearer notes, connection-key bills). It is generated once and stored
// 0600 beside the pocket at <dir>/pocket.devkey; it is never published, never binds
// a bill, and is not a sync identity. Race-safe: O_CREATE|O_EXCL, and on a lost
// race the winner's key is read back, so concurrent processes converge on one key.
//
// Seam (documented, low-risk at dev stage): a wallet that holds value under this
// device key and LATER runs `cashctl init` seals subsequent writes under the new
// identity key, which cannot open the device-key-sealed blobs. The identity-less
// paths are ephemeral (receive/redeem a bearer note), and init-first is the norm,
// so this transition is rare; a future `cashctl init` can adopt the device key.
func pocketDeviceKey(dir string) (string, error) {
	path := filepath.Join(dir, "pocket.devkey")
	if b, err := os.ReadFile(path); err == nil {
		if k := strings.TrimSpace(string(b)); len(k) == 64 { // 32-byte hex
			return k, nil
		}
		// malformed (truncated write, etc.): regenerate — dev stage, zero prod users.
	}
	id, err := vault.GenerateIdentity()
	if err != nil {
		return "", fmt.Errorf("pocket device key: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) { // lost the race: adopt the winner's key
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return "", rerr
			}
			return strings.TrimSpace(string(b)), nil
		}
		return "", err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(id.PrivKeyHex); err != nil {
		return "", err
	}
	return id.PrivKeyHex, nil
}

// dialPocketPriv is the pocket identity key loadStore resolved, reused by the
// dial path's zap-event read (heldZapContext → zapEventFor), which has no cmd to
// resolve it again. Safe as package state: cashctl runs one command per process,
// and loadStore runs before any dial. "" for a keyless wallet (which holds no zap
// bill to dial).
var dialPocketPriv string

// zapEventFor reads the Cash Zap (kind-9522) stored with a held zap bill from
// whichever store the toggle selects. The dial path has no cmd/store, so — like
// ledger.ZapEventFor — this is self-contained: default reads cashctl.db; the
// pocket opens a short-lived store with the key loadStore already resolved (no
// re-prompt), reads, and closes it. Used by heldZapContext (cmd/dial.go).
func zapEventFor(token string) (string, error) {
	if !usePocket() {
		return ledger.ZapEventFor(token)
	}
	dbPath, err := store.Path()
	if err != nil {
		return "", err
	}
	s, err := pocket.OpenLedgerStore(context.Background(), filepath.Dir(dbPath), dialPocketPriv, ledger.LokiHRP)
	if err != nil {
		return "", err
	}
	defer func() { _ = s.Close() }()
	return s.ZapEventFor(token)
}
