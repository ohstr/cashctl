package cmd

import (
	"context"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/pocket"
	"github.com/ohstr/cashctl/internal/store"
)

// pocketToggleEnv selects the pocket-backed ledger.Store instead of the old
// cashctl.db ledger. It defaults OFF: until a command family is migrated
// (stage 3 of the pocket-ledger migration, the plan's §8) nothing sets it, so
// loadStore returns the existing ledger and behaviour is unchanged.
const pocketToggleEnv = "CASHCTL_POCKET"

// loadStore returns the ledger.Store the command layer should use: the existing
// cashctl.db ledger by default, or the pocket-backed store when the toggle is
// set. It is the single construction seam a family migration flips a command
// onto the pocket through (replacing that command's direct ledger.Load()).
//
// Stage 2 adds this mechanism; no command calls it yet (the 32 ledger.Load()
// sites are untouched), so it is dead until stage 3 — which keeps the default
// trivially unchanged.
func loadStore(cmd *cobra.Command) (ledger.Store, error) {
	if os.Getenv(pocketToggleEnv) == "" {
		return ledger.Load()
	}
	priv, err := localPrivKey(cmd)
	if err != nil {
		return nil, err
	}
	dbPath, err := store.Path()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(dbPath)
	// loki is cashctl's chain; a per-chain default is wired when multi-chain
	// lands. Context is the command's lifetime; the pocket's reads are bounded.
	return pocket.OpenLedgerStore(context.Background(), dir, priv, ledger.LokiHRP)
}
