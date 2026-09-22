package store

import (
	"database/sql"
	"sync"
	"testing"
)

// preRenameSchema is the entries table exactly as cashctl 0.3.x wrote it,
// before NIP-CASH renamed bearer mode to cash mode: three columns carrying
// the old spelling, everything else already in its current shape.
const preRenameSchema = `
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
	bearer_secret              TEXT,
	connection_key_platform    TEXT,
	connection_key_external_id TEXT,
	attestation_event_id       TEXT,
	ia_pubkey                  TEXT,
	pending_bearer_secret      TEXT,
	expires_at                 INTEGER,
	bearer_protection          TEXT
);`

// openRaw opens the ledger file directly, without running Open's migrations
// — the point is to build a pre-rename database and then watch Open migrate
// it, so the test can't use Open to create it.
func openRaw(t *testing.T) *sql.DB {
	t.Helper()
	p, err := Path()
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}
	db, err := sql.Open("sqlite", sqliteDSN(p))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	return db
}

func columnNames(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info('entries')`)
	if err != nil {
		t.Fatalf("pragma_table_info: %v", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[n] = true
	}
	return out
}

func seedPreRenameLedger(t *testing.T, schema string) {
	t.Helper()
	raw := openRaw(t)
	if _, err := raw.Exec(schema); err != nil {
		t.Fatalf("creating pre-rename schema: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO entries
		(id, token, wallet_pubkey, secret, received_at, verified, status, bearer_secret,
		 pending_bearer_secret, bearer_protection)
		VALUES ('e1', 'lokicash1abc', 'wp', 'dial-secret', '2026-09-01T00:00:00Z', 1, 'held',
		        'spending-secret', 'pending-secret', 'protected')`); err != nil {
		t.Fatalf("seeding a pre-rename entry: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("closing raw handle: %v", err)
	}
}

// TestOpen_RenamesBearerColumns is the migration's whole point: a ledger
// written by 0.3.x opens under the new names with every value intact. A
// lost cash_secret here is lost money, so the values are asserted, not just
// the column names.
func TestOpen_RenamesBearerColumns(t *testing.T) {
	withTempConfigDir(t)
	seedPreRenameLedger(t, preRenameSchema)

	db, err := Open()
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = db.Close() }()

	cols := columnNames(t, db)
	for _, want := range []string{"cash_secret", "pending_cash_secret", "cash_protection"} {
		if !cols[want] {
			t.Errorf("column %s missing after migration", want)
		}
	}
	for _, gone := range []string{"bearer_secret", "pending_bearer_secret", "bearer_protection"} {
		if cols[gone] {
			t.Errorf("old column %s still present after migration", gone)
		}
	}

	var secret, pending, protection string
	if err := db.QueryRow(
		`SELECT cash_secret, pending_cash_secret, cash_protection FROM entries WHERE id = 'e1'`,
	).Scan(&secret, &pending, &protection); err != nil {
		t.Fatalf("reading the migrated row: %v", err)
	}
	if secret != "spending-secret" {
		t.Errorf("cash_secret = %q, want the original spending secret", secret)
	}
	if pending != "pending-secret" {
		t.Errorf("pending_cash_secret = %q, want the original pending secret", pending)
	}
	if protection != "protected" {
		t.Errorf("cash_protection = %q, want \"protected\"", protection)
	}
}

func TestOpen_RenameIsIdempotent(t *testing.T) {
	withTempConfigDir(t)
	seedPreRenameLedger(t, preRenameSchema)

	for i := 0; i < 3; i++ {
		db, err := Open()
		if err != nil {
			t.Fatalf("Open() #%d error = %v", i+1, err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("Close() #%d error = %v", i+1, err)
		}
	}

	db, err := Open()
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = db.Close() }()

	var secret string
	if err := db.QueryRow(`SELECT cash_secret FROM entries WHERE id = 'e1'`).Scan(&secret); err != nil {
		t.Fatalf("reading after repeated opens: %v", err)
	}
	if secret != "spending-secret" {
		t.Errorf("cash_secret = %q after repeated opens", secret)
	}
}

// TestOpen_RenameOnLedgerMissingLaterColumns covers a ledger old enough to
// predate pending_bearer_secret/bearer_protection entirely: there is
// nothing to rename for those two, and addColumnsIfMissing must still add
// them under their new names.
func TestOpen_RenameOnLedgerMissingLaterColumns(t *testing.T) {
	withTempConfigDir(t)
	seedOld := `
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
	bearer_secret              TEXT,
	connection_key_platform    TEXT,
	connection_key_external_id TEXT,
	attestation_event_id       TEXT,
	ia_pubkey                  TEXT
);`
	raw := openRaw(t)
	if _, err := raw.Exec(seedOld); err != nil {
		t.Fatalf("creating the older schema: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO entries
		(id, token, wallet_pubkey, secret, received_at, verified, status, bearer_secret)
		VALUES ('e1', 'lokicash1abc', 'wp', 'dial-secret', '2026-09-01T00:00:00Z', 1, 'held', 'spending-secret')`); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	_ = raw.Close()

	db, err := Open()
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = db.Close() }()

	cols := columnNames(t, db)
	for _, want := range []string{"cash_secret", "pending_cash_secret", "cash_protection"} {
		if !cols[want] {
			t.Errorf("column %s missing", want)
		}
	}
	var secret string
	if err := db.QueryRow(`SELECT cash_secret FROM entries WHERE id = 'e1'`).Scan(&secret); err != nil {
		t.Fatalf("reading the migrated row: %v", err)
	}
	if secret != "spending-secret" {
		t.Errorf("cash_secret = %q, want the original spending secret", secret)
	}
}

// TestOpen_RenameWhenBothColumnsExist is the downgrade-then-upgrade path: a
// 0.3.x binary opened an already-migrated ledger and re-added the empty old
// columns. The new column's value must win, and the stale duplicate must go.
func TestOpen_RenameWhenBothColumnsExist(t *testing.T) {
	withTempConfigDir(t)
	seedPreRenameLedger(t, preRenameSchema)

	db, err := Open()
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	_ = db.Close()

	// Simulate the old binary: re-add the columns it knows about.
	raw := openRaw(t)
	for _, ddl := range []string{
		`ALTER TABLE entries ADD COLUMN bearer_secret TEXT`,
		`ALTER TABLE entries ADD COLUMN pending_bearer_secret TEXT`,
		`ALTER TABLE entries ADD COLUMN bearer_protection TEXT`,
	} {
		if _, err := raw.Exec(ddl); err != nil {
			t.Fatalf("re-adding a legacy column: %v", err)
		}
	}
	_ = raw.Close()

	db2, err := Open()
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	defer func() { _ = db2.Close() }()

	cols := columnNames(t, db2)
	for _, gone := range []string{"bearer_secret", "pending_bearer_secret", "bearer_protection"} {
		if cols[gone] {
			t.Errorf("stale column %s survived", gone)
		}
	}
	var secret string
	if err := db2.QueryRow(`SELECT cash_secret FROM entries WHERE id = 'e1'`).Scan(&secret); err != nil {
		t.Fatalf("reading after the merge: %v", err)
	}
	if secret != "spending-secret" {
		t.Errorf("cash_secret = %q, want the original value to win over the empty legacy column", secret)
	}
}

// TestOpen_RenameConcurrent runs the migration from several goroutines at
// once, standing in for several cashctl processes opening the same ledger
// together: exactly one performs the rename, and none of them error.
func TestOpen_RenameConcurrent(t *testing.T) {
	withTempConfigDir(t)
	seedPreRenameLedger(t, preRenameSchema)

	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	dbs := make([]*sql.DB, n)

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			db, err := Open()
			errs[i], dbs[i] = err, db
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent Open() #%d error = %v", i, err)
		}
		if dbs[i] != nil {
			_ = dbs[i].Close()
		}
	}

	db, err := Open()
	if err != nil {
		t.Fatalf("Open() after the concurrent round: %v", err)
	}
	defer func() { _ = db.Close() }()

	var secret string
	if err := db.QueryRow(`SELECT cash_secret FROM entries WHERE id = 'e1'`).Scan(&secret); err != nil {
		t.Fatalf("reading after concurrent migration: %v", err)
	}
	if secret != "spending-secret" {
		t.Errorf("cash_secret = %q after concurrent migration", secret)
	}
}
