package pocket

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/ohstr/nmilat/nipCP"
	"github.com/ohstr/nmilat/nipcash"
	"github.com/ohstr/nmilat/nipcash/client"
	"github.com/ohstr/nmilat/utils"
)

// localRelay is the single, in-process event store. nipCP.Store/Sync need a relay
// name to iterate; the Store adapter ignores it (there is one local store, no
// socket). nipCP.Store's MinStored clamps to len(relays), so one suffices.
const localRelay = "local"

// Pocket is cashctl's local NIP-CP Cash Pocket: the canonical event store
// (pocket.db), the decoded cache (pocket-cache.db), and a PocketJournal that
// writes calls ahead into the store. It is offline by default — nothing listens,
// nothing is exposed (sync/serve are later phases). This file (P2 step 1) wires
// the journal + the ingest/apply loop; the cutover of the money commands off the
// old cashctl.db ledger is P2 step 2.
type Pocket struct {
	Store   *Store
	Cache   *Cache
	Journal *Journal

	userPriv string
	userPub  string
	relays   []string
}

// Journal is cashctl's client.Journal: nmilat's PocketJournal (the write-ahead
// protocol) plus a projection of each written pending entry into the decoded
// cache. It satisfies the SAME nipcash/client.Journal interface the old
// internal/ledger ledgerJournal did, so the nipcash client drives it unchanged.
type Journal struct {
	*nipCP.PocketJournal
	cache *Cache
}

var _ client.Journal = (*Journal)(nil)

// WriteAhead stores each call's pending entry durably (the embedded PocketJournal
// seals it into pocket.db and reads it back), then projects it into the decoded
// cache. The cache projection is best-effort: the event store is authoritative
// and RebuildFromStore recovers any drift, so a cache-fold error here must NOT
// fail the call — the pending entry is already durable, and failing would stop a
// call whose write-ahead actually succeeded (the plan's risk 16: store before
// cache; cache failure non-fatal).
func (j *Journal) WriteAhead(ctx context.Context, calls []nipcash.PendingCall) error {
	if err := j.PocketJournal.WriteAhead(ctx, calls); err != nil {
		return err
	}
	for _, pc := range calls {
		if e, ok := j.PocketJournal.Written(pc.RequestID); ok {
			_, _ = j.cache.Fold(e) // best-effort; store is authoritative
		}
	}
	return nil
}

// Open opens (creating if absent) the pocket under dir: pocket.db (event store)
// and pocket-cache.db (decoded cache), and builds the journal for userPriv. chain
// is the default chain stamped on a call that carries none. If the cache schema
// predates this build, the decoded projection is rebuilt from the event store
// (the cache is derived, never authoritative — the plan's risk 15).
func Open(ctx context.Context, dir, userPriv, chain string) (*Pocket, error) {
	userPub, err := utils.GetPublicKey(userPriv)
	if err != nil {
		return nil, fmt.Errorf("pocket: user key: %w", err)
	}
	store, err := OpenStore(filepath.Join(dir, "pocket.db"))
	if err != nil {
		return nil, err
	}
	cache, err := OpenCache(filepath.Join(dir, "pocket-cache.db"))
	if err != nil {
		store.Close()
		return nil, err
	}
	relays := []string{localRelay}
	pj := &nipCP.PocketJournal{UserPriv: userPriv, Relays: relays, IO: store, Device: cache, Chain: chain}
	p := &Pocket{
		Store:    store,
		Cache:    cache,
		Journal:  &Journal{PocketJournal: pj, cache: cache},
		userPriv: userPriv,
		userPub:  userPub,
		relays:   relays,
	}
	if err := p.rebuildIfStale(ctx); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}

// OpenLedgerOnly opens ONLY the decoded cache (pocket-cache.db, sqlite) for the
// A′ ledger path — NOT the bbolt pocket.db event store. The ledger path
// (LedgerStore: embed + sealed faithful tables in the cache) never touches the
// event store, so opening pocket.db here would take its single-writer bbolt lock
// for nothing and deadlock the nested ledger.Load() calls several command flows
// make (withdraw/order). sqlite tolerates multiple connections per process, so a
// cache-only open lets each (fresh) load succeed — matching the old file-based
// ledger's per-call read. The pocket.db event store is opened separately, only by
// the NIP-CP projection/sync/serve (later phases), never on the ledger path.
func OpenLedgerOnly(dir, userPriv string) (*Pocket, error) {
	// A keyless wallet (no identity configured yet) has nothing sealed: the cache
	// is empty, so no blob is ever opened and no key is needed. userPub stays ""
	// (used only to open sealed blobs, of which there are none). loadLedger over a
	// non-empty cache with no key fails closed on the first openEntry — the key
	// loss/SPOF case, not this fresh-wallet path.
	var userPub string
	if userPriv != "" {
		var err error
		userPub, err = utils.GetPublicKey(userPriv)
		if err != nil {
			return nil, fmt.Errorf("pocket: user key: %w", err)
		}
	}
	cache, err := OpenCache(filepath.Join(dir, "pocket-cache.db"))
	if err != nil {
		return nil, err
	}
	// Store/Journal are nil: the ledger path uses only the cache. Close() and the
	// LedgerStore tolerate a nil Store (the rebuild-from-store path, which needs
	// pocket.db, is not reachable on the ledger path).
	return &Pocket{Cache: cache, userPriv: userPriv, userPub: userPub}, nil
}

// Close releases the event store (bbolt file lock) and the cache. Store is nil on
// the ledger-only path (OpenLedgerOnly); the nil check makes Close safe there.
func (p *Pocket) Close() {
	if p.Store != nil {
		p.Store.Close()
	}
	if p.Cache != nil {
		p.Cache.Close()
	}
}

// rebuildIfStale re-folds the cache from the event store when the cache's stored
// schema version differs from this build's (a forward-compat upgrade). On a fresh
// cache the versions match and nothing happens.
func (p *Pocket) rebuildIfStale(ctx context.Context) error {
	v, err := p.Cache.SchemaVersion()
	if err != nil {
		return err
	}
	if v == cacheSchemaVersion {
		return nil
	}
	if err := p.RebuildFromStore(ctx); err != nil {
		return err
	}
	return p.Cache.setSchemaVersion(cacheSchemaVersion)
}

// allEntries reads every pocket entry (all revs of every w) from the event store.
// The fold resolves the winner per w, so order does not matter.
func (p *Pocket) allEntries(ctx context.Context) ([]nipCP.Entry, error) {
	user, err := utils.GetPublicKey(p.userPriv)
	if err != nil {
		return nil, fmt.Errorf("pocket: user key: %w", err)
	}
	wraps, err := p.Store.Query(ctx, localRelay, nipCP.FullSyncFilter(user))
	if err != nil {
		return nil, err
	}
	return nipCP.OpenAll(wraps, p.userPriv)
}

// RebuildFromStore drops the decoded projection and re-folds every entry from the
// event store. Because the fold is order-independent, the result equals the
// incrementally-maintained cache.
func (p *Pocket) RebuildFromStore(ctx context.Context) error {
	all, err := p.allEntries(ctx)
	if err != nil {
		return err
	}
	return p.Cache.Rebuild(all)
}

// put seals one entry, stores it durably in the event store (read back), then
// folds it into the decoded cache. The store write is fatal (funds durability);
// a cache-fold error after a successful store is returned but is non-fatal —
// the store is the source of truth and RebuildFromStore recovers (risk 16).
func (p *Pocket) put(ctx context.Context, e nipCP.Entry) error {
	wrap, sealed, err := nipCP.Seal(e, p.userPriv)
	if err != nil {
		return fmt.Errorf("pocket: seal %s: %w", e.W, err)
	}
	if _, err := nipCP.Store(ctx, p.Store, p.relays, wrap); err != nil {
		return fmt.Errorf("pocket: store %s: %w", e.W, err)
	}
	if _, err := p.Cache.Fold(sealed); err != nil {
		return fmt.Errorf("pocket: cache fold %s (stored ok; RebuildFromStore recovers): %w", e.W, err)
	}
	return nil
}

// Add records a bill the wallet now holds (minted, received, or a remainder) as a
// pocket entry: sealed into the event store and folded into the cache.
func (p *Pocket) Add(ctx context.Context, e nipCP.Entry) error { return p.put(ctx, e) }

// Apply reconciles a call's outcome into the pocket (NIP-CP §Writing). It runs
// nmilat's ApplyResult — the protocol logic — then persists its result in the
// required order: the new bills first (each stored and read back), then the close
// batch (the pending entry done, and every source not rewritten in place marked
// spent). ApplyResult requires the pending entry still pending; once applied it
// is done, so a caller that re-reads the pending's state (PendingState) before
// Apply gets exactly-once. Re-running ApplyResult over a still-pending entry
// re-derives the SAME Hub-side outcome (the bill the Hub already made), which the
// store deduplicates by content/rev; a transient duplicate wrap resolves by rev
// (self-healing, never fund loss).
func (p *Pocket) Apply(ctx context.Context, pending nipCP.Entry, sources []nipCP.Entry, outcome nipCP.Outcome) error {
	writes, closeBatch, err := nipCP.ApplyResult(pending, sources, outcome)
	if err != nil {
		return err
	}
	// New bills first, each durable + read back, BEFORE the close batch — so a
	// crash between them leaves the pending open (reconcile re-runs) rather than
	// a source marked spent with its replacement bill not yet stored.
	for _, e := range writes {
		if err := p.put(ctx, e); err != nil {
			return err
		}
	}
	for _, e := range closeBatch {
		if err := p.put(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// Lookup maps a lost-reply recovery action for a still-pending call to the
// pocket action the settle driver performs (NIP-CP §Writing, lookup answers). It
// is nmilat's ApplyLookup with this device's id; the settle driver (P2 step 2)
// executes the returned PocketAction (apply a done result, close, or re-place
// under the derived id). localDevice is read from the cache's DeviceStore.
func (p *Pocket) Lookup(ctx context.Context, pending nipCP.Entry, pc nipcash.PendingCall, a client.Action) (nipCP.PocketAction, error) {
	dev, err := p.Cache.DeviceID(ctx)
	if err != nil {
		return nipCP.PocketAction{}, err
	}
	return nipCP.ApplyLookup(pending, pc, a, dev), nil
}
