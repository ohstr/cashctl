//go:build integration

package integration

import "testing"

// #100: a cash note two people hold, which one of them then re-keys. The other's
// copy no longer matches, and the Hub says so as NOT_FOUND: their bill is gone,
// so cashctl reports not_found (exit 4), not an input mistake to fix and retry.
func TestCashSecret_ANoteSomeoneElseReKeyedIsNotFound(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	gift := mintCashGift(t, hub, 4_000)

	bob := newFixture(t)
	if res := bob.runInteractive("n\n", "receive", gift); res.ExitCode != 0 {
		t.Fatalf("bob receive (decline protect): exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	show := bob.mustJSON("wallet", "show")
	held, _ := show["held_tokens"].([]any)
	if len(held) != 1 {
		t.Fatalf("bob holds %d tokens, want 1", len(held))
	}
	id := entryID(t, held[0])

	alice := newFixture(t)
	alice.mustJSON("receive", gift) // protects it: re-keyed under a secret only alice holds

	for _, args := range [][]string{
		{"cash", "status", "--token", id},
		{"redeem", "--token", id, "--yes"},
	} {
		res := bob.run(args...)
		if res.ExitCode != 4 || res.Stdout != "" {
			t.Errorf("%v: exit %d, stdout %q; want exit 4 and nothing on stdout\nstderr: %s", args, res.ExitCode, res.Stdout, res.Stderr)
			continue
		}
		if e := parseErrorReport(t, res); e.Code != "not_found" || e.Retryable {
			t.Errorf("%v: code %q retryable %v, want not_found, not retryable", args, e.Code, e.Retryable)
		}
	}
}
