package store

import (
	"errors"
	"time"

	"modernc.org/sqlite"
)

// IsBusy reports whether err is a transient SQLITE_BUSY ("database is
// locked") — the failure mode a save can hit when another process (or,
// within tests, another goroutine) holds cashctl.db's write lock at the
// exact same instant. 5 == SQLITE_BUSY, sqlite3.h's own stable numeric
// code; masked with 0xff so an extended busy sub-code (e.g.
// SQLITE_BUSY_SNAPSHOT) still matches, since those share the low byte with
// the primary code.
func IsBusy(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == 5
}

// IsUniqueViolation reports whether err is specifically a UNIQUE or
// PRIMARY KEY violation — 2067 == SQLITE_CONSTRAINT_UNIQUE and 1555 ==
// SQLITE_CONSTRAINT_PRIMARYKEY, sqlite3.h's own stable extended result
// codes. Deliberately not the coarse primary code (19), which every
// constraint flavor shares — NOT NULL, CHECK, FOREIGN KEY and a trigger's
// own RAISE(ABORT, ...) too — and would misreport any of those as a
// duplicate.
func IsUniqueViolation(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	code := sqliteErr.Code()
	return code == 2067 || code == 1555
}

// WithBusyRetry runs op, retrying a bounded number of times on a transient
// IsBusy error before giving up and returning it. SQLite's write lock is
// only ever held for the length of one transaction commit — a handful of
// local writes, no network I/O inside it — so a short backoff is enough to
// let a concurrent process's own save clear. Retrying the WHOLE operation
// (not just one statement) is safe as long as op rolls its own transaction
// back on any error before returning (every caller does, via defer
// tx.Rollback()), so a retried attempt starts from a clean slate — and op
// must itself be idempotent (upserts, diff-against-snapshot writes), so
// replaying one that did in fact land is a no-op.
//
// This sits on top of the busy_timeout(5000) Open configures: that handles
// plain lock contention inside SQLite, but a deferred transaction that has
// already read and then tries to upgrade to a write can get SQLITE_BUSY
// immediately, without waiting, to avoid a deadlock — only a retry of the
// whole operation recovers from that.
func WithBusyRetry(op func() error) error {
	const maxAttempts = 25
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err = op(); err == nil || !IsBusy(err) {
			return err
		}
		time.Sleep(time.Duration(2+attempt) * time.Millisecond)
	}
	return err
}
