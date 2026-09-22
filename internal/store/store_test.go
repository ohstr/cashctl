package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ohstr/cashctl/internal/appdir"
)

func withTempConfigDir(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	appdir.SetOverride(tmp)
	t.Cleanup(func() { appdir.SetOverride("") })
}

func TestOpen_CreatesFileWithRestrictivePermissions(t *testing.T) {
	withTempConfigDir(t)

	db, err := Open()
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = db.Close() }()

	p, err := Path()
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("cashctl.db permissions = %o, want 0600", perm)
	}
}

func TestOpen_IsIdempotent(t *testing.T) {
	withTempConfigDir(t)

	db1, err := Open()
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	_ = db1.Close()

	// Re-opening an existing database must not fail or wipe it — the
	// schema's CREATE TABLE/INDEX IF NOT EXISTS statements must be safe to
	// re-run against an already-migrated file.
	db2, err := Open()
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	defer func() { _ = db2.Close() }()

	if _, err := db2.Exec(`INSERT INTO connections (name, value, added_at) VALUES ('a', 'b', 'c')`); err != nil {
		t.Fatalf("inserting into a re-opened database failed: %v", err)
	}

	db3, err := Open()
	if err != nil {
		t.Fatalf("third Open() error = %v", err)
	}
	defer func() { _ = db3.Close() }()

	var count int
	if err := db3.QueryRow(`SELECT COUNT(*) FROM connections`).Scan(&count); err != nil {
		t.Fatalf("querying after re-open error = %v", err)
	}
	if count != 1 {
		t.Errorf("row count after re-opening = %d, want 1 (re-opening must not wipe existing data)", count)
	}
}

// TestOpen_MigratesPreExistingDatabaseMissingNewColumn simulates a
// cashctl.db created before pending_cash_secret existed (an old
// CREATE TABLE IF NOT EXISTS run, before that column was ever added to the
// schema) — Open must add it via ALTER TABLE rather than silently leaving
// it missing (CREATE TABLE IF NOT EXISTS is a no-op against an
// already-existing table, so the column would otherwise never appear).
func TestOpen_MigratesPreExistingDatabaseMissingNewColumn(t *testing.T) {
	withTempConfigDir(t)

	p, err := Path()
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}
	pre, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatalf("pre-create db: %v", err)
	}
	oldSchema := strings.Replace(schema, ",\n\tpending_cash_secret      TEXT", "", 1)
	if oldSchema == schema {
		t.Fatal("test fixture bug: pending_cash_secret line not found in schema to strip")
	}
	if _, err := pre.Exec(oldSchema); err != nil {
		t.Fatalf("apply old schema: %v", err)
	}
	if _, err := pre.Exec(`INSERT INTO entries (id, token, wallet_pubkey, secret, received_at, verified, status) VALUES ('tok-1','t','w','s','2020-01-01T00:00:00Z',1,'held')`); err != nil {
		t.Fatalf("insert pre-migration row: %v", err)
	}
	if err := pre.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open()
	if err != nil {
		t.Fatalf("Open() on a pre-existing DB missing the new column: error = %v", err)
	}
	defer func() { _ = db.Close() }()

	var got sql.NullString
	if err := db.QueryRow(`SELECT pending_cash_secret FROM entries WHERE id = 'tok-1'`).Scan(&got); err != nil {
		t.Fatalf("querying the migrated column: %v", err)
	}
	if got.Valid {
		t.Errorf("pending_cash_secret = %q, want NULL for a pre-existing row", got.String)
	}

	// A second Open() (the ordinary "every command opens the db" case)
	// must not fail now that the column already exists.
	db2, err := Open()
	if err != nil {
		t.Fatalf("second Open() after migration: error = %v", err)
	}
	_ = db2.Close()
}

// TestOpen_MigratesPreExistingDatabaseMissingExpiresAt is
// TestOpen_MigratesPreExistingDatabaseMissingNewColumn's own scenario for
// expires_at specifically — a cashctl.db from before that column existed
// must gain it via ALTER TABLE, not fail or silently leave it missing.
func TestOpen_MigratesPreExistingDatabaseMissingExpiresAt(t *testing.T) {
	withTempConfigDir(t)

	p, err := Path()
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}
	pre, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatalf("pre-create db: %v", err)
	}
	oldSchema := strings.Replace(schema, ",\n\texpires_at                 INTEGER", "", 1)
	if oldSchema == schema {
		t.Fatal("test fixture bug: expires_at line not found in schema to strip")
	}
	if _, err := pre.Exec(oldSchema); err != nil {
		t.Fatalf("apply old schema: %v", err)
	}
	if _, err := pre.Exec(`INSERT INTO entries (id, token, wallet_pubkey, secret, received_at, verified, status) VALUES ('tok-1','t','w','s','2020-01-01T00:00:00Z',1,'held')`); err != nil {
		t.Fatalf("insert pre-migration row: %v", err)
	}
	if err := pre.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open()
	if err != nil {
		t.Fatalf("Open() on a pre-existing DB missing expires_at: error = %v", err)
	}
	defer func() { _ = db.Close() }()

	var got sql.NullInt64
	if err := db.QueryRow(`SELECT expires_at FROM entries WHERE id = 'tok-1'`).Scan(&got); err != nil {
		t.Fatalf("querying the migrated column: %v", err)
	}
	if got.Valid {
		t.Errorf("expires_at = %v, want NULL for a pre-existing row", got.Int64)
	}
}

// TestOpen_MigratesPreExistingDatabaseMissingCashProtection is the same
// scenario for cash_protection.
func TestOpen_MigratesPreExistingDatabaseMissingCashProtection(t *testing.T) {
	withTempConfigDir(t)

	p, err := Path()
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}
	pre, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatalf("pre-create db: %v", err)
	}
	oldSchema := strings.Replace(schema, ",\n\tcash_protection          TEXT", "", 1)
	if oldSchema == schema {
		t.Fatal("test fixture bug: cash_protection line not found in schema to strip")
	}
	if _, err := pre.Exec(oldSchema); err != nil {
		t.Fatalf("apply old schema: %v", err)
	}
	if _, err := pre.Exec(`INSERT INTO entries (id, token, wallet_pubkey, secret, received_at, verified, status) VALUES ('tok-1','t','w','s','2020-01-01T00:00:00Z',1,'held')`); err != nil {
		t.Fatalf("insert pre-migration row: %v", err)
	}
	if err := pre.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open()
	if err != nil {
		t.Fatalf("Open() on a pre-existing DB missing cash_protection: error = %v", err)
	}
	defer func() { _ = db.Close() }()

	var got sql.NullString
	if err := db.QueryRow(`SELECT cash_protection FROM entries WHERE id = 'tok-1'`).Scan(&got); err != nil {
		t.Fatalf("querying the migrated column: %v", err)
	}
	if got.Valid {
		t.Errorf("cash_protection = %q, want NULL for a pre-existing row", got.String)
	}
}

// A --config-dir containing '?', '#' or '%' is a perfectly legal directory
// name, but the path used to be spliced raw into a SQLite "file:" URI, so
// those were parsed as URI syntax: the database got created at a truncated
// path (with default permissions) while the follow-up chmod looked at the
// real one, and Open failed.
func TestOpen_DirectoryNamesWithURISignificantCharacters(t *testing.T) {
	for _, name := range []string{"q?mark", "h#ash", "p%41ct", "a?b#c%d", "50%"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir() + "/" + name
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			appdir.SetOverride(dir)
			t.Cleanup(func() { appdir.SetOverride("") })

			db, err := Open()
			if err != nil {
				t.Fatalf("Open() in %q error = %v", name, err)
			}
			defer func() { _ = db.Close() }()

			info, err := os.Stat(dir + "/cashctl.db")
			if err != nil {
				t.Fatalf("no cashctl.db inside the requested directory: %v", err)
			}
			if perm := info.Mode().Perm(); perm != 0600 {
				t.Errorf("cashctl.db permissions = %o, want 0600", perm)
			}
			entries, _ := os.ReadDir(filepath.Dir(dir))
			if len(entries) != 1 {
				t.Errorf("state leaked outside the requested directory: %v", entries)
			}
		})
	}
}
