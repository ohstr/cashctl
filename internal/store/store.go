// Package store manages cashctl's single local SQLite database
// (cashctl.db), replacing the three flat JSON files (identity.json,
// connections.json, ledger.json) v0.0.1 used. No migration path: this is
// a clean replacement, not an upgrade — v0.0.1 has no real installed base
// to preserve continuity for (docs/ux-review.md Part 3).
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/ohstr/cashctl/internal/appdir"
)

// schema is applied with CREATE TABLE/INDEX IF NOT EXISTS on every Open,
// so it's always safe to run against an existing database.
const schema = `
CREATE TABLE IF NOT EXISTS identity (
	source   TEXT NOT NULL,
	npub     TEXT,
	label    TEXT,
	priv_hex TEXT
);

CREATE TABLE IF NOT EXISTS connections (
	name                     TEXT PRIMARY KEY,
	value                    TEXT NOT NULL,
	added_at                 TEXT NOT NULL,
	last_known_balance_mloki INTEGER,
	last_known_balance_at    TEXT,
	is_default               INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS entries (
	id                         TEXT PRIMARY KEY,
	token                      TEXT NOT NULL UNIQUE,
	wallet_pubkey              TEXT NOT NULL,
	minter_pubkey              TEXT,
	secret                     TEXT NOT NULL,
	relay_urls                 TEXT,
	identity_required          INTEGER,
	amount_millis              INTEGER,
	received_at                TEXT NOT NULL,
	verified                   INTEGER NOT NULL,
	status                     TEXT NOT NULL,
	cash_secret              TEXT,
	connection_key_platform    TEXT,
	connection_key_external_id TEXT,
	attestation_event_id       TEXT,
	ia_pubkey                  TEXT,
	pending_cash_secret      TEXT,
	expires_at                 INTEGER,
	cash_protection          TEXT
);
CREATE INDEX IF NOT EXISTS idx_entries_status_minter ON entries(status, minter_pubkey);

CREATE TABLE IF NOT EXISTS history (
	id     INTEGER PRIMARY KEY AUTOINCREMENT,
	at     TEXT NOT NULL,
	action TEXT NOT NULL,
	detail TEXT NOT NULL
);
`

// Path returns cashctl.db's location under appdir.Dir(), without opening
// it — mainly useful for tests asserting on-disk file permissions.
func Path() (string, error) {
	dir, err := appdir.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cashctl.db"), nil
}

// sqliteDSN builds the SQLite URI for the database at path. The driver
// hands "file:" DSNs to SQLite in URI mode, where '?' starts the query
// string, '#' starts a fragment and '%' introduces an escape — so a
// --config-dir containing any of them (a perfectly legal directory name)
// used to have its path silently truncated or mangled: the database was
// created at a different location than the one chmod'd afterwards, with
// default permissions, and init failed. Only those three characters need
// escaping; everything else in a path is literal in URI mode.
func sqliteDSN(path string) string {
	esc := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	return "file:" + esc + "?_pragma=busy_timeout(5000)"
}

// Open opens (creating and migrating if needed) cashctl's single local
// database, cashctl.db, under appdir.Dir(). Callers own the returned *sql.DB
// and must Close it. 0600: it can hold a plaintext identity privkey and
// cash-mode spending secrets — same sensitivity ledger.json and
// identity.json always had (see ledger.Entry.CashSecret's own doc
// comment).
func Open() (*sql.DB, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}

	// busy_timeout(5000): SQLite's own internal busy-handler, not a
	// hand-rolled one — on SQLITE_BUSY it retries with backoff at the C
	// level for up to 5s before ever returning the error to Go, which is
	// what ledger.Save's own Go-level withBusyRetry then wraps as a second,
	// coarser line of defense (see its own doc comment). Without this, two
	// `cashctl` processes committing within the same few milliseconds could
	// fail outright — confirmed live and under `go test -race`, where the
	// race detector's own overhead widens that window enough to turn a rare
	// case into a routine one.
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("cashctl.db schema migration: %w", err)
	}
	// Before addColumnsIfMissing, never after: on a pre-rename DB the add
	// step would otherwise create an empty cash_* column right next to the
	// bearer_* one still holding the data, and the rename below would then
	// have nowhere to go.
	if err := renameColumnsIfNeeded(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("cashctl.db schema migration: %w", err)
	}
	if err := addColumnsIfMissing(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("cashctl.db schema migration: %w", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// addedColumns lists every column added to an existing table after that
// table's own CREATE TABLE IF NOT EXISTS above first shipped — that clause
// is a no-op against a database from before the column existed, so it has
// to be ALTERed in separately. ALTER TABLE ADD COLUMN is instant and safe
// here (nullable, no default, so SQLite never rewrites existing rows) —
// nothing more elaborate than this loop is needed unless a future column
// needs backfilling.
var addedColumns = []struct{ table, column, ddl string }{
	// pending_cash_secret: a rekey/protect step generates the new cash
	// secret client-side before ever placing the wire call (see
	// cmd/receive_secure.go's own doc comment) — persisting it here first
	// means a kill mid-call loses at most a retry, never the secret itself.
	{"entries", "pending_cash_secret", `ALTER TABLE entries ADD COLUMN pending_cash_secret TEXT`},
	// expires_at: the Hub-side redemption deadline (nipcash.CheckClaimResult
	// .ExpiresAt, unix seconds), cached at receive time so `balance` can
	// exclude an expired held token from the total without a live re-check
	// per token on every call — before this column existed there was
	// nowhere to persist it, so an expired-but-still-`held` token kept
	// counting as spendable money forever.
	{"entries", "expires_at", `ALTER TABLE entries ADD COLUMN expires_at INTEGER`},
	// cash_protection: "" (unset/n/a), "shared" (a cash-mode entry's
	// secret is still the one embedded in the original token/gift string
	// — anyone else shown it can spend it too), or "protected" (re-keyed
	// by protectCashReceipt/protectRekeyOnly, exclusively known to this
	// ledger). Before this column existed there was no way to tell the
	// two apart after the fact — a declined/failed protect left the same
	// CashSecret shape as a genuinely re-keyed one.
	{"entries", "cash_protection", `ALTER TABLE entries ADD COLUMN cash_protection TEXT`},
}

// addColumnsIfMissing applies addedColumns' migrations exactly once each,
// keyed off PRAGMA table_info rather than sniffing the "duplicate column"
// error text — errors.Is has nothing to match here (modernc.org/sqlite
// doesn't export a typed error for it), and pragma_table_info is the
// direct, unambiguous way to ask "does this column exist" instead.
func addColumnsIfMissing(db *sql.DB) error {
	present := map[[2]string]bool{}
	for _, c := range addedColumns {
		rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, c.table)
		if err != nil {
			return fmt.Errorf("inspecting table %s: %w", c.table, err)
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				_ = rows.Close()
				return err
			}
			present[[2]string{c.table, name}] = true
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
	}
	for _, c := range addedColumns {
		if present[[2]string{c.table, c.column}] {
			continue
		}
		if _, err := db.Exec(c.ddl); err != nil {
			return fmt.Errorf("adding %s.%s: %w", c.table, c.column, err)
		}
	}
	return nil
}

// renamedColumns lists every column renamed in place on an existing table.
// NIP-CASH renamed "bearer" mode to "cash mode" (see nipcash.CashTarget),
// so the three ledger columns that carried the old spelling move to the new
// one. The stored values are untouched: a secret is still the same secret,
// and cash_protection still holds "shared"/"protected".
var renamedColumns = []struct{ table, from, to string }{
	{"entries", "bearer_secret", "cash_secret"},
	{"entries", "pending_bearer_secret", "pending_cash_secret"},
	{"entries", "bearer_protection", "cash_protection"},
}

// renameColumnsIfNeeded applies renamedColumns to a ledger written before
// the rename. It deliberately does NOT copy the DB aside first: cashctl.db
// holds live spending secrets, so a backup would duplicate them on disk,
// and the whole rename runs in one transaction anyway — it either lands
// completely or not at all.
//
// Downgrading is not supported, and fails loudly rather than quietly: an
// older cashctl opening a migrated ledger stops at "no such column:
// bearer_secret" instead of reading a half-renamed row. It could not talk
// to a renamed Hub regardless — the wire moved in the same release.
//
// BEGIN IMMEDIATE on a dedicated connection, rather than a deferred
// database/sql transaction: two cashctl processes may open the same ledger
// at once, and taking the write lock up front means the second one waits on
// SQLite's own busy handler (busy_timeout in the DSN) and then re-reads
// pragma_table_info under that lock, instead of racing a rename that has
// already happened and failing on the ALTER.
func renameColumnsIfNeeded(db *sql.DB) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("taking the ledger write lock: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		}
	}()

	for _, c := range renamedColumns {
		hasOld, err := columnExists(ctx, conn, c.table, c.from)
		if err != nil {
			return err
		}
		hasNew, err := columnExists(ctx, conn, c.table, c.to)
		if err != nil {
			return err
		}

		switch {
		case hasOld && !hasNew:
			if _, err := conn.ExecContext(ctx, fmt.Sprintf(
				`ALTER TABLE %s RENAME COLUMN %s TO %s`, c.table, c.from, c.to)); err != nil {
				return fmt.Errorf("renaming %s.%s to %s: %w", c.table, c.from, c.to, err)
			}
		case hasOld && hasNew:
			// An older cashctl opened this already-migrated ledger and its
			// own addColumnsIfMissing re-added the empty old column. Fold
			// anything it wrote back in (COALESCE keeps the new column's
			// value wherever it has one) and drop the stale duplicate, so
			// the next run sees a single, unambiguous column.
			if _, err := conn.ExecContext(ctx, fmt.Sprintf(
				`UPDATE %s SET %s = COALESCE(%s, %s)`, c.table, c.to, c.to, c.from)); err != nil {
				return fmt.Errorf("merging %s.%s into %s: %w", c.table, c.from, c.to, err)
			}
			if _, err := conn.ExecContext(ctx, fmt.Sprintf(
				`ALTER TABLE %s DROP COLUMN %s`, c.table, c.from)); err != nil {
				return fmt.Errorf("dropping stale %s.%s: %w", c.table, c.from, err)
			}
		}
		// !hasOld: a fresh DB (the schema above already used the new name),
		// one already migrated, or one so old it never had this column at
		// all — addColumnsIfMissing adds the new name next.
	}

	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("committing the ledger column rename: %w", err)
	}
	committed = true
	return nil
}

// columnExists asks SQLite directly rather than sniffing an error string,
// for the same reason addColumnsIfMissing does: modernc.org/sqlite exports
// no typed error for "no such column".
func columnExists(ctx context.Context, conn *sql.Conn, table, column string) (bool, error) {
	rows, err := conn.QueryContext(ctx,
		`SELECT 1 FROM pragma_table_info(?) WHERE name = ?`, table, column)
	if err != nil {
		return false, fmt.Errorf("inspecting %s.%s: %w", table, column, err)
	}
	defer func() { _ = rows.Close() }()

	found := rows.Next()
	if err := rows.Err(); err != nil {
		return false, err
	}
	return found, nil
}
