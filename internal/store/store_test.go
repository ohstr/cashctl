package store

import (
	"database/sql"
	"errors"
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

	if _, err := db2.Exec(`INSERT INTO connections (name, kind, value, added_at) VALUES ('a', 'nwc', 'b', 'c')`); err != nil {
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

// T-129: a new file gets the version-2 schema.
func TestOpen_CreatesSchemaVersion2(t *testing.T) {
	withTempConfigDir(t)
	db, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var v string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v); err != nil || v != SchemaVersion {
		t.Fatalf("schema_version = %q (%v), want %q", v, err, SchemaVersion)
	}
	for _, table := range []string{"device", "pending_calls", "call_secrets", "derived_ids", "entries", "connections"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil || n != 1 {
			t.Errorf("table %s missing from the v3 schema", table)
		}
	}
}

// T-129: a database an earlier cashctl wrote is refused, naming its path, and
// left exactly as it was: nothing in it is read, nothing added.
func TestOpen_RefusesAnEarlierDatabase(t *testing.T) {
	for name, setup := range map[string]string{
		"rc.17 schema, no meta": `CREATE TABLE entries (id TEXT PRIMARY KEY, token TEXT); INSERT INTO entries VALUES ('tok-1', 'lokicash1x')`,
		"meta of version 1":     `CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL); INSERT INTO meta VALUES ('schema_version', '1')`,
		"pre-release version 2": `CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL); INSERT INTO meta VALUES ('schema_version', '2')`,
	} {
		t.Run(name, func(t *testing.T) {
			withTempConfigDir(t)
			path, _ := Path()
			raw, err := sql.Open("sqlite", sqliteDSN(path))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(setup); err != nil {
				t.Fatal(err)
			}
			_ = raw.Close()

			_, err = Open()
			var old *ErrOldDatabase
			if !errors.As(err, &old) || old.Path != path || !strings.Contains(err.Error(), "move it aside") {
				t.Fatalf("Open() = %v, want ErrOldDatabase naming %s", err, path)
			}
			raw, _ = sql.Open("sqlite", sqliteDSN(path))
			defer func() { _ = raw.Close() }()
			var n int
			_ = raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'device'`).Scan(&n)
			if n != 0 {
				t.Error("the refused database was written to")
			}
		})
	}
}

// The device id is made once and kept; a copy of the file elsewhere (another
// fingerprint) gets a new one and says so in history.
func TestOpen_DeviceIDRotatesWhenTheFileIsCopied(t *testing.T) {
	withTempConfigDir(t)
	first, err := DeviceID()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 64 || strings.ToLower(first) != first {
		t.Fatalf("device id %q, want 64 lowercase hex", first)
	}
	again, err := DeviceID()
	if err != nil || again != first {
		t.Fatalf("device id changed on reopen: %q → %q (%v)", first, again, err)
	}

	orig := machineIDFn
	machineIDFn = func() string { return "another-machine" }
	t.Cleanup(func() { machineIDFn = orig })
	moved, err := DeviceID()
	if err != nil || moved == first {
		t.Fatalf("device id after a copy to another machine: %q (%v), want a new one", moved, err)
	}
	db, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM history WHERE action = 'device'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("history lines for the rotation = %d (%v), want 1", n, err)
	}
}

// CC-A4: the same file reached through a symlinked directory, or with another
// device number, keeps its fingerprint; a copy (another inode) does not.
func TestFingerprint_StableAcrossPathSpellingsAndDevice(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "cashctl.db")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skip("no symlinks here")
	}
	origFile := fileIDFn
	t.Cleanup(func() { fileIDFn = origFile })
	dev := uint64(1)
	fileIDFn = func(string) (uint64, uint64, error) { return dev, 42, nil }
	a, err := fingerprint(file)
	if err != nil {
		t.Fatal(err)
	}
	dev = 2
	b, err := fingerprint(filepath.Join(link, "cashctl.db"))
	if err != nil || a != b {
		t.Errorf("fingerprint through a symlink with another device number changed: %s → %s (%v)", a, b, err)
	}
	fileIDFn = func(string) (uint64, uint64, error) { return 1, 43, nil }
	if c, _ := fingerprint(file); c == a {
		t.Error("a copy (another inode) kept the fingerprint")
	}
}
