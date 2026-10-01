package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/ohstr/cashctl/internal/appdir"
)

// Audit D, CLI role, D-CLI-1's persistence layer.
//
// A database written before pending_destination_cash_secret existed must open,
// migrate and read back cleanly. The column reaches an existing table only
// through addColumnsIfMissing — CREATE TABLE IF NOT EXISTS is a no-op against
// one that already exists — so forgetting the addedColumns entry would leave
// every pre-existing ledger failing to open with "no such column", while a
// fresh one worked perfectly and every other test stayed green.
//
// Worth a permanent test rather than a one-off check because the trail already
// has this exact class, in its most expensive form: lokihub's db_migrate copied
// 6 of 27 tables and reported success (C2, fixed in 81a9ac2). A schema change
// whose failure mode only appears on data that predates it needs a test that
// predates it too, which is what the hand-built pre-change table below is.
func TestMigration_OldDBWithoutDestinationColumnOpens(t *testing.T) {
	dir := t.TempDir()
	appdir.SetOverride(dir)
	t.Cleanup(func() { appdir.SetOverride("") })

	p, err := Path()
	if err != nil {
		t.Fatalf("Path() = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// The entries table exactly as it stood BEFORE this change.
	raw, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE entries (
		id TEXT PRIMARY KEY, token TEXT NOT NULL UNIQUE, wallet_pubkey TEXT NOT NULL,
		minter_pubkey TEXT, secret TEXT NOT NULL, relay_urls TEXT, identity_required INTEGER,
		amount_millis INTEGER, received_at TEXT NOT NULL, verified INTEGER NOT NULL,
		status TEXT NOT NULL, cash_secret TEXT, connection_key_platform TEXT,
		connection_key_external_id TEXT, attestation_event_id TEXT, ia_pubkey TEXT,
		pending_cash_secret TEXT, expires_at INTEGER, cash_protection TEXT);`); err != nil {
		t.Fatalf("creating the pre-change table: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO entries (id, token, wallet_pubkey, secret, received_at, verified, status, cash_secret)
		VALUES ('tok-old','lokicash1old','pub','sec','2026-09-01T00:00:00Z',1,'held','the-old-secret')`); err != nil {
		t.Fatalf("seeding a pre-change row: %v", err)
	}
	_ = raw.Close()

	db, err := Open()
	if err != nil {
		t.Fatalf("Open() on a pre-change database = %v — every existing ledger would fail to open", err)
	}
	defer func() { _ = db.Close() }()

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('entries') WHERE name='pending_destination_cash_secret'`).Scan(&n); err != nil {
		t.Fatalf("pragma_table_info: %v", err)
	}
	if n != 1 {
		t.Fatalf("pending_destination_cash_secret column count = %d, want 1 — the migration did not run", n)
	}
	// The pre-existing row survives, with its spending secret intact and the new
	// column reading as NULL (which Load scans through sql.NullString).
	var cash string
	var dest sql.NullString
	if err := db.QueryRow(`SELECT cash_secret, pending_destination_cash_secret FROM entries WHERE id='tok-old'`).Scan(&cash, &dest); err != nil {
		t.Fatalf("reading the migrated row: %v", err)
	}
	if cash != "the-old-secret" {
		t.Fatalf("cash_secret = %q after migration, want it carried across untouched", cash)
	}
	if dest.Valid && dest.String != "" {
		t.Fatalf("pending_destination_cash_secret = %q on a pre-change row, want NULL/empty", dest.String)
	}
}
