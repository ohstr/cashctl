package store

import (
	"os"
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
	defer db.Close()

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
	db1.Close()

	// Re-opening an existing database must not fail or wipe it — the
	// schema's CREATE TABLE/INDEX IF NOT EXISTS statements must be safe to
	// re-run against an already-migrated file.
	db2, err := Open()
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	defer db2.Close()

	if _, err := db2.Exec(`INSERT INTO connections (name, value, added_at) VALUES ('a', 'b', 'c')`); err != nil {
		t.Fatalf("inserting into a re-opened database failed: %v", err)
	}

	db3, err := Open()
	if err != nil {
		t.Fatalf("third Open() error = %v", err)
	}
	defer db3.Close()

	var count int
	if err := db3.QueryRow(`SELECT COUNT(*) FROM connections`).Scan(&count); err != nil {
		t.Fatalf("querying after re-open error = %v", err)
	}
	if count != 1 {
		t.Errorf("row count after re-opening = %d, want 1 (re-opening must not wipe existing data)", count)
	}
}
