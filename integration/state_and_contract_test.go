//go:build integration

package integration

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// Offline checks of local-state handling and the machine contract, found by
// the campaign's E1 (identity/meta) executor and re-verified here against the
// compiled binary with worthless fake values.

// errorReport is the --json failure shape on stderr (AGENTS.md).
type errorReport struct {
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
}

func parseErrorReport(t *testing.T, res result) errorReport {
	t.Helper()
	var e errorReport
	if err := json.Unmarshal([]byte(res.Stderr), &e); err != nil {
		t.Fatalf("stderr is not a JSON error report: %v\nstderr: %s", err, res.Stderr)
	}
	return e
}

// A connection value that is not even a valid connection string is the
// caller's mistake: invalid_input/not_found, never `network` with
// retryable:true (an agent that backs off and retries would loop on a typo).
func TestErrors_UnparseableConnection_IsNotRetryableNetwork(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")
	for _, args := range [][]string{
		{"-c", "garbage", "wallet", "get-info"},
		{"-c", "no-such-registered-name", "wallet", "budget"},
		{"redeem", "--invoice", "lnfc1x", "--yes"},
	} {
		res := f.run(args...)
		if res.ExitCode == 0 {
			t.Errorf("cashctl %v unexpectedly succeeded", args)
			continue
		}
		if e := parseErrorReport(t, res); e.Code == "network" || e.Retryable {
			t.Errorf("cashctl %v: reported as code=%q retryable=%v for input that can never work; want a non-retryable invalid_input/not_found\nstderr: %s",
				args, e.Code, e.Retryable, res.Stderr)
		}
	}
}

// `init` offers to register a default wallet; anything typed at that prompt
// must not be saved as a wallet unless it is a wallet connection.
func TestInit_DefaultWalletPrompt_DoesNotSaveGarbage(t *testing.T) {
	f := newFixture(t)
	res := f.runInteractive("n\n", "init")
	if res.ExitCode != 0 {
		t.Fatalf("init: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	list := mustDecodeJSON(t, "connect list", f.run("connect", "list").Stdout)
	if conns, _ := list["connections"].([]any); len(conns) != 0 {
		t.Errorf("answering `n` at init's default-wallet prompt saved %d connection(s): %v", len(conns), conns)
	}
}

// A --config-dir containing URL-significant characters must work like any
// other path: `?` and `#` used to be parsed as part of the SQLite URI, so the
// database was created at a truncated path (with default permissions) and
// init then failed.
func TestConfigDir_URLSignificantCharacters(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"q?mark", "h#ash", "p%41ct"} {
		parent := t.TempDir()
		dir := filepath.Join(parent, name)
		res := f.rawRun("--config-dir", dir, "--json", "init")
		if res.ExitCode != 0 {
			t.Errorf("--config-dir %q: init exit %d\nstderr: %s", name, res.ExitCode, res.Stderr)
		}
		if _, err := os.Stat(filepath.Join(dir, "cashctl.db")); err != nil {
			t.Errorf("--config-dir %q: no cashctl.db inside the requested directory: %v", name, err)
		}
		entries, _ := os.ReadDir(parent)
		if len(entries) != 1 || entries[0].Name() != name {
			var got []string
			for _, e := range entries {
				got = append(got, e.Name())
			}
			t.Errorf("--config-dir %q: state was written outside the requested directory: %v", name, got)
		}
	}
}

// Eight `connect add` calls at once must register eight wallets. Each
// process saves a snapshot of the whole table, so concurrent writers used to
// silently drop each other's registrations while all exiting 0.
func TestConnect_ParallelAdds_AllRegistered(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")
	const n = 8
	var calls [][]string
	for i := 0; i < n; i++ {
		uri := "nostr+walletconnect://" + fakeHex32(t) + "?relay=wss%3A%2F%2Ffake.invalid&secret=" + fakeHex32(t)
		calls = append(calls, []string{"connect", "add", "w" + string(rune('a'+i)), uri})
	}
	for i, res := range runConcurrently(f, calls...) {
		if res.ExitCode != 0 {
			t.Fatalf("connect add #%d: exit %d\nstderr: %s", i, res.ExitCode, res.Stderr)
		}
	}
	list := mustDecodeJSON(t, "connect list", f.run("connect", "list").Stdout)
	if conns, _ := list["connections"].([]any); len(conns) != n {
		t.Errorf("%d concurrent `connect add` calls all exited 0 but only %d wallets are registered", n, len(conns))
	}
}

// Five first-time `init`s racing on one fresh dir must leave exactly one
// identity, and no caller may be told about a different one.
func TestInit_ParallelFirstRun_OneIdentity(t *testing.T) {
	f := newFixture(t)
	calls := make([][]string, 5)
	for i := range calls {
		calls[i] = []string{"init"}
	}
	printed := map[string]bool{}
	for _, res := range runConcurrently(f, calls...) {
		if res.ExitCode != 0 {
			t.Fatalf("init: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
		}
		if npub, _ := mustDecodeJSON(t, "init", res.Stdout)["npub"].(string); npub != "" {
			printed[npub] = true
		}
	}
	survivor, _ := f.mustJSON("wallet", "show")["npub"].(string)
	for npub := range printed {
		if npub != survivor {
			t.Errorf("a concurrent `init` reported identity %s… but the wallet ended up with %s… — funds sent to the reported one would be unspendable",
				npub[:14], survivor[:14])
		}
	}
}

// --json output for an empty collection must be `[]`, not `null`: `jq '.[]'`
// and typed decoders choke on null.
func TestJSON_EmptyCollectionsAreArrays(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")
	show := f.mustJSON("wallet", "show")
	for _, key := range []string{"held_tokens", "wallets"} {
		if v, ok := show[key]; !ok || v == nil {
			t.Errorf("wallet show --json: %q is %v, want []", key, v)
		}
	}
	hist := f.mustJSON("wallet", "history")
	if v, ok := hist["history"]; !ok || v == nil {
		t.Errorf("wallet history --json: \"history\" is %v, want []", v)
	}
}

// If the ledger's local write fails AFTER a money-moving Hub call has
// already succeeded (disk full, quota, a locked/read-only volume), the
// result must not be silently discarded: cashctl must fail loudly and,
// where it has one, still surface the new token/secret the Hub already
// handed back — never report success while quietly losing the money's own
// record. Simulated with a SQLite trigger that fails ordinary UPDATEs on the
// entries table, the same fault a full disk or a read-only mount produces.
func TestTransfer_LedgerSaveFailureAfterHubSuccess_IsNotSilentlyDiscarded(t *testing.T) {
	f := newFixture(t)
	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 5_000))

	dbPath := filepath.Join(f.configDir, "cashctl.db")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open cashctl.db directly: %v", err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`CREATE TRIGGER zz_block_update BEFORE UPDATE ON entries BEGIN SELECT RAISE(ABORT, 'database or disk is full'); END`,
		`CREATE TRIGGER zz_block_insert BEFORE INSERT ON entries BEGIN SELECT RAISE(ABORT, 'database or disk is full'); END`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("install trigger: %v", err)
		}
	}

	res := f.run("transfer", fakeHex32(t), "2", "--yes") // splits a 5-loki token
	if res.ExitCode == 0 {
		t.Fatalf("transfer succeeded despite the ledger write being blocked (trigger not installed?): %s", res.Stdout)
	}
	if strings.Contains(res.Stderr, "already in your ledger") {
		t.Errorf("a blocked local write (disk full) was misclassified as ErrAlreadyHeld:\n%s", res.Stderr)
	}
	// The recovery tokens belong in the error report on stderr, not stdout
	// — stdout stays reserved for a --json success shape (AGENTS.md:
	// "Every command's result goes to stdout only").
	if res.Stdout != "" {
		t.Errorf("transfer failed but still wrote to stdout: %q", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "lokicash") {
		t.Errorf("transfer failed after the Hub had already split the token, but neither the recipient's new token nor the remainder was surfaced anywhere for recovery:\nexit=%d stdout=%q stderr=%q",
			res.ExitCode, res.Stdout, res.Stderr)
	}
}
