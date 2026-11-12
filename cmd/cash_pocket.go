package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nipCP"
	"github.com/ohstr/nmilat/utils"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
	"github.com/ohstr/cashctl/internal/pocket"
	"github.com/ohstr/cashctl/internal/store"
)

// newCashPocketCmd is the `cash pocket` group: inspect the local Cash Pocket
// (NIP-CP) and, opt-in, sync it to/from relays. `balance`/`entries` read the
// local decoded cache only (instant, no relay, no key). `sync` is the only
// command that contacts a relay, and only when run. `export`/`import` move the
// pocket as a portable bundle.
func newCashPocketCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pocket",
		Short: "Inspect and sync the local Cash Pocket (NIP-CP)",
		RunE:  groupRunE,
	}
	cmd.AddCommand(
		newCashPocketBalanceCmd(),
		newCashPocketEntriesCmd(),
		newCashPocketSyncCmd(),
		newCashPocketExportCmd(),
		newCashPocketImportCmd(),
	)
	return cmd
}

// pocketDir is the directory holding pocket.db + pocket-cache.db (the same dir
// the ledger store uses — filepath.Dir of the cashctl.db path, honouring
// --config-dir).
func pocketDir() (string, error) {
	dbPath, err := store.Path()
	if err != nil {
		return "", err
	}
	return filepath.Dir(dbPath), nil
}

// openPocketCache opens the decoded cache (pocket-cache.db) alone — no key, no
// relay, no bbolt. The cache holds decoded rows, so reads need no decryption.
func openPocketCache() (*pocket.Cache, error) {
	dir, err := pocketDir()
	if err != nil {
		return nil, err
	}
	return pocket.OpenCache(filepath.Join(dir, "pocket-cache.db"))
}

// ---- cash pocket balance ------------------------------------------------------

func newCashPocketBalanceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "balance",
		Short: "Show the pocket balance per chain (from the local cache; no relay)",
		Args:  output.NoArgs,
		RunE:  runCashPocketBalance,
	}
}

func runCashPocketBalance(cmd *cobra.Command, _ []string) error {
	c, err := openPocketCache()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	defer c.Close()
	bal, err := c.Balance()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	if jsonMode {
		out := map[string]uint64{}
		for chain, amt := range bal {
			out[chain] = amt
		}
		output.PrintJSON(out)
		return nil
	}
	if len(bal) == 0 {
		output.Println("Pocket is empty.")
		return nil
	}
	for chain, amt := range bal {
		output.Printf("%s: %s\n", chain, output.FormatAmount(int64(amt)))
	}
	return nil
}

// ---- cash pocket entries ------------------------------------------------------

func newCashPocketEntriesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "entries",
		Short: "List the resolved pocket entries (from the local cache; no relay)",
		Args:  output.NoArgs,
		RunE:  runCashPocketEntries,
	}
}

func runCashPocketEntries(cmd *cobra.Command, _ []string) error {
	c, err := openPocketCache()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	defer c.Close()
	entries, conflicts, err := c.Entries()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	if jsonMode {
		rows := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			rows = append(rows, pocketEntryJSON(e))
		}
		output.PrintJSON(map[string]any{"entries": rows, "conflicts": conflicts})
		return nil
	}
	if len(entries) == 0 {
		output.Println("Pocket is empty.")
		return nil
	}
	for _, e := range entries {
		output.Printf("%s  %s  %s  %s\n", e.W[:min(8, len(e.W))], e.State, e.Chain, output.FormatAmount(int64(e.Amount)))
	}
	if len(conflicts) > 0 {
		output.Printf("(%d entr%s need a cash_status re-check)\n", len(conflicts), plural(len(conflicts)))
	}
	return nil
}

func pocketEntryJSON(e nipCP.Entry) map[string]any {
	row := map[string]any{
		"w":              e.W,
		"rev":            e.Rev,
		"state":          string(e.State),
		"amount_millis":  e.Amount,
		"chain":          e.Chain,
		"identity_type":  string(e.Identity.Type),
		"identity_value": e.Identity.Value,
	}
	if e.ExpiresAt != nil {
		row["expires_at"] = *e.ExpiresAt
	}
	if e.Mint != "" {
		row["minter"] = e.Mint
	}
	if e.Source != "" {
		row["source"] = e.Source
	}
	return row
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// ---- cash pocket sync ---------------------------------------------------------

func newCashPocketSyncCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Sync the Cash Pocket to/from relays (opt-in — the only command that contacts a relay)",
		Long: `Projects the held bills to pure NIP-CP kind-9525 entries, publishes them
to the given relays, and pulls the same identity's entries back (incrementally,
from a saved cursor minus a 2-day window), folding them into the local cache.

Off by default — nothing contacts a relay unless this command is run.`,
		Args: output.NoArgs,
		RunE: runCashPocketSync,
	}
	cmd.Flags().StringSlice("relay", nil, "relay(s) to sync with (wss://…) — required")
	return cmd
}

// pocketSyncResult is a sync's outcome, for --json and the human summary.
type pocketSyncResult struct {
	Published int   `json:"published"`
	Imported  int   `json:"imported"`
	Spendable int   `json:"spendable"`
	Cursor    int64 `json:"cursor"`
}

func runCashPocketSync(cmd *cobra.Command, _ []string) error {
	relays, _ := cmd.Flags().GetStringSlice("relay")
	if len(relays) == 0 {
		return output.InvocationError(cmd, fmt.Errorf("say which relay(s) to sync with: --relay wss://…"))
	}
	priv, err := localPrivKey(cmd)
	if err != nil {
		return err // no identity: the pocket is encrypt-to-self; needs a key to sync
	}
	userPub, err := utils.GetPublicKey(priv)
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	dir, err := pocketDir()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Held bills from the authoritative sealed ledger (cache-only open — no bbolt),
	// read before opening the full Pocket so the two bbolt opens don't overlap.
	ls, err := pocket.OpenLedgerStore(ctx, dir, priv, ledger.LokiHRP)
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	held := ls.AllEntries()
	_ = ls.Close()

	// The full Pocket (pocket.db event store + cache) for project + sync.
	p, err := pocket.Open(ctx, dir, priv, ledger.LokiHRP)
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	defer p.Close()

	remote := nipCP.DialRelays(priv)
	res, err := syncPocket(ctx, p, held, priv, userPub, remote, relays, time.Now().Unix())
	if err != nil {
		return output.RuntimeError(cmd, err)
	}

	jsonMode, _ := cmd.Flags().GetBool("json")
	if jsonMode {
		output.PrintJSON(res)
		return nil
	}
	output.Printf("Synced: published %d, imported %d (%d now spendable).\n", res.Published, res.Imported, res.Spendable)
	return nil
}

// syncPocket is the sync engine, decoupled from the relay transport (DialRelays
// in the command; a fake in tests). It (1) projects held bills into pocket.db as
// pure 9525s, (2) publishes every local winner wrap to the relays, (3) pulls the
// same identity's wraps incrementally (cursor − 2-day NIP-59 window, risk 13),
// (4) stores each pulled wrap locally and folds it into the cache, and (5)
// advances the cursor. It is additive: the sealed ledger stays authoritative.
func syncPocket(ctx context.Context, p *pocket.Pocket, held []ledger.Entry, priv, userPub string, remote nipCP.Relays, relays []string, now int64) (pocketSyncResult, error) {
	var res pocketSyncResult

	// 1. Project held bills → local pocket.db (pure NIP-CP 9525s).
	if err := p.Project(ctx, held); err != nil {
		return res, err
	}

	// 2. Publish every local winner wrap to the relays.
	localWraps, err := p.Store.Query(ctx, pocket.LocalRelay, nipCP.FullSyncFilter(userPub))
	if err != nil {
		return res, err
	}
	byID := make(map[string]*nip01.Event, len(localWraps))
	for _, w := range localWraps {
		byID[w.ID] = w
	}
	localAll, err := nipCP.OpenAll(localWraps, priv)
	if err != nil {
		return res, err
	}
	localWinners, _ := nipCP.Resolve(localAll)
	for _, w := range localWinners {
		wrap, ok := byID[w.WrapID]
		if !ok {
			continue
		}
		if _, err := nipCP.Store(ctx, remote, relays, wrap); err != nil {
			return res, err
		}
		res.Published++
	}

	// 3. Pull incrementally (cursor − 2-day window).
	cursor, err := p.Cache.SyncCursor()
	if err != nil {
		return res, err
	}
	synced, err := nipCP.Sync(ctx, remote, relays, priv, nipCP.IncrementalFilter(userPub, cursor), nipCP.SeenSet{})
	if err != nil {
		return res, err
	}

	// 4. Store each pulled wrap locally (idempotent) and fold into the cache.
	for _, ev := range synced.Wraps {
		if err := p.Store.Publish(ctx, pocket.LocalRelay, ev); err != nil {
			return res, err
		}
	}
	for _, e := range synced.Entries {
		if _, err := p.Cache.Fold(e); err != nil {
			return res, err
		}
		res.Imported++
	}

	// 4b. Reverse-project pulled HELD bills into the authoritative sealed ledger so
	// the money commands can SPEND them, not just show them (the cache fold above is
	// display-only). Idempotent — dedup by wallet pubkey; foreign/spent are skipped.
	spendable, err := p.ImportHeld(ctx, synced.Entries)
	if err != nil {
		return res, err
	}
	res.Spendable = spendable

	// 5. Advance the cursor.
	if err := p.Cache.SetSyncCursor(now); err != nil {
		return res, err
	}
	res.Cursor = now
	return res, nil
}

// ---- cash pocket export / import ---------------------------------------------

func newCashPocketExportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export <file>",
		Short: "Export the pocket as a portable bundle of gift-wrapped entries",
		Args:  output.MaximumNArgs(1),
		RunE:  runCashPocketExport,
	}
	return cmd
}

func runCashPocketExport(cmd *cobra.Command, args []string) error {
	path := ""
	if len(args) == 1 {
		path = args[0]
	}
	if path == "" {
		return output.InvocationError(cmd, fmt.Errorf("say where to write the bundle: cashctl cash pocket export <file>"))
	}
	priv, err := localPrivKey(cmd)
	if err != nil {
		return err
	}
	userPub, err := utils.GetPublicKey(priv)
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	dir, err := pocketDir()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	ctx := context.Background()

	ls, err := pocket.OpenLedgerStore(ctx, dir, priv, ledger.LokiHRP)
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	held := ls.AllEntries()
	_ = ls.Close()

	p, err := pocket.Open(ctx, dir, priv, ledger.LokiHRP)
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	defer p.Close()
	if err := p.Project(ctx, held); err != nil {
		return output.RuntimeError(cmd, err)
	}
	wraps, err := p.Store.Query(ctx, pocket.LocalRelay, nipCP.FullSyncFilter(userPub))
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	blob, err := json.MarshalIndent(wraps, "", "  ")
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		return output.RuntimeError(cmd, err)
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	if jsonMode {
		output.PrintJSON(map[string]any{"exported": len(wraps), "path": path})
		return nil
	}
	output.Printf("Exported %d pocket entr%s to %s (needs your identity key to read).\n", len(wraps), plural(len(wraps)), path)
	return nil
}

func newCashPocketImportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "import <file>",
		Short: "Import a pocket bundle back into the local pocket",
		Args:  output.MaximumNArgs(1),
		RunE:  runCashPocketImport,
	}
}

func runCashPocketImport(cmd *cobra.Command, args []string) error {
	path := ""
	if len(args) == 1 {
		path = args[0]
	}
	if path == "" {
		return output.InvocationError(cmd, fmt.Errorf("say which bundle to import: cashctl cash pocket import <file>"))
	}
	priv, err := localPrivKey(cmd)
	if err != nil {
		return err
	}
	dir, err := pocketDir()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	ctx := context.Background()

	blob, err := os.ReadFile(path)
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	var wraps []*nip01.Event
	if err := json.Unmarshal(blob, &wraps); err != nil {
		return output.InvalidInputError(cmd, path, fmt.Errorf("not a pocket bundle: %w", err))
	}

	p, err := pocket.Open(ctx, dir, priv, ledger.LokiHRP)
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	defer p.Close()

	imported := 0
	for _, w := range wraps {
		if w == nil {
			continue
		}
		if err := p.Store.Publish(ctx, pocket.LocalRelay, w); err != nil {
			return output.RuntimeError(cmd, err)
		}
	}
	entries, err := nipCP.OpenAll(wraps, priv)
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	for _, e := range entries {
		if _, err := p.Cache.Fold(e); err != nil {
			return output.RuntimeError(cmd, err)
		}
		imported++
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	if jsonMode {
		output.PrintJSON(map[string]any{"imported": imported})
		return nil
	}
	output.Printf("Imported %d pocket entr%s.\n", imported, plural(imported))
	return nil
}
