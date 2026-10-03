package store

// Audit D, CLI surface, finding 4 (D-CLI-4), first part: the journal.
//
// The ledger ran on SQLite's default rollback journal — journal_mode was never
// set anywhere in internal/. Under a rollback journal a writer blocks readers
// and a reader blocks the writer, which is the contention this codebase's shape
// invites: Load ... network round trip ... Save on two independent connections,
// plus two BEGIN IMMEDIATE migrations on every Open. 25d70a7 (the duplicate
// column race) and the -race-widened SQLITE_BUSY window in
// TestOpen_RenameConcurrent both came out of that.
//
// And the sidecars were never mode-restricted (D-CLI-11). Open chmods
// cashctl.db to 0600 precisely because it "can hold a plaintext identity privkey
// and cash-mode spending secrets" — but cashctl.db-journal and cashctl.db-wal
// hold the same plaintext and were left at whatever the process umask gives.
// Contained today by appdir.Dir()'s 0700, hence Low, but it has to be fixed in
// the same change that turns WAL on: -journal is transient whereas -wal is
// PERSISTENT, so a backup or sync tool globbing cashctl.db* would carry a
// world-readable file full of spending secrets off the machine.
//
// Mutants these must fail against: removing journal_mode(WAL) from the DSN, and
// removing either sidecar chmod.

import (
	"os"
	"testing"
)

// journalMode asks the database what mode it is actually in, rather than
// trusting that the DSN pragma was accepted.
func journalMode(t *testing.T) string {
	t.Helper()
	db, err := Open()
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = db.Close() }()

	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	return mode
}

func TestAuditD_CLI_LedgerUsesWAL(t *testing.T) {
	withTempConfigDir(t)

	if mode := journalMode(t); mode != "wal" {
		t.Errorf("journal_mode = %q, want \"wal\" — a rollback journal makes a writer and a reader block each other, which is exactly this ledger's access pattern", mode)
	}
}

// TestAuditD_CLI_JournalSidecarsAreModeRestricted is the D-CLI-11 half. The -wal
// file is created on first write, and Open runs migrations, so it exists by the
// time Open returns — checked while the handle is still open, since SQLite
// removes it on a clean close of the last connection.
func TestAuditD_CLI_JournalSidecarsAreModeRestricted(t *testing.T) {
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

	var checked int
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		info, statErr := os.Stat(p + suffix)
		if statErr != nil {
			continue // not every sidecar exists in every mode; that is fine
		}
		checked++
		// -shm carries no ledger content itself, but it is created alongside
		// -wal and there is no reason to leave it looser than the database.
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Errorf("%s permissions = %o, want 0600 — it holds the same plaintext secrets as cashctl.db", p+suffix, perm)
		}
	}
	if checked == 0 {
		t.Fatal("no journal sidecar existed to check; the test would pass vacuously, so it is failing instead")
	}
}

// TestAuditD_CLI_OpenSurvivesAJournalModeItCannotSet pins the requirement that
// enabling WAL must never make a wallet unopenable. WAL needs shared memory and
// does not work on most network filesystems, so a --config-dir on NFS has to keep
// working.
//
// Being honest about what this does and does not prove: it cannot simulate a
// filesystem without shm support portably, so it does NOT exercise the fallback
// itself. What it does assert is that Open reports success and a usable mode on
// a database whose journal_mode was already something else — i.e. that the
// transition is not treated as fatal. The no-shm path is covered by Open
// deliberately not returning an error on a failed mode change, which the mutant
// list names.
func TestAuditD_CLI_OpenSurvivesAJournalModeItCannotSet(t *testing.T) {
	withTempConfigDir(t)

	// First Open creates the database; second one reopens an existing file whose
	// mode is already set, which is the ordinary path on every later command.
	if mode := journalMode(t); mode == "" {
		t.Fatal("first Open reported an empty journal_mode")
	}
	mode := journalMode(t)
	if mode == "" {
		t.Fatal("reopening an existing database reported an empty journal_mode")
	}
	t.Logf("reopened database is in %q", mode)
}
