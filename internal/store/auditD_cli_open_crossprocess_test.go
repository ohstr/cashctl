package store

// Audit D, CLI surface, finding 4 (D-CLI-4) — concurrent Open across PROCESSES.
//
// This exists because enabling WAL broke exactly this case and no unit test
// caught it. TestOpen_RenameConcurrent uses goroutines in one process, which
// share a single SQLite library instance and one *sql.DB pool, so it does not
// exercise POSIX file locking between processes at all. The break only showed up
// in the live integration suite, as TestInit_ParallelFirstRun_OneIdentity
// reporting
//
//	cashctl.db schema migration: database is locked (5) (SQLITE_BUSY)
//
// in 0.07s — far below the 5s busy_timeout, i.e. something was not waiting at
// all. Cause: journal_mode was being set through the DSN, a WAL transition needs
// an exclusive lock, and SQLite answers one it cannot take with SQLITE_BUSY
// immediately rather than honouring busy_timeout. A DSN pragma that fails takes
// the connection down with it.
//
// WHY THE FILE BARRIER. The first version of this test just released five
// `exec.Command` children from a channel and passed against the broken code,
// 5 runs out of 5 — process startup is slow and jittery enough that they reached
// Open at quite different moments, so they never contended. A test that cannot
// fail is worse than no test, so the children now rendezvous on the filesystem:
// each signals ready and then spins on a barrier file, which puts them inside
// Open within microseconds of each other.
//
// Measured, for the record: 5 concurrent `cashctl init` runs on one fresh dir
// failed about 1 in 50 with the DSN pragma and 0 in 50 without it; after the fix,
// 0 in 100.
//
// And measured for this test specifically: with the DSN pragma restored it fails
// 2 runs in 5; with the fix, 0 in 5. So it is a PROBABILISTIC detector, not a
// guaranteed gate — worth stating plainly rather than implying it catches this
// every time. It is sound in the one direction that matters: it cannot fail
// unless concurrent Opens really are failing, so a red here is always a real
// bug. The integration suite's TestInit_ParallelFirstRun_OneIdentity is the other
// half of the guard, at the CLI level and against a real config dir.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ohstr/cashctl/internal/appdir"
)

// openHelperDirEnv names the config dir for a helper child, and its presence is
// what tells the child it is the helper rather than an ordinary test run.
const openHelperDirEnv = "CASHCTL_TEST_STORE_OPEN_DIR"

// TestHelperStoreOpen is not a test of its own — it is the child process the
// cross-process test re-executes. Skips immediately in a normal run.
func TestHelperStoreOpen(t *testing.T) {
	dir := os.Getenv(openHelperDirEnv)
	if dir == "" {
		t.Skip("helper process entry point; only runs when " + openHelperDirEnv + " is set")
	}

	// Announce readiness, then rendezvous, so every child is about to call Open
	// at the same instant. os.Getpid keeps the ready files distinct.
	barrier := filepath.Join(dir, "barrier")
	ready := filepath.Join(dir, "ready")
	if err := os.WriteFile(ready+"."+itoa(os.Getpid()), nil, 0600); err != nil {
		t.Fatalf("signalling ready: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(barrier); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("barrier never appeared")
		}
	}

	appdir.SetOverride(dir)
	db, err := Open()
	if err != nil {
		t.Fatalf("Open() in helper: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() in helper: %v", err)
	}
}

// itoa avoids pulling strconv in for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestAuditD_CLI_ConcurrentOpenAcrossProcessesOnAFreshDatabase(t *testing.T) {
	// Not t.TempDir(): the children write their rendezvous files here too, and
	// the config dir is the thing they must contend over.
	dir := t.TempDir()
	const n = 5

	var wg sync.WaitGroup
	outs := make([]string, n)
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperStoreOpen$", "-test.v")
			cmd.Env = append(os.Environ(), openHelperDirEnv+"="+dir)
			var buf bytes.Buffer
			cmd.Stdout = &buf
			cmd.Stderr = &buf
			errs[i] = cmd.Run()
			outs[i] = buf.String()
		}(i)
	}

	// Release only once every child is parked on the barrier.
	deadline := time.Now().Add(60 * time.Second)
	for {
		matches, _ := filepath.Glob(filepath.Join(dir, "ready.*"))
		if len(matches) == n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d children reported ready", len(matches), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if err := os.WriteFile(filepath.Join(dir, "barrier"), nil, 0600); err != nil {
		t.Fatalf("releasing the barrier: %v", err)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Errorf("concurrent Open #%d failed: %v\n%s", i, errs[i], outs[i])
		}
	}
}
