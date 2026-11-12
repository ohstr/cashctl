package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nip11"
	"github.com/ohstr/nmilat/nipCP"
	"github.com/ohstr/nmilat/relay"
	"github.com/ohstr/nmilat/utils"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
	"github.com/ohstr/cashctl/internal/pocket"
	"github.com/ohstr/cashctl/internal/store"
)

// newCashPocketCmd is the `cash pocket` group: inspect the local Cash Pocket
// (NIP-CP) and, opt-in, sync it to/from relays. `balance`/`entries` read the
// authoritative held bills from the local sealed ledger (no relay). `sync` is the
// only command that contacts a relay, and only when run. `export`/`import` move
// the pocket as a portable bundle.
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
		newCashPocketServeCmd(),
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

// ---- cash pocket balance ------------------------------------------------------

func newCashPocketBalanceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "balance",
		Short: "Show the pocket balance per chain (held bills; no relay)",
		Args:  output.NoArgs,
		RunE:  runCashPocketBalance,
	}
}

func runCashPocketBalance(cmd *cobra.Command, _ []string) error {
	// Read the authoritative held bills (local + synced), not the decoded cache:
	// the money-command Save() path persists to the sealed ledger without folding
	// into the cache, so the cache misses locally-held bills (#20). loadStore is
	// the pocket (default) or the old ledger (CASHCTL_POCKET=0).
	st, err := loadStore(cmd)
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	defer st.Close()
	bal := map[string]uint64{}
	for _, e := range st.Held() {
		pe, ok := pocket.DisplayEntry(e)
		if !ok {
			continue
		}
		bal[pe.Chain] += pe.Amount
	}
	// Deterministic chain order (a map range is unordered; a stable order matches
	// `cash consolidate`'s sortedHubKeys so repeated runs/agents see the same output).
	chains := make([]string, 0, len(bal))
	for chain := range bal {
		chains = append(chains, chain)
	}
	sort.Strings(chains)

	jsonMode, _ := cmd.Flags().GetBool("json")
	if jsonMode {
		// Wrapped, labeled object (matching `cash pocket entries` and `wallet
		// balance`): a named key, and every amount carries the _millis unit suffix —
		// not a bare `{chain: amount}` map whose values have no stated unit or key.
		rows := make([]map[string]any, 0, len(chains))
		for _, chain := range chains {
			rows = append(rows, map[string]any{"chain": chain, "amount_millis": bal[chain]})
		}
		output.PrintJSON(map[string]any{"balances": rows})
		return nil
	}
	if len(bal) == 0 {
		output.Println("Pocket is empty.")
		return nil
	}
	for _, chain := range chains {
		output.Printf("%s: %s\n", chain, output.FormatAmount(int64(bal[chain])))
	}
	return nil
}

// ---- cash pocket entries ------------------------------------------------------

func newCashPocketEntriesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "entries",
		Short: "List held pocket bills (no relay)",
		Args:  output.NoArgs,
		RunE:  runCashPocketEntries,
	}
}

func runCashPocketEntries(cmd *cobra.Command, _ []string) error {
	// Authoritative held bills (local + synced) from the sealed ledger, not the
	// decoded cache (which misses locally-held bills — #20).
	st, err := loadStore(cmd)
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	defer st.Close()
	var entries []nipCP.Entry
	for _, e := range st.Held() {
		if pe, ok := pocket.DisplayEntry(e); ok {
			entries = append(entries, pe)
		}
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	if jsonMode {
		rows := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			rows = append(rows, pocketEntryJSON(e))
		}
		output.PrintJSON(map[string]any{"entries": rows})
		return nil
	}
	if len(entries) == 0 {
		output.Println("Pocket is empty.")
		return nil
	}
	for _, e := range entries {
		output.Printf("%s  %s  %s  %s\n", e.W[:min(8, len(e.W))], e.State, e.Chain, output.FormatAmount(int64(e.Amount)))
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

func newCashPocketServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the local pocket as your own relay (opt-in; be your own relay)",
		Long: `Fronts the local pocket.db (pure NIP-CP gift-wraps) with a NIP-01 relay
over a WebSocket, so another device on the same identity key — or a cashkit
wallet — can sync off YOU, peer-to-peer, with no third-party relay.

It serves kind-1059 gift-wraps ONLY to a connection that authenticates
(NIP-42) as their recipient (the owner key) — a non-owner is refused.

Binds to loopback by default; --public (0.0.0.0) requires TLS / a tunnel for
anything off-LAN and is exposed to your whole network. This is the one command
that opens a listening socket.`,
		Args: output.NoArgs,
		RunE: runCashPocketServe,
	}
	cmd.Flags().String("listen", "127.0.0.1:7777", "address to listen on (host:port)")
	cmd.Flags().Bool("public", false, "bind 0.0.0.0 (all interfaces) instead of loopback — exposes the pocket to your network")
	return cmd
}

func runCashPocketServe(cmd *cobra.Command, _ []string) error {
	priv, err := localPrivKey(cmd)
	if err != nil {
		return err // no identity: the pocket is encrypt-to-self; needs a key to serve
	}
	dir, err := pocketDir()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	listenAddr, _ := cmd.Flags().GetString("listen")
	public, _ := cmd.Flags().GetBool("public")
	if public {
		if _, port, e := net.SplitHostPort(listenAddr); e == nil {
			listenAddr = net.JoinHostPort("0.0.0.0", port)
		}
		output.Notef(false, "serving on ALL interfaces (%s) — your pocket is reachable from your network; use TLS or a tunnel off-LAN.", listenAddr)
	} else if h, _, e := net.SplitHostPort(listenAddr); e == nil && h == "0.0.0.0" {
		return output.InvocationError(cmd, fmt.Errorf("refusing to bind 0.0.0.0 without --public (which exposes the pocket to your network)"))
	}

	ctx := context.Background()

	// Held bills from the sealed ledger (cache-only, no bbolt), read before the
	// full Pocket opens so the two bbolt opens don't overlap.
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

	// Listen first so the handler's NIP-42 relay URL matches the address clients
	// dial (ValidateAuthEvent requires an exact relay-tag match, else AUTH — and
	// so the recipient-only 1059 gate — silently fails).
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return output.RuntimeError(cmd, fmt.Errorf("listen %s: %w", listenAddr, err))
	}
	url := "ws://" + ln.Addr().String()

	handler, err := pocketServeHandler(ctx, p, held, url)
	if err != nil {
		_ = ln.Close()
		return output.RuntimeError(cmd, err)
	}
	defer handler.VerificationWorker.Stop()

	// Emit a machine-readable readiness line BEFORE blocking, so an agent/supervisor
	// has a parseable "listening at X" signal (stdout stays JSON-only under --json)
	// rather than scraping human prose. The command then blocks until signalled.
	if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
		output.PrintJSON(map[string]any{"listening": url, "relay": url, "public": public})
	} else {
		output.Printf("Serving the pocket at %s — sync a second device with: cashctl cash pocket sync --relay %s\n", url, url)
		output.Printf("(serves kind-1059 only to the owner key; Ctrl-C to stop)\n")
	}
	// Blocks until the process is signalled; the bbolt pocket.db is held open for
	// the serve's lifetime, so don't run `cash pocket sync`/`export` on this same
	// device meanwhile (they open pocket.db too — single-writer).
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return output.RuntimeError(cmd, err)
	}
	return nil
}

// pocketServeHandler projects the held bills into pocket.db (pure NIP-CP 9525s)
// and returns a relay SessionHandler over that same event store, gated so a
// kind-1059 wrap is served ONLY to a connection authenticated (NIP-42) as its
// recipient (WithRecipientOnlyKinds(1059)). Factored out so a test can serve it
// on an ephemeral listener. The caller owns the Pocket + must Stop the returned
// handler's VerificationWorker.
func pocketServeHandler(ctx context.Context, p *pocket.Pocket, held []ledger.Entry, relayURL string) (*relay.SessionHandler, error) {
	if err := p.Project(ctx, held); err != nil {
		return nil, fmt.Errorf("project held bills to pocket.db: %w", err)
	}
	// URL must equal what clients dial: NIP-42's ValidateAuthEvent does an exact
	// relay-tag match, and without it AUTH (and the 1059 recipient-only gate) fails.
	// Limitation.AuthRequired is the session's live gate (s.limitation == &meta.Limitation):
	// require NIP-42 AUTH before any REQ or EVENT. Serve is network READ-ONLY — the
	// owner's device populates pocket.db LOCALLY via Project and remote devices only
	// PULL, so no client ever legitimately writes; this blocks an anonymous LAN host
	// from publishing events that would accumulate unbounded in pocket.db (review
	// finding — poisoning/DoS, Medium on --public).
	meta := &nip11.Metadata{
		URL:         relayURL,
		Name:        "cashctl-pocket",
		Description: "a personal Cash Pocket relay (NIP-CP)",
		Limitation:  nip11.Limitation{AuthRequired: true},
	}
	handler := relay.NewSessionHandler(p.Store.EventStore(), meta, nil,
		// Reads: a 1059 is delivered ONLY to a connection NIP-42-authed as a pubkey in
		// its p tags (the owner).
		relay.WithRecipientOnlyKinds(1059),
		// Writes: refuse kind-1059 EVENTs entirely. Gift-wraps are ephemeral-key
		// signed (never relay-self-authored) and the serve has no self key, so
		// WithRelayAuthoredKinds(1059) rejects every 1059 publish — closing the
		// foreign-wrap injection the owner would otherwise re-pull and re-discard on
		// every sync. Injected wraps were already inert (OpenAll discards a
		// foreign-sealed wrap); this stops them being stored and re-processed.
		relay.WithRelayAuthoredKinds(1059),
	)
	handler.VerificationWorker.Start(1)
	return handler, nil
}

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
	// Reverse-project imported HELD bills into the authoritative sealed ledger so
	// they are SPENDABLE, not just visible — the cache fold above is display-only.
	// Mirrors sync's pull path (#21). Idempotent (dedup by w); foreign/spent skipped.
	spendable, err := p.ImportHeld(ctx, entries)
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	if jsonMode {
		output.PrintJSON(map[string]any{"imported": imported, "spendable": spendable})
		return nil
	}
	output.Printf("Imported %d pocket entr%s (%d spendable).\n", imported, plural(imported), spendable)
	return nil
}
