//go:build integration

package integration

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// The pending-secret fallback these tests once covered is gone: every call
// that creates a bill is written ahead (the request id and every secret it
// generates are saved before it is sent) and settled through cash_lookup, so
// the ledger has no pending_cash_secret column left to fall back on. What
// stays are the helpers the --as tests use to make the stored secret wrong.

// currentCashSecret reads entryID's live cash_secret column directly —
// receive's own --json output never includes it (Entry.CashSecret is
// json:"-"), so this is the only way a test can learn the real secret
// protect just established.
func currentCashSecret(t *testing.T, f *fixture, entryID string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.configDir, "cashctl.db"))
	if err != nil {
		t.Fatalf("open cashctl.db directly: %v", err)
	}
	defer db.Close()
	var secret string
	if err := db.QueryRow(`SELECT cash_secret FROM entries WHERE id = ?`, entryID).Scan(&secret); err != nil {
		t.Fatalf("querying cash_secret: %v", err)
	}
	return secret
}

// corruptCashSecret opens f's cashctl.db directly and overwrites the held
// entry's cash_secret, so only a credential passed in --as can spend it.
func corruptCashSecret(t *testing.T, f *fixture, entryID, cashSecret string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.configDir, "cashctl.db"))
	if err != nil {
		t.Fatalf("open cashctl.db directly: %v", err)
	}
	defer db.Close()
	res, err := db.Exec(`UPDATE entries SET cash_secret = ? WHERE id = ?`, cashSecret, entryID)
	if err != nil {
		t.Fatalf("corrupt entries row: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("corrupt entries row: matched %d rows, want 1", n)
	}
}
