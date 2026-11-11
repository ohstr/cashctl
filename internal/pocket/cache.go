package pocket

import (
	"database/sql"
	"fmt"
	"strconv"

	_ "modernc.org/sqlite" // pure-Go sqlite driver, as internal/store uses

	"github.com/ohstr/nmilat/nipCP"
)

// cacheSchemaVersion is bumped whenever the decoded-cache schema changes; a
// mismatch forces a rebuild from pocket.db (the cache is derived, never
// authoritative). See the plan's risk 15 (forward-compat).
const cacheSchemaVersion = 1

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
