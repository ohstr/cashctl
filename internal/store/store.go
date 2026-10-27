// Package store manages cashctl's single local SQLite database, cashctl.db.
//
// The schema is version 2, created fresh: there is no migration from any
// earlier cashctl, and a database without schema_version 2 is refused, never
// read (nothing earlier was in production).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/ohstr/cashctl/internal/appdir"
)

// SchemaVersion is the only schema this cashctl reads or writes.
const SchemaVersion = "2"

// schema is created once, in an empty database, together with
// meta(schema_version).
const schema = `
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);

CREATE TABLE device (
	id          INTEGER PRIMARY KEY CHECK (id = 1),
	device_id   TEXT NOT NULL,
	fingerprint TEXT NOT NULL,
	created_at  INTEGER NOT NULL
);

CREATE TABLE identity (
	source   TEXT NOT NULL,
	npub     TEXT,
	label    TEXT,
	priv_hex TEXT
);

CREATE TABLE connections (
	name                      TEXT PRIMARY KEY,
	kind                      TEXT NOT NULL CHECK (kind IN ('nwc', 'circle')),
	value                     TEXT,
	added_at                  TEXT NOT NULL,
	is_default                INTEGER NOT NULL DEFAULT 0,
	last_known_balance_millis INTEGER,
	last_known_balance_at     TEXT
);

CREATE TABLE entries (
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
	cash_secret                TEXT,
	connection_key_platform    TEXT,
	connection_key_external_id TEXT,
	attestation_event_id       TEXT,
	ia_pubkey                  TEXT,
	expires_at                 INTEGER,
	cash_protection            TEXT,
	throwaway_key              TEXT,
	pending_throwaway_key      TEXT,
	redeem_fee_millis          INTEGER,
	zap_event                  TEXT,
	created_by_call            TEXT,
	version                    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_entries_status_minter ON entries(status, minter_pubkey);

CREATE TABLE pending_calls (
	call_id           TEXT PRIMARY KEY,
	request_id        TEXT NOT NULL UNIQUE,
	method            TEXT NOT NULL,
	purpose           TEXT NOT NULL,
	deadline          INTEGER NOT NULL,
	sent_h            INTEGER NOT NULL,
	hub_identity      TEXT NOT NULL,
	target            TEXT NOT NULL,
	target_credential TEXT NOT NULL,
	device_id         TEXT NOT NULL,
	amount_millis     INTEGER NOT NULL,
	chain             TEXT NOT NULL,
	authorizer_kind   TEXT NOT NULL,
	authorizer_pubkey TEXT,
	authorizer_secret TEXT,
	source_entry_ids  TEXT NOT NULL,
	params_json       TEXT NOT NULL,
	replaces          TEXT,
	attempt           INTEGER NOT NULL DEFAULT 0,
	state             TEXT NOT NULL,
	detail            TEXT,
	first_error       TEXT,
	first_detail      TEXT,
	saw_reused        INTEGER NOT NULL DEFAULT 0,
	retry_after       INTEGER,
	created_at        INTEGER NOT NULL,
	updated_at        INTEGER NOT NULL,
	result_json       TEXT,
	version           INTEGER NOT NULL DEFAULT 0,
	zap_context       TEXT
);

CREATE TABLE call_secrets (
	call_id    TEXT NOT NULL REFERENCES pending_calls(call_id),
	role       TEXT NOT NULL,
	secret     TEXT NOT NULL,
	commitment TEXT NOT NULL,
	PRIMARY KEY (call_id, role)
);

CREATE TABLE derived_ids (
	request_id        TEXT PRIMARY KEY,
	parent_request_id TEXT NOT NULL,
	deadline          INTEGER,
	keep_until        INTEGER NOT NULL
);

CREATE TABLE history (
	id     INTEGER PRIMARY KEY AUTOINCREMENT,
	at     TEXT NOT NULL,
	action TEXT NOT NULL,
	detail TEXT NOT NULL
);

CREATE TABLE handoffs (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	at            TEXT NOT NULL,
	kind          TEXT NOT NULL,
	handoff       TEXT NOT NULL,
	amount_millis INTEGER NOT NULL,
	target        TEXT NOT NULL,
	wallet_pubkey TEXT NOT NULL,
	expires_at    INTEGER,
	status        TEXT NOT NULL,
	checked_at    TEXT
);

CREATE TABLE joins (
	connection_pubkey TEXT PRIMARY KEY,
	connection_secret TEXT NOT NULL,
	hub_pubkey        TEXT NOT NULL,
	member_pubkey     TEXT NOT NULL,
	hub_connection    TEXT NOT NULL,
	hub_identity      TEXT NOT NULL,
	chain             TEXT NOT NULL,
	hub_label         TEXT NOT NULL,
	conn_verified     INTEGER NOT NULL,
	deadline          INTEGER NOT NULL,
	device_id         TEXT NOT NULL,
	terms             TEXT,
	state             TEXT NOT NULL,
	detail            TEXT,
	retry_after       INTEGER,
	wallet_name       TEXT,
	lookup_answered   INTEGER NOT NULL DEFAULT 0,
	alt_lookup_tried  INTEGER NOT NULL DEFAULT 0,
	created_at        INTEGER NOT NULL,
	updated_at        INTEGER NOT NULL
);

CREATE TABLE orders (
	order_id        TEXT PRIMARY KEY,
	call_id         TEXT NOT NULL,
	notify          TEXT NOT NULL,
	payment         TEXT NOT NULL,
	amount_millis   INTEGER NOT NULL,
	recipient_kind  TEXT NOT NULL,
	recipient       TEXT NOT NULL,
	hub_identity    TEXT NOT NULL,
	chain           TEXT NOT NULL,
	network         TEXT,
	conn_verified   INTEGER NOT NULL,
	state           TEXT NOT NULL,
	saw_progress    INTEGER NOT NULL DEFAULT 0,
	terms_changed   INTEGER NOT NULL DEFAULT 0,
	pay_by          INTEGER,
	order_json      TEXT,
	accepted_relays TEXT,
	created_at      INTEGER NOT NULL,
	checked_at      INTEGER,
	finished_at     INTEGER
);

CREATE TABLE discover_relays (url TEXT PRIMARY KEY, added_at INTEGER NOT NULL);
CREATE TABLE discover_curators (pubkey TEXT PRIMARY KEY, added_at INTEGER NOT NULL);
CREATE TABLE discover_state (key TEXT PRIMARY KEY, value BLOB NOT NULL, updated_at INTEGER NOT NULL);

CREATE TABLE trusted_minters (
	minter_xonly TEXT NOT NULL,
	chain        TEXT NOT NULL,
	added_at     INTEGER NOT NULL,
	PRIMARY KEY (minter_xonly, chain)
);

CREATE TABLE withdraw_quotes (
	quote_handle  TEXT PRIMARY KEY,
	entry_id      TEXT NOT NULL,
	wallet_pubkey TEXT NOT NULL,
	hub_identity  TEXT NOT NULL,
	address       TEXT NOT NULL,
	quote_json    TEXT NOT NULL,
	quote_id      TEXT NOT NULL,
	send_before   INTEGER,
	used          INTEGER NOT NULL DEFAULT 0,
	created_at    INTEGER NOT NULL
);

CREATE TABLE withdrawals (
	withdrawal_id      TEXT PRIMARY KEY,
	call_id            TEXT NOT NULL,
	source_entry_id    TEXT NOT NULL,
	wallet_pubkey      TEXT NOT NULL,
	hub_identity       TEXT NOT NULL,
	address            TEXT,
	quote_json         TEXT,
	quote_id           TEXT,
	amount_millis      INTEGER NOT NULL,
	network_fee_millis INTEGER,
	hub_fee_millis     INTEGER,
	fee_millis         INTEGER NOT NULL,
	payout_millis      INTEGER NOT NULL,
	sent_by            INTEGER NOT NULL,
	holds_until        INTEGER,
	send_before        INTEGER,
	notify             TEXT NOT NULL,
	accepted_relays    TEXT,
	state              TEXT NOT NULL,
	payout_json        TEXT,
	remint_entry_id    TEXT,
	quote_expired_seen INTEGER NOT NULL DEFAULT 0,
	confirmed_at       INTEGER,
	checked_at         INTEGER,
	finished_at        INTEGER
);

CREATE TABLE unverified_confirmations (
	conn_sha256  TEXT PRIMARY KEY,
	hrp          TEXT NOT NULL,
	hub_identity TEXT NOT NULL,
	reason       TEXT NOT NULL,
	confirmed_at INTEGER NOT NULL,
	via          TEXT NOT NULL
);
`

// ErrOldDatabase is a cashctl.db written by an earlier cashctl. It is never
// read: the caller moves it aside.
type ErrOldDatabase struct{ Path string }

func (e *ErrOldDatabase) Error() string {
	return fmt.Sprintf("%s was written by an earlier cashctl; move it aside (nothing in it is read)", e.Path)
}

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
// journal_mode is deliberately NOT set here — see enableWAL, which does it after
// the connection is up precisely because a DSN pragma that fails takes the whole
// connection down with it.
func sqliteDSN(path string) string {
	esc := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	return "file:" + esc + "?_pragma=busy_timeout(5000)"
}

// enableWAL switches the database to WAL, best effort, reporting nothing.
//
// Why WAL: the default rollback journal makes a writer block readers and a reader
// block the writer, which is this ledger's exact access pattern — Load, a network
// round trip, then Save, on two independent connections, plus two BEGIN IMMEDIATE
// migrations on every Open. 25d70a7 (two processes both adding a column) and the
// -race-widened SQLITE_BUSY window in TestOpen_RenameConcurrent both came out of
// that contention.
//
// Why best effort, and why not in the DSN — this is the part that was learned the
// hard way. Changing journal_mode needs an exclusive lock, and SQLite answers a
// WAL transition it cannot take with SQLITE_BUSY *immediately*: busy_timeout does
// not apply to it. Set as a DSN pragma, that failure took the connection itself
// down and surfaced on the first statement as
// "cashctl.db schema migration: database is locked (5) (SQLITE_BUSY)". Five
// concurrent `cashctl init` runs on one fresh directory then failed about once in
// fifty, measured, against zero in fifty without WAL. A wallet must not fail to
// initialise because another copy of it was initialising at the same instant.
//
// Doing it here is safe because WAL is a PERSISTENT property of the database
// file: it only has to be set successfully once, ever. Whichever process wins
// sets it for every later one, and until then the database behaves exactly as it
// did before. WithBusyRetry absorbs brief contention; a lasting failure is
// ignored, which also covers the filesystem that cannot support WAL at all (it
// needs shared memory, so most network mounts cannot) — a --config-dir on NFS
// keeps working rather than becoming unopenable.
//
// `synchronous` is deliberately left alone. WAL plus synchronous(NORMAL) is the
// usual pairing and can lose the last committed transaction on power loss — here
// that transaction is a redeem whose money has already moved on the Hub. No
// durability trade on this database for a throughput win.
func enableWAL(db *sql.DB) {
	// QueryRow, not Exec: the pragma answers with the resulting mode, and the row
	// has to be consumed. The result is intentionally discarded — nothing about
	// correctness here depends on which mode we ended up in.
	var mode string
	_ = WithBusyRetry(func() error {
		return db.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&mode)
	})
}

// Open opens cashctl's single local database, cashctl.db, under appdir.Dir(),
// creating the version-2 schema in a new or empty file and refusing any other
// (ErrOldDatabase). It also checks this file's device record (EnsureDevice).
// Callers own the returned *sql.DB and must Close it. 0600: it holds a
// plaintext identity key and cash-mode spending secrets.
func Open() (*sql.DB, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	// busy_timeout(5000): SQLite's own busy handler, so two cashctl processes
	// committing within the same few milliseconds wait rather than fail;
	// WithBusyRetry wraps it as a second, coarser line of defence.
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, err
	}
	enableWAL(db)
	if err := initSchema(db, path); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := chmodJournalSidecars(path); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// initSchema creates the schema in an empty database, refuses one of another
// version, and checks the device record — under one write lock, so two
// processes opening a fresh file at once never both create it.
func initSchema(db *sql.DB, path string) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("cashctl.db: taking the write lock: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		}
	}()

	var tables int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table'`).Scan(&tables); err != nil {
		return fmt.Errorf("cashctl.db: %w", err)
	}
	if tables == 0 {
		if _, err := conn.ExecContext(ctx, schema); err != nil {
			return fmt.Errorf("cashctl.db: creating the schema: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('schema_version', ?)`, SchemaVersion); err != nil {
			return fmt.Errorf("cashctl.db: %w", err)
		}
	} else {
		var version string
		err := conn.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version)
		if err != nil || version != SchemaVersion {
			return &ErrOldDatabase{Path: path}
		}
	}
	if err := ensureDevice(ctx, conn, path); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("cashctl.db: %w", err)
	}
	committed = true
	return nil
}

// chmodJournalSidecars restricts cashctl.db's journal files to 0600, the same as
// the database itself.
//
// They hold the same plaintext an identity privkey and cash-mode spending
// secrets are stored in, and they are created with the process umask, so
// chmodding only cashctl.db left them readable (D-CLI-11). appdir.Dir()'s own
// 0700 contains that today, which is why this is a small fix rather than an
// urgent one — but -wal is PERSISTENT where -journal is transient, so a backup
// or sync tool globbing cashctl.db* would otherwise carry a world-readable file
// full of spending secrets off the machine. That is why it lands with WAL rather
// than after it.
//
// Called after the schema is created, so a -wal those writes made already exists.
// A sidecar that is absent is not an error: which ones exist depends on the
// journal mode and on whether a connection is currently open.
func chmodJournalSidecars(path string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := os.Chmod(path+suffix, 0600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("restricting %s permissions: %w", filepath.Base(path+suffix), err)
		}
	}
	return nil
}
