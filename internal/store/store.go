// Package store manages cashctl's single local SQLite database
// (cashctl.db), replacing the three flat JSON files (identity.json,
// connections.json, ledger.json) v0.0.1 used. No migration path: this is
// a clean replacement, not an upgrade — v0.0.1 has no real installed base
// to preserve continuity for (docs/ux-review.md Part 3).
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

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
	bearer_secret              TEXT,
	connection_key_platform    TEXT,
	connection_key_external_id TEXT,
	attestation_event_id       TEXT,
	ia_pubkey                  TEXT
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

// Open opens (creating and migrating if needed) cashctl's single local
// database, cashctl.db, under appdir.Dir(). Callers own the returned *sql.DB
// and must Close it. 0600: it can hold a plaintext identity privkey and
// bearer-mode spending secrets — same sensitivity ledger.json and
// identity.json always had (see ledger.Entry.BearerSecret's own doc
// comment).
func Open() (*sql.DB, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("cashctl.db schema migration: %w", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}
