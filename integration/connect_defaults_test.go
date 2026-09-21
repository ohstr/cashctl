//go:build integration

package integration

import "testing"

func fakeNWCURI(t *testing.T) string {
	t.Helper()
	return "nostr+walletconnect://" + fakeHex32(t) + "?relay=wss%3A%2F%2Ffake.invalid&secret=" + fakeHex32(t)
}

// A taken name can never succeed on retry — the caller must pick another —
// so it must not be reported as `conflict`, which AGENTS.md documents as
// retryable (an agent that backs off and retries would loop forever).
func TestConnectAdd_DuplicateName_IsNotRetryable(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("connect", "add", "work", fakeNWCURI(t))

	res := f.run("connect", "add", "work", fakeNWCURI(t))
	if res.ExitCode == 0 {
		t.Fatal("a duplicate name was accepted")
	}
	if e := parseErrorReport(t, res); e.Retryable || e.Code != "invalid_input" {
		t.Errorf("duplicate name reported as code=%q retryable=%v, want invalid_input, not retryable\nstderr: %s", e.Code, e.Retryable, res.Stderr)
	}
	list := f.mustJSON("connect", "list")
	if conns, _ := list["connections"].([]any); len(conns) != 1 {
		t.Errorf("connections = %d after a rejected duplicate, want 1", len(conns))
	}
}

// --yes means "don't ask", not "yes, change which wallet my money goes to":
// registering a second wallet must never silently replace the default.
// (--json has always behaved this way; --yes in text mode did not.)
func TestConnectAdd_YesDoesNotReplaceAnExistingDefault(t *testing.T) {
	f := newFixture(t)
	if res := f.runInteractive("\n", "connect", "add", "first", fakeNWCURI(t)); res.ExitCode != 0 {
		t.Fatalf("first add: exit %d\n%s", res.ExitCode, res.Stderr)
	}
	if res := f.runInteractive("", "--yes", "connect", "add", "second", fakeNWCURI(t)); res.ExitCode != 0 {
		t.Fatalf("second add --yes: exit %d\n%s", res.ExitCode, res.Stderr)
	}
	if got := f.mustJSON("connect", "list")["default"]; got != "first" {
		t.Errorf("default = %v after `connect add second --yes`, want it to stay %q", got, "first")
	}
}

// The first wallet ever registered still becomes the default under --yes:
// there's no existing default to protect, and "no wallet configured" right
// after adding one would just be a dead end.
func TestConnectAdd_YesStillDefaultsTheFirstWallet(t *testing.T) {
	f := newFixture(t)
	if res := f.runInteractive("", "--yes", "connect", "add", "only", fakeNWCURI(t)); res.ExitCode != 0 {
		t.Fatalf("add --yes: exit %d\n%s", res.ExitCode, res.Stderr)
	}
	if got := f.mustJSON("connect", "list")["default"]; got != "only" {
		t.Errorf("default = %v, want %q", got, "only")
	}
}
