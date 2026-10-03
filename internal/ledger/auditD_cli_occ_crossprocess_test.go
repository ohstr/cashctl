package ledger

// Audit D, CLI surface, finding 4 (D-CLI-4), part 2: the lost update.
//
// Save is diff-based — a `loaded` baseline, an entriesEqual skip, history
// appended only past historyLoaded — so a row only one process touched was never
// at risk, and this finding is narrower than "any concurrent transition is lost".
// What remained was a row BOTH processes touched: A loads, B transitions it and
// saves, then A upserts its whole stale row back and reverts B. Narrow, but the
// state at stake is a bill's held/spent status and its pending secrets, and it
// was lost SILENTLY, which is the part that mattered.
//
// WHY THESE ARE STAGED AND NOT RACED. A probabilistic version of this test would
// flake in CI, get marked flaky, and then get skipped — which is worse than not
// having it. The interleaving is therefore forced with filesystem rendezvous:
// both sides Load from the same baseline, then the parent saves, and only then
// does the child save. That is the exact ordering the bug needs, every run.
//
// The second test is the one that guards against the fix being too eager:
// two processes writing DIFFERENT rows must both succeed, with no false conflict.
// That is the regression this change could plausibly introduce, and Save being
// diff-based is what makes it safe.
//
// Mutants these must fail against: dropping the WHERE from the DO UPDATE;
// dropping the version bump; treating RowsAffected()==0 as success; and applying
// the conflict check to rows this process is adding rather than only to loaded
// ones.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/cashctl/internal/appdir"
)

const (
	occHelperDirEnv  = "CASHCTL_TEST_OCC_DIR"
	occHelperIDEnv   = "CASHCTL_TEST_OCC_ID"
	occHelperWantEnv = "CASHCTL_TEST_OCC_WANT" // "conflict" or "ok"
)

func occTouch(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
		t.Fatalf("signalling %s: %v", name, err)
	}
}

func occWaitFor(t *testing.T, dir, name string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", name)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestHelperLedgerOCC is the child process: Load from the same baseline the
// parent sees, wait for the parent to commit, then try to write.
func TestHelperLedgerOCC(t *testing.T) {
	dir := os.Getenv(occHelperDirEnv)
	if dir == "" {
		t.Skip("helper process entry point; only runs when " + occHelperDirEnv + " is set")
	}
	appdir.SetOverride(dir)

	l, err := Load()
	if err != nil {
		t.Fatalf("child Load: %v", err)
	}
	occTouch(t, dir, "child-loaded")
	occWaitFor(t, dir, "parent-saved")

	id := os.Getenv(occHelperIDEnv)
	if err := l.SetVerified(id, true); err != nil {
		t.Fatalf("child SetVerified(%s): %v", id, err)
	}
	saveErr := l.Save()

	switch os.Getenv(occHelperWantEnv) {
	case "conflict":
		if !errors.Is(saveErr, ErrConcurrentUpdate) {
			t.Fatalf("child Save on a row the parent changed = %v, want ErrConcurrentUpdate", saveErr)
		}
	default:
		if saveErr != nil {
			t.Fatalf("child Save on a row nobody else touched = %v, want success", saveErr)
		}
	}
}

// seedTwoEntries writes two held entries and returns their ids.
func seedTwoEntries(t *testing.T) (dir, idA, idB string) {
	t.Helper()
	dir = t.TempDir()
	appdir.SetOverride(dir)
	t.Cleanup(func() { appdir.SetOverride("") })

	l, err := Load()
	if err != nil {
		t.Fatalf("seed Load: %v", err)
	}
	amt := uint64(1000)
	a, err := l.Add(Entry{Token: "lokicash1" + strings.Repeat("a", 60), Status: StatusHeld, AmountMillis: &amt})
	if err != nil {
		t.Fatalf("seed Add A: %v", err)
	}
	b, err := l.Add(Entry{Token: "lokicash1" + strings.Repeat("b", 60), Status: StatusHeld, AmountMillis: &amt})
	if err != nil {
		t.Fatalf("seed Add B: %v", err)
	}
	if err := l.Save(); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	return dir, a.ID, b.ID
}

func startOCCHelper(t *testing.T, dir, id, want string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperLedgerOCC$", "-test.v")
	cmd.Env = append(os.Environ(),
		occHelperDirEnv+"="+dir,
		occHelperIDEnv+"="+id,
		occHelperWantEnv+"="+want,
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting helper: %v", err)
	}
	return cmd
}

func TestAuditD_CLI_SameRowConcurrentSaveIsRefusedNotLost(t *testing.T) {
	dir, idA, _ := seedTwoEntries(t)

	// Child will transition the SAME row the parent is about to change.
	child := startOCCHelper(t, dir, idA, "conflict")

	parent, err := Load()
	if err != nil {
		t.Fatalf("parent Load: %v", err)
	}
	occWaitFor(t, dir, "child-loaded") // both now hold the same baseline

	if err := parent.SetStatus(idA, StatusRedeemed); err != nil {
		t.Fatalf("parent SetStatus: %v", err)
	}
	if err := parent.Save(); err != nil {
		t.Fatalf("parent Save: %v", err)
	}
	occTouch(t, dir, "parent-saved")

	if err := child.Wait(); err != nil {
		t.Fatalf("child did not report a conflict as expected: %v", err)
	}

	// The parent's transition must still be on disk — the whole point is that
	// the loser is refused rather than the winner being reverted.
	after, err := Load()
	if err != nil {
		t.Fatalf("re-Load: %v", err)
	}
	e, ok := after.Find(idA)
	if !ok {
		t.Fatalf("entry %s vanished", idA)
	}
	if e.Status != StatusRedeemed {
		t.Errorf("status = %q, want %q — the parent's committed transition was reverted", e.Status, StatusRedeemed)
	}
	if e.Verified {
		t.Error("the child's refused write landed anyway")
	}
}

// TestAuditD_CLI_DisjointRowConcurrentSavesBothSucceed is the no-false-conflict
// guard, and the regression this change could plausibly have introduced. Save
// being diff-based is what makes it safe: the parent's save never rewrites the
// row it did not touch, so the child's version for that row is still current.
func TestAuditD_CLI_DisjointRowConcurrentSavesBothSucceed(t *testing.T) {
	dir, idA, idB := seedTwoEntries(t)

	// Child transitions B; parent transitions A.
	child := startOCCHelper(t, dir, idB, "ok")

	parent, err := Load()
	if err != nil {
		t.Fatalf("parent Load: %v", err)
	}
	occWaitFor(t, dir, "child-loaded")

	if err := parent.SetStatus(idA, StatusRedeemed); err != nil {
		t.Fatalf("parent SetStatus: %v", err)
	}
	if err := parent.Save(); err != nil {
		t.Fatalf("parent Save: %v", err)
	}
	occTouch(t, dir, "parent-saved")

	if err := child.Wait(); err != nil {
		t.Fatalf("a disjoint-row save was refused: %v", err)
	}

	after, err := Load()
	if err != nil {
		t.Fatalf("re-Load: %v", err)
	}
	ea, _ := after.Find(idA)
	eb, _ := after.Find(idB)
	if ea.Status != StatusRedeemed {
		t.Errorf("entry A status = %q, want %q", ea.Status, StatusRedeemed)
	}
	if !eb.Verified {
		t.Error("entry B's concurrent change to a different row was lost")
	}
}

// TestAuditD_CLI_TwoSavesOnOneLedgerDoNotConflictWithEachOther is the guard for
// the bug the optimistic-concurrency change originally introduced, and it is
// worth its own test because the shape is load-bearing rather than exotic.
//
// The D-CLI-1 write-ahead path parks a destination cash secret, places the wire
// call, and saves AGAIN on the same *Ledger. The first save bumps the row's
// version on disk; if loadedVersions stays at its Load value, the second save
// compares against a version its own first save already moved and reports a
// conflict with no second process in sight. On that path a spurious conflict is
// not cosmetic: it is the error returned after the Hub has already moved money.
//
// Save's existing comment claimed re-baselining was "a no-op for every current
// caller (each only calls Save once per process)". It was not true even before
// this change, and this test is what stops it being assumed again.
func TestAuditD_CLI_TwoSavesOnOneLedgerDoNotConflictWithEachOther(t *testing.T) {
	dir, idA, _ := seedTwoEntries(t)
	_ = dir

	l, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// First save: park something on the row, exactly as the write-ahead does.
	for i := range l.Entries {
		if l.Entries[i].ID == idA {
			l.Entries[i].PendingDestinationCashSecret = "parked-secret"
		}
	}
	if err := l.Save(); err != nil {
		t.Fatalf("first Save: %v", err)
	}

	// Second save on the SAME *Ledger, as happens after the wire call returns.
	if err := l.SetStatus(idA, StatusConsolidated); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if err := l.Save(); err != nil {
		t.Fatalf("second Save on the same *Ledger: %v \u2014 a process must not conflict with itself", err)
	}

	after, err := Load()
	if err != nil {
		t.Fatalf("re-Load: %v", err)
	}
	e, ok := after.Find(idA)
	if !ok {
		t.Fatalf("entry %s vanished", idA)
	}
	if e.Status != StatusConsolidated {
		t.Errorf("status = %q, want %q", e.Status, StatusConsolidated)
	}
	if e.PendingDestinationCashSecret != "parked-secret" {
		t.Errorf("parked secret = %q, want it preserved across both saves", e.PendingDestinationCashSecret)
	}
}
