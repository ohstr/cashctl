package pocket

import (
	"database/sql"
	"fmt"
	"strconv"

	_ "modernc.org/sqlite" // pure-Go sqlite driver, as internal/store uses

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/nmilat/nipCP"
)

// cacheSchemaVersion is bumped whenever the decoded-cache schema changes; a
// mismatch forces a rebuild from pocket.db (the cache is derived, never
// authoritative). See the plan's risk 15 (forward-compat).
const cacheSchemaVersion = 2

// Cache is the decoded state cache (pocket-cache.db): a relational projection of
// the resolved pocket, for zero-latency reads (indexed balance/entries) and
// reconciliation. It is LOCAL_ONLY — never synced, never served — and fully
// rebuildable from the canonical event store (Rebuild). It is NOT a relational
// wallet model: resolution runs through nmilat's nipCP semantics; this only
// stores the decoded winner per w.
type Cache struct {
	db *sql.DB
}

const cacheDDL = `
CREATE TABLE IF NOT EXISTS entries (
  w                 TEXT PRIMARY KEY,
  rev               INTEGER NOT NULL,
  state             TEXT NOT NULL,
  is_final          INTEGER NOT NULL,
  is_call           INTEGER NOT NULL,
  rumor_created_at  INTEGER NOT NULL,
  rumor_id          TEXT NOT NULL,
  amount            INTEGER NOT NULL,
  chain             TEXT NOT NULL,
  identity_type     TEXT NOT NULL DEFAULT '',
  identity_value    TEXT NOT NULL DEFAULT '',
  identity_ia       TEXT NOT NULL DEFAULT '',
  expires_at        INTEGER,            -- NULL = never
  mint              TEXT NOT NULL DEFAULT '',
  source            TEXT NOT NULL DEFAULT '',
  cash              TEXT NOT NULL DEFAULT '',
  checked_at        INTEGER NOT NULL,
  wrap_id           TEXT NOT NULL DEFAULT '',
  wrap_key          TEXT NOT NULL DEFAULT '',
  conflict          INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_entries_state       ON entries(state);
CREATE INDEX IF NOT EXISTS idx_entries_chain_state ON entries(chain, state);
CREATE INDEX IF NOT EXISTS idx_entries_expires     ON entries(expires_at);

CREATE TABLE IF NOT EXISTS derived_ids (
  id                 TEXT PRIMARY KEY,
  parent_request_id  TEXT NOT NULL DEFAULT '',
  deadline           INTEGER,
  keep_until         INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

-- pending_calls holds cashctl's in-flight bill-creating calls (ledger.Call),
-- each serialized and NIP-44-sealed (encrypt-to-self) so no secret is ever
-- cleartext at rest (the plan's risk 17). Keyed by request_id. These are
-- cashctl-private local state (not rebuildable from pocket.db) — the precious
-- rows of risk 19. The migration's A' approach loads these into an in-memory
-- ledger.Ledger buffer and writes them back here on Save.
CREATE TABLE IF NOT EXISTS pending_calls (
  request_id TEXT PRIMARY KEY,
  sealed     TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);

-- handoffs holds recipient hand-offs (ledger.Handoff), NIP-44-sealed — a
-- recipient token must never be lost, so it is persisted durably + encrypted.
CREATE TABLE IF NOT EXISTS handoffs (
  id         INTEGER PRIMARY KEY,
  sealed     TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);

-- entries_full holds the FAITHFUL ledger.Entry (NIP-44-sealed), keyed by the
-- cashctl-assigned entry id. The pure-NIP-CP held-token 9525 in pocket.db is the
-- interop/sync copy; this is cmd's source of truth so the in-memory ledger
-- buffer reloads byte-identical — the cashctl-only fields the 9525 can't carry
-- (random id, throwaway/redeem/created-by-call) and the id↔SourceEntryIDs
-- linkage are preserved. The A' Save double-writes (9525 for interop + this
-- blob for fidelity); this table is local-only precious state (risk 19).
CREATE TABLE IF NOT EXISTS entries_full (
  id         TEXT PRIMARY KEY,
  w          TEXT NOT NULL,
  sealed     TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);

-- orders_full / withdrawals_full hold the FAITHFUL ledger.Order / ledger.Withdrawal
-- (NIP-44-sealed, encrypt-to-self — Notify/OrderJSON/QuoteJSON secrets never cleartext,
-- risk 17), a separate lazily-loaded collection (NOT part of PersistLedger's
-- entry/call snapshot). order_id/call_id are handles (not secrets) kept as indexed
-- columns so FindOrder/OrderForCall are indexed; the secret fields live in sealed.
-- Per-row sealed CRUD (standalone PutOrder) + a Save staged-flush mirror the old
-- ledger's dual write-path. cashctl-private precious state (risk 19).
CREATE TABLE IF NOT EXISTS orders_full (
  order_id   TEXT PRIMARY KEY,
  call_id    TEXT NOT NULL DEFAULT '',
  sealed     TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_orders_full_call ON orders_full(call_id);

CREATE TABLE IF NOT EXISTS withdrawals_full (
  withdrawal_id TEXT PRIMARY KEY,
  call_id       TEXT NOT NULL DEFAULT '',
  sealed        TEXT NOT NULL,
  updated_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_withdrawals_full_call ON withdrawals_full(call_id);
`

// OpenCache opens (creating if absent) the decoded cache at path.
func OpenCache(path string) (*Cache, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("pocket: open cache %s: %w", path, err)
	}
	if _, err := db.Exec(cacheDDL); err != nil {
		db.Close()
		return nil, fmt.Errorf("pocket: cache schema: %w", err)
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO meta(key, value) VALUES('schema_version', ?)`, cacheSchemaVersion); err != nil {
		db.Close()
		return nil, fmt.Errorf("pocket: cache schema_version: %w", err)
	}
	return &Cache{db: db}, nil
}

// Close closes the cache database.
func (c *Cache) Close() error { return c.db.Close() }

// key is the comparator key of an entry at a given w: the fields NIP-CP
// resolution orders on. The winner across revs is the highest rev; a final state
// wins only among entries of its own rev (nipCP.Resolve / beats).
type key struct {
	rev       uint64
	final     bool
	createdAt int64
	id        string
}

// beatsSameRev orders two entries known to share a w and rev, exactly as
// nipCP's unexported beats: a final state first, then the latest created_at,
// then the lowest rumor id. (Verified against nipCP.Resolve by the fold property
// test.)
func (a key) beatsSameRev(b key) bool {
	if a.final != b.final {
		return a.final
	}
	if a.createdAt != b.createdAt {
		return a.createdAt > b.createdAt
	}
	return a.id < b.id
}

// wins reports whether a is the resolution winner over b: a higher rev always
// wins; at equal rev, beatsSameRev decides.
func (a key) wins(b key) bool {
	if a.rev != b.rev {
		return a.rev > b.rev
	}
	return a.beatsSameRev(b)
}

func keyOf(e nipCP.Entry) key {
	return key{rev: e.Rev, final: e.State.Final(), createdAt: e.RumorCreatedAt, id: e.RumorID}
}

// Fold applies one decoded entry to the cache (NIP-CP resolution, done
// incrementally). It is order-independent and idempotent: because resolution is
// a total order, re-applying an event or applying events in any order converges
// on the same winner (the fold property test cross-checks this against
// nipCP.Resolve). An entry whose state this build does not recognise is ignored,
// as Resolve ignores it. Returns whether the stored winner or its conflict flag
// changed.
func (c *Cache) Fold(e nipCP.Entry) (changed bool, err error) {
	if !e.Recognised() {
		return false, nil
	}
	tx, err := c.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	changed, err = foldTx(tx, e)
	if err != nil {
		return false, err
	}
	return changed, tx.Commit()
}

// foldTx applies one decoded entry within tx (the fold core, shared by Fold and
// the atomic Rebuild). An unrecognised state is skipped by the caller.
func foldTx(tx *sql.Tx, e nipCP.Entry) (changed bool, err error) {
	if !e.Recognised() {
		return false, nil
	}
	var (
		curRev       uint64
		curState     string
		curCreatedAt int64
		curID        string
		curConflict  bool
	)
	row := tx.QueryRow(`SELECT rev, state, rumor_created_at, rumor_id, conflict FROM entries WHERE w = ?`, e.W)
	switch err := row.Scan(&curRev, &curState, &curCreatedAt, &curID, &curConflict); err {
	case sql.ErrNoRows:
		if err := upsertEntry(tx, e, false); err != nil {
			return false, err
		}
		return true, nil
	case nil:
		// fall through
	default:
		return false, err
	}

	ck := key{rev: curRev, final: nipCP.State(curState).Final(), createdAt: curCreatedAt, id: curID}
	ek := keyOf(e)

	switch {
	case ek.rev > ck.rev:
		// New highest rev: sole winner so far, conflict resets.
		if err := upsertEntry(tx, e, false); err != nil {
			return false, err
		}
		return true, nil
	case ek.rev == ck.rev:
		// Two distinct rumors at the winning rev is a conflict (re-check with
		// cash_status). The same rumor id again is not.
		conflict := curConflict || e.RumorID != curID
		if ek.beatsSameRev(ck) {
			if err := upsertEntry(tx, e, conflict); err != nil {
				return false, err
			}
			return true, nil
		}
		if conflict != curConflict {
			if _, err := tx.Exec(`UPDATE entries SET conflict = ? WHERE w = ?`, conflict, e.W); err != nil {
				return false, err
			}
			return true, nil
		}
		return false, nil
	default:
		// Lower rev: e loses, nothing changes.
		return false, nil
	}
}

func upsertEntry(tx *sql.Tx, e nipCP.Entry, conflict bool) error {
	var expires any
	if e.ExpiresAt != nil {
		expires = *e.ExpiresAt
	}
	_, err := tx.Exec(`
INSERT INTO entries(
  w, rev, state, is_final, is_call, rumor_created_at, rumor_id, amount, chain,
  identity_type, identity_value, identity_ia, expires_at, mint, source, cash,
  checked_at, wrap_id, wrap_key, conflict)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(w) DO UPDATE SET
  rev=excluded.rev, state=excluded.state, is_final=excluded.is_final,
  is_call=excluded.is_call, rumor_created_at=excluded.rumor_created_at,
  rumor_id=excluded.rumor_id, amount=excluded.amount, chain=excluded.chain,
  identity_type=excluded.identity_type, identity_value=excluded.identity_value,
  identity_ia=excluded.identity_ia, expires_at=excluded.expires_at,
  mint=excluded.mint, source=excluded.source, cash=excluded.cash,
  checked_at=excluded.checked_at, wrap_id=excluded.wrap_id,
  wrap_key=excluded.wrap_key, conflict=excluded.conflict`,
		e.W, e.Rev, string(e.State), e.State.Final(), e.IsCall(), e.RumorCreatedAt, e.RumorID,
		e.Amount, e.Chain, e.Identity.Type, e.Identity.Value, e.Identity.IA, expires,
		e.Mint, e.Source, e.Cash, e.CheckedAt, e.WrapID, e.WrapKey, conflict)
	return err
}

// Balance sums live entries per chain (the hot read: index-only, no decrypt).
func (c *Cache) Balance() (map[string]uint64, error) {
	rows, err := c.db.Query(`SELECT chain, SUM(amount) FROM entries WHERE state = 'live' GROUP BY chain`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]uint64{}
	for rows.Next() {
		var chain string
		var sum uint64
		if err := rows.Scan(&chain, &sum); err != nil {
			return nil, err
		}
		out[chain] = sum
	}
	return out, rows.Err()
}

// Entries returns every resolved winner, as decoded nipCP.Entry values (bill
// fields; pending-call extras are a later phase). Conflicts carries the w's
// flagged for a cash_status re-check.
func (c *Cache) Entries() (entries []nipCP.Entry, conflicts []string, err error) {
	rows, err := c.db.Query(`
SELECT w, rev, state, is_call, rumor_created_at, rumor_id, amount, chain,
       identity_type, identity_value, identity_ia, expires_at, mint, source,
       cash, checked_at, wrap_id, wrap_key, conflict
FROM entries ORDER BY w`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			e        nipCP.Entry
			state    string
			isCall   bool
			expires  sql.NullInt64
			conflict bool
		)
		if err := rows.Scan(&e.W, &e.Rev, &state, &isCall, &e.RumorCreatedAt, &e.RumorID, &e.Amount, &e.Chain,
			&e.Identity.Type, &e.Identity.Value, &e.Identity.IA, &expires, &e.Mint, &e.Source,
			&e.Cash, &e.CheckedAt, &e.WrapID, &e.WrapKey, &conflict); err != nil {
			return nil, nil, err
		}
		e.State = nipCP.State(state)
		// A call entry's W is its request_id (NIP-CP); restore RequestID so
		// IsCall() holds on read-back (the richer pending params live in the
		// sealed wrap, re-Opened on demand).
		if isCall {
			e.RequestID = e.W
		}
		if expires.Valid {
			v := expires.Int64
			e.ExpiresAt = &v
		}
		entries = append(entries, e)
		if conflict {
			conflicts = append(conflicts, e.W)
		}
	}
	return entries, conflicts, rows.Err()
}

// Rebuild drops the decoded projection and re-folds all entries from scratch
// (e.g. after a schema bump, or to recover a corrupt cache). It is ATOMIC — the
// delete and every fold run in one transaction — so a crash mid-rebuild leaves
// the previous projection intact rather than an empty cache that nothing would
// auto-recover. Because the fold is order-independent, the result equals the
// incrementally-maintained cache.
func (c *Cache) Rebuild(all []nipCP.Entry) error {
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM entries`); err != nil {
		return err
	}
	for _, e := range all {
		if _, err := foldTx(tx, e); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SchemaVersion is the decoded cache's stored schema version (0 if unset).
func (c *Cache) SchemaVersion() (int, error) {
	v, err := c.metaGet("schema_version")
	if err != nil || v == "" {
		return 0, err
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("pocket: schema_version %q: %w", v, err)
	}
	return n, nil
}

func (c *Cache) setSchemaVersion(v int) error { return c.metaSet("schema_version", strconv.Itoa(v)) }

// State returns the resolved state of w (and whether w is known). The settle
// driver reads it before applying a reply so a call is reconciled exactly once:
// once its pending entry is done, State reports done and the apply is skipped.
func (c *Cache) State(w string) (state string, known bool, err error) {
	err = c.db.QueryRow(`SELECT state FROM entries WHERE w = ?`, w).Scan(&state)
	switch err {
	case sql.ErrNoRows:
		return "", false, nil
	case nil:
		return state, true, nil
	default:
		return "", false, fmt.Errorf("pocket: state %s: %w", w, err)
	}
}

// OpenCalls lists the w (request_id) of every still-open pending call (a call
// entry in state pending). The settle driver works through these.
func (c *Cache) OpenCalls() ([]string, error) {
	rows, err := c.db.Query(`SELECT w FROM entries WHERE is_call = 1 AND state = ?`, string(nipCP.StatePending))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var w string
		if err := rows.Scan(&w); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// currentRev returns the resolved rev of w (and whether w is known) — what the
// A' Save reads to write a changed bill at the next rev (so the fold supersedes
// the prior one by rev rather than flagging a same-rev conflict).
func (c *Cache) currentRev(w string) (rev uint64, known bool, err error) {
	err = c.db.QueryRow(`SELECT rev FROM entries WHERE w = ?`, w).Scan(&rev)
	switch err {
	case sql.ErrNoRows:
		return 0, false, nil
	case nil:
		return rev, true, nil
	default:
		return 0, false, fmt.Errorf("pocket: rev %s: %w", w, err)
	}
}

// ---- sealed pending_calls (cashctl's in-flight calls, encrypt-to-self) ----

// PutSealedCall upserts the NIP-44-sealed blob of one ledger.Call by request id.
func (c *Cache) PutSealedCall(requestID, sealed string, updatedAt int64) error {
	_, err := c.db.Exec(`
INSERT INTO pending_calls(request_id, sealed, updated_at) VALUES(?,?,?)
ON CONFLICT(request_id) DO UPDATE SET sealed=excluded.sealed, updated_at=excluded.updated_at`,
		requestID, sealed, updatedAt)
	return err
}

// DeleteSealedCall removes a call's sealed blob (a call never sent, dropped).
func (c *Cache) DeleteSealedCall(requestID string) error {
	_, err := c.db.Exec(`DELETE FROM pending_calls WHERE request_id = ?`, requestID)
	return err
}

// SealedCalls returns every stored (request_id, sealed) pending-call blob.
func (c *Cache) SealedCalls() (map[string]string, error) {
	rows, err := c.db.Query(`SELECT request_id, sealed FROM pending_calls`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, sealed string
		if err := rows.Scan(&id, &sealed); err != nil {
			return nil, err
		}
		out[id] = sealed
	}
	return out, rows.Err()
}

// ---- sealed handoffs (recipient tokens, encrypt-to-self) ----

// PutSealedHandoff inserts (id==0) or updates a handoff's sealed blob, returning
// the row id. A recipient token must never be lost, so it is persisted durably.
func (c *Cache) PutSealedHandoff(id int64, sealed string, updatedAt int64) (int64, error) {
	if id == 0 {
		res, err := c.db.Exec(`INSERT INTO handoffs(sealed, updated_at) VALUES(?,?)`, sealed, updatedAt)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	_, err := c.db.Exec(`
INSERT INTO handoffs(id, sealed, updated_at) VALUES(?,?,?)
ON CONFLICT(id) DO UPDATE SET sealed=excluded.sealed, updated_at=excluded.updated_at`, id, sealed, updatedAt)
	return id, err
}

// SealedHandoffs returns every stored (id, sealed) handoff blob.
func (c *Cache) SealedHandoffs() (map[int64]string, error) {
	rows, err := c.db.Query(`SELECT id, sealed FROM handoffs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var sealed string
		if err := rows.Scan(&id, &sealed); err != nil {
			return nil, err
		}
		out[id] = sealed
	}
	return out, rows.Err()
}

// ---- faithful sealed ledger.Entry (cmd's source of truth, encrypt-to-self) ----

// PutSealedEntry upserts the NIP-44-sealed faithful ledger.Entry by its id.
func (c *Cache) PutSealedEntry(id, w, sealed string, updatedAt int64) error {
	_, err := c.db.Exec(`
INSERT INTO entries_full(id, w, sealed, updated_at) VALUES(?,?,?,?)
ON CONFLICT(id) DO UPDATE SET w=excluded.w, sealed=excluded.sealed, updated_at=excluded.updated_at`,
		id, w, sealed, updatedAt)
	return err
}

// DeleteSealedEntry removes a faithful entry blob by id.
func (c *Cache) DeleteSealedEntry(id string) error {
	_, err := c.db.Exec(`DELETE FROM entries_full WHERE id = ?`, id)
	return err
}

// SealedEntries returns every stored (id, sealed) faithful-entry blob.
func (c *Cache) SealedEntries() (map[string]string, error) {
	rows, err := c.db.Query(`SELECT id, sealed FROM entries_full`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, sealed string
		if err := rows.Scan(&id, &sealed); err != nil {
			return nil, err
		}
		out[id] = sealed
	}
	return out, rows.Err()
}

// ---- sealed orders / withdrawals (cashctl-private funds records, encrypt-to-self) ----

// SealedOrderRow / SealedWithdrawalRow are the stored handle+ciphertext of one
// ledger.Order / ledger.Withdrawal. The handles (order/call/withdrawal ids) are
// not secrets; the Order/Withdrawal's secret fields live inside Sealed.
type SealedOrderRow struct{ OrderID, CallID, Sealed string }
type SealedWithdrawalRow struct{ WithdrawalID, CallID, Sealed string }

// PutSealedOrder upserts a sealed order; created_at is preserved on conflict.
func (c *Cache) PutSealedOrder(orderID, callID, sealed string, createdAt, updatedAt int64) error {
	_, err := c.db.Exec(`
INSERT INTO orders_full(order_id, call_id, sealed, created_at, updated_at) VALUES(?,?,?,?,?)
ON CONFLICT(order_id) DO UPDATE SET call_id=excluded.call_id, sealed=excluded.sealed, updated_at=excluded.updated_at`,
		orderID, callID, sealed, createdAt, updatedAt)
	return err
}

// DeleteSealedOrder removes an order by id.
func (c *Cache) DeleteSealedOrder(orderID string) error {
	_, err := c.db.Exec(`DELETE FROM orders_full WHERE order_id = ?`, orderID)
	return err
}

// SealedOrders returns every sealed order, oldest first (created_at, rowid).
func (c *Cache) SealedOrders() ([]SealedOrderRow, error) {
	return scanSealedOrders(c.db.Query(`SELECT order_id, call_id, sealed FROM orders_full ORDER BY created_at, rowid`))
}

// SealedOrderByID / SealedOrderByCall look one up; ok=false on no row.
func (c *Cache) SealedOrderByID(orderID string) (SealedOrderRow, bool, error) {
	return oneSealedOrder(c.db.Query(`SELECT order_id, call_id, sealed FROM orders_full WHERE order_id = ?`, orderID))
}
func (c *Cache) SealedOrderByCall(callID string) (SealedOrderRow, bool, error) {
	return oneSealedOrder(c.db.Query(`SELECT order_id, call_id, sealed FROM orders_full WHERE call_id = ?`, callID))
}

func scanSealedOrders(rows *sql.Rows, qerr error) ([]SealedOrderRow, error) {
	if qerr != nil {
		return nil, qerr
	}
	defer rows.Close()
	out := []SealedOrderRow{}
	for rows.Next() {
		var r SealedOrderRow
		if err := rows.Scan(&r.OrderID, &r.CallID, &r.Sealed); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func oneSealedOrder(rows *sql.Rows, qerr error) (SealedOrderRow, bool, error) {
	all, err := scanSealedOrders(rows, qerr)
	if err != nil || len(all) == 0 {
		return SealedOrderRow{}, false, err
	}
	return all[0], true, nil
}

// PutSealedWithdrawal upserts a sealed withdrawal.
func (c *Cache) PutSealedWithdrawal(withdrawalID, callID, sealed string, updatedAt int64) error {
	_, err := c.db.Exec(`
INSERT INTO withdrawals_full(withdrawal_id, call_id, sealed, updated_at) VALUES(?,?,?,?)
ON CONFLICT(withdrawal_id) DO UPDATE SET call_id=excluded.call_id, sealed=excluded.sealed, updated_at=excluded.updated_at`,
		withdrawalID, callID, sealed, updatedAt)
	return err
}

// SealedWithdrawals returns every sealed withdrawal, oldest first (rowid).
func (c *Cache) SealedWithdrawals() ([]SealedWithdrawalRow, error) {
	return scanSealedWithdrawals(c.db.Query(`SELECT withdrawal_id, call_id, sealed FROM withdrawals_full ORDER BY rowid`))
}

func (c *Cache) SealedWithdrawalByID(withdrawalID string) (SealedWithdrawalRow, bool, error) {
	return oneSealedWithdrawal(c.db.Query(`SELECT withdrawal_id, call_id, sealed FROM withdrawals_full WHERE withdrawal_id = ?`, withdrawalID))
}
func (c *Cache) SealedWithdrawalByCall(callID string) (SealedWithdrawalRow, bool, error) {
	return oneSealedWithdrawal(c.db.Query(`SELECT withdrawal_id, call_id, sealed FROM withdrawals_full WHERE call_id = ?`, callID))
}

func scanSealedWithdrawals(rows *sql.Rows, qerr error) ([]SealedWithdrawalRow, error) {
	if qerr != nil {
		return nil, qerr
	}
	defer rows.Close()
	out := []SealedWithdrawalRow{}
	for rows.Next() {
		var r SealedWithdrawalRow
		if err := rows.Scan(&r.WithdrawalID, &r.CallID, &r.Sealed); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func oneSealedWithdrawal(rows *sql.Rows, qerr error) (SealedWithdrawalRow, bool, error) {
	all, err := scanSealedWithdrawals(rows, qerr)
	if err != nil || len(all) == 0 {
		return SealedWithdrawalRow{}, false, err
	}
	return all[0], true, nil
}

// ---- atomic faithful-ledger persist (the A' Save backend) ----

// SealedEntry/SealedCall/SealedHandoff are the already-sealed rows PersistLedger
// writes — the caller seals the ledger.Entry/Call/Handoff (sealedcall.go), this
// just stores the ciphertext.
type SealedEntry struct{ ID, W, Sealed string }
type SealedCall struct{ RequestID, Sealed string }
type SealedHandoff struct {
	ID     int64
	Sealed string
}

// StoreVersion is the whole-store optimistic-concurrency token. PersistLedger
// bumps it on every successful save; OpenLedgerStore captures it at load and
// passes it back, so a save whose caller loaded an older version (another cashctl
// process wrote in between) is refused with ledger.ErrConcurrentUpdate instead of
// clobbering it. 0 (default/absent) is a fresh store.
func (c *Cache) StoreVersion() (int64, error) {
	v, err := c.metaGet("store_version")
	if err != nil || v == "" {
		return 0, err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("pocket: store_version %q: %w", v, err)
	}
	return n, nil
}

// PersistLedger writes the full faithful ledger state (entries_full, pending_calls,
// handoffs) in ONE transaction — matching the old cashctl.db ledger's atomic
// whole-table Save, so a crash mid-save leaves the previous state intact rather
// than a half-written mix (e.g. a source marked spent with its pending call not
// yet durable). It is a full replace of the current state: delete-all then insert
// the caller's current rows, atomically.
//
// Multi-process concurrency (the plan's #14 — adversarial-confirmed): the delete-all
// is guarded by an optimistic-concurrency compare-and-set on store_version, read
// INSIDE the tx so check-and-bump is atomic. If another cashctl process wrote since
// this store loaded (disk version != expectedVersion), the save is refused with
// ledger.ErrConcurrentUpdate and NOTHING is overwritten — closing the stranding
// bug where a stale process's blind delete erased another's written-ahead call.
// A whole-store counter is coarser than the old ledger's per-call version but
// strictly stronger: ANY concurrent write is caught, so it subsumes both the old
// per-call CAS and refuseForeignOpenCall (R-CP-10) in one guard. Returns the new
// store version on success.
func (c *Cache) PersistLedger(entries []SealedEntry, calls []SealedCall, handoffs []SealedHandoff, stagedOrders []SealedOrderRow, stagedWithdrawals []SealedWithdrawalRow, now, expectedVersion int64) (int64, error) {
	tx, err := c.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var cur int64
	var curStr string
	switch err := tx.QueryRow(`SELECT value FROM meta WHERE key = 'store_version'`).Scan(&curStr); err {
	case sql.ErrNoRows:
		cur = 0
	case nil:
		if cur, err = strconv.ParseInt(curStr, 10, 64); err != nil {
			return 0, fmt.Errorf("pocket: store_version %q: %w", curStr, err)
		}
	default:
		return 0, err
	}
	if cur != expectedVersion {
		return 0, fmt.Errorf("%w (pocket store at v%d, this command loaded v%d)", ledger.ErrConcurrentUpdate, cur, expectedVersion)
	}

	for _, t := range []string{"entries_full", "pending_calls", "handoffs"} {
		if _, err := tx.Exec(`DELETE FROM ` + t); err != nil {
			return 0, err
		}
	}
	for _, e := range entries {
		if _, err := tx.Exec(`INSERT INTO entries_full(id, w, sealed, updated_at) VALUES(?,?,?,?)`, e.ID, e.W, e.Sealed, now); err != nil {
			return 0, err
		}
	}
	for _, cl := range calls {
		if _, err := tx.Exec(`INSERT INTO pending_calls(request_id, sealed, updated_at) VALUES(?,?,?)`, cl.RequestID, cl.Sealed, now); err != nil {
			return 0, err
		}
	}
	for _, h := range handoffs {
		if _, err := tx.Exec(`INSERT INTO handoffs(id, sealed, updated_at) VALUES(?,?,?)`, h.ID, h.Sealed, now); err != nil {
			return 0, err
		}
	}
	// Orders/withdrawals are a SEPARATE collection (not the delete-all snapshot):
	// staged ones are UPSERTed in this same tx so an order/withdrawal and the call
	// that places it land atomically (mirrors the old ledger's ledger.go:732).
	for _, o := range stagedOrders {
		if _, err := tx.Exec(`INSERT INTO orders_full(order_id, call_id, sealed, created_at, updated_at) VALUES(?,?,?,?,?)
			ON CONFLICT(order_id) DO UPDATE SET call_id=excluded.call_id, sealed=excluded.sealed, updated_at=excluded.updated_at`,
			o.OrderID, o.CallID, o.Sealed, now, now); err != nil {
			return 0, err
		}
	}
	for _, w := range stagedWithdrawals {
		if _, err := tx.Exec(`INSERT INTO withdrawals_full(withdrawal_id, call_id, sealed, updated_at) VALUES(?,?,?,?)
			ON CONFLICT(withdrawal_id) DO UPDATE SET call_id=excluded.call_id, sealed=excluded.sealed, updated_at=excluded.updated_at`,
			w.WithdrawalID, w.CallID, w.Sealed, now); err != nil {
			return 0, err
		}
	}

	newV := expectedVersion + 1
	if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES('store_version', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, strconv.FormatInt(newV, 10)); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return newV, nil
}
