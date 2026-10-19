//go:build integration

package integration

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func pendingCashSecret(t *testing.T, f *fixture, entryID string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.configDir, "cashctl.db"))
	if err != nil {
		t.Fatalf("open cashctl.db directly: %v", err)
	}
	defer db.Close()
	var pending sql.NullString
	if err := db.QueryRow(`SELECT pending_cash_secret FROM entries WHERE id = ?`, entryID).Scan(&pending); err != nil {
		t.Fatalf("read pending_cash_secret: %v", err)
	}
	return pending.String
}

func heldProtection(t *testing.T, f *fixture, entryID string) string {
	t.Helper()
	held, _ := f.mustJSON("wallet", "show")["held_tokens"].([]any)
	for _, h := range held {
		if m, _ := h.(map[string]any); m["id"] == entryID {
			p, _ := m["cash_protection"].(string)
			return p
		}
	}
	t.Fatalf("%s is not held", entryID)
	return ""
}

// #97: a protect whose re-key landed but whose reply was lost leaves the live
// secret pending. The next command that settles (here a consolidate with
// nothing to merge) asks the Hub, read-only, and promotes it: the bill is
// protected again without waiting for a spend to stumble on it.
func TestSettle_PromotesAPendingSecretThatLanded(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	id := receiveAndScrambleCashSecret(t, f, mintCashGift(t, hub, 4_000))
	real := pendingCashSecret(t, f, id)

	if res := f.run("consolidate", "--yes"); res.ExitCode != 0 && res.ExitCode != 2 {
		t.Fatalf("consolidate: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if got := currentCashSecret(t, f, id); got != real {
		t.Errorf("cash_secret = %q, want the pending secret %q promoted", got, real)
	}
	if got := pendingCashSecret(t, f, id); got != "" {
		t.Errorf("pending_cash_secret = %q, want cleared", got)
	}
	if got := heldProtection(t, f, id); got != "protected" {
		t.Errorf("cash_protection = %q, want protected", got)
	}
}

// One whose re-key did not land: the current secret still answers, so the dead
// pending one is dropped.
func TestSettle_DropsAPendingSecretThatDidNotLand(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	recv := f.mustJSON("receive", mintCashGift(t, hub, 4_000))
	id := entryID(t, recv["entry"])
	real := currentCashSecret(t, f, id)
	corruptCashSecret(t, f, id, real, strings.Repeat("d", 64))

	if res := f.run("consolidate", "--yes"); res.ExitCode != 0 && res.ExitCode != 2 {
		t.Fatalf("consolidate: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if got := currentCashSecret(t, f, id); got != real {
		t.Errorf("cash_secret = %q, want %q kept", got, real)
	}
	if got := pendingCashSecret(t, f, id); got != "" {
		t.Errorf("pending_cash_secret = %q, want the dead one dropped", got)
	}
}
