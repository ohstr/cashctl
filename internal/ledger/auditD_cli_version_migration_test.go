package ledger

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/store"
)

// Audit D, CLI surface, finding 4 (D-CLI-4), part 2: the migration half.
//
// A ledger written before the version column existed must open, read back with
// its secrets intact, and — this is the part that would actually have bitten —
// still SAVE.
//
// A pre-version row has SQL NULL in version. `NULL = 0` is NULL, not true, in
// SQLite, so without the COALESCE on both sides of the comparison the
// DO UPDATE's WHERE would never match for any pre-existing row: every save
// against an existing ledger would come back as ErrConcurrentUpdate, with no
// concurrency involved at all. That failure mode appears only on data that
// predates the column, so it needs a test that predates it too — the same
// reasoning as TestMigration_OldDBWithoutDestinationColumnOpens, and the same
// hand-built table.
func TestAuditD_CLI_PreVersionLedgerOpensReadsAndSaves(t *testing.T) {
	dir := t.TempDir()
	appdir.SetOverride(dir)
	t.Cleanup(func() { appdir.SetOverride("") })

	p, err := store.Path()
	if err != nil {
		t.Fatalf("store.Path(): %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// The entries table exactly as it stood BEFORE the version column, and the
	// history table Load also reads.
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
		pending_cash_secret TEXT, expires_at INTEGER, cash_protection TEXT,
		pending_destination_cash_secret TEXT);
		CREATE TABLE history (id INTEGER PRIMARY KEY AUTOINCREMENT, at TEXT NOT NULL,
		action TEXT NOT NULL, detail TEXT NOT NULL);`); err != nil {
		t.Fatalf("creating the pre-version tables: %v", err)
	}
	const oldSecret = "the-old-cash-secret"
	// The '' columns are not padding: Load scans minter_pubkey and the
	// connection_key_*/attestation/ia columns into plain strings rather than
	// sql.NullString, which is safe only because Save always writes '' for an
	// unset one. A hand-built row has to match what Save would have produced,
	// or this test fails on its own fixture rather than on the migration.
	if _, err := raw.Exec(`INSERT INTO entries (id, token, wallet_pubkey, minter_pubkey, secret,
		received_at, verified, status, cash_secret, connection_key_platform,
		connection_key_external_id, attestation_event_id, ia_pubkey)
		VALUES ('tok-old','lokicash1old','pub','','sec','2026-09-01T00:00:00Z',0,'held',?,'','','','')`, oldSecret); err != nil {
		t.Fatalf("seeding a pre-version row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("closing the raw handle: %v", err)
	}

	l, err := Load()
	if err != nil {
		t.Fatalf("Load() on a pre-version ledger: %v", err)
	}
	e, ok := l.Find("tok-old")
	if !ok {
		t.Fatal("the pre-version row did not survive Load")
	}
	if e.CashSecret != oldSecret {
		t.Errorf("cash_secret = %q, want %q — a spending secret did not survive the migration", e.CashSecret, oldSecret)
	}

	// The real guard: saving a change to that row must succeed. Its version is
	// SQL NULL, and NULL = 0 is NULL rather than true, so a missing COALESCE
	// would turn every save against any pre-existing ledger into a spurious
	// conflict.
	if err := l.SetVerified("tok-old", true); err != nil {
		t.Fatalf("SetVerified: %v", err)
	}
	if err := l.Save(); err != nil {
		t.Fatalf("Save() on a pre-version row: %v — a NULL version must read as 0, not as a conflict", err)
	}

	after, err := Load()
	if err != nil {
		t.Fatalf("re-Load: %v", err)
	}
	e2, _ := after.Find("tok-old")
	if !e2.Verified {
		t.Error("the change to a pre-version row was not persisted")
	}
	if e2.CashSecret != oldSecret {
		t.Errorf("cash_secret after save = %q, want %q", e2.CashSecret, oldSecret)
	}
}
