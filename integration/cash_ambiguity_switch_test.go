//go:build integration

// cash_ambiguity_switch_test.go covers #19's F4 (docs/private/
// amount-first-decisions.md, question 4): wallet protect and cash status
// move from refusing an ambiguous selection to processing/showing every
// eligible candidate, matching consolidate's own existing pattern — through
// the real compiled binary, not just the unit-level resolver/printer tests
// in cmd/.
package integration

import (
	"strings"
	"testing"
)

// TestWalletProtect_MultipleUnprotected_JSONModeProtectsAll is the live
// proof of the switch: two unprotected cash-mode holdings, none named,
// under --json — the case that used to be a usage refusal (ExitCode 2) and
// now re-keys both in one call.
func TestWalletProtect_MultipleUnprotected_JSONModeProtectsAll(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	f.mustJSON("wallet", "init")

	f.mustJSON("receive", mintCashGift(t, hub, 2_000))
	f.mustJSON("receive", mintCashGift(t, hub, 3_000))

	res := f.run("wallet", "protect", "--yes")
	if res.ExitCode != 0 {
		t.Fatalf("wallet protect with 2 unprotected holdings, none named: exit=%d\nstdout=%s\nstderr=%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	// Multi-outcome shape: a "protected" array, not the old bare object —
	// see printProtectOutcomes' own doc comment for why the single-success
	// shape stays bare and only this case gains the array.
	if !strings.Contains(res.Stdout, `"protected"`) {
		t.Fatalf("expected a protected outcome array in stdout: %s", res.Stdout)
	}

	show := f.mustJSON("wallet", "show")
	held, _ := show["held_tokens"].([]any)
	for _, h := range held {
		entry, _ := h.(map[string]any)
		if prot, _ := entry["cash_protection"].(string); prot != "protected" {
			t.Errorf("holding not protected after wallet protect processed all: %v", entry)
		}
	}
}

// TestCashStatus_MultipleHeld_JSONModeShowsAll is the same live proof for
// cash status: read-only, so the switch is even less contentious than
// protect's — two held tokens, none named, under --json now reports both
// instead of refusing.
func TestCashStatus_MultipleHeld_JSONModeShowsAll(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 2_000))
	f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 3_000))

	res := f.run("cash", "status") // f.run always passes --json itself
	if res.ExitCode != 0 {
		t.Fatalf("cash status with 2 held tokens, none named: exit=%d\nstdout=%s\nstderr=%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stdout, `"statuses"`) {
		t.Fatalf("expected a \"statuses\" array in stdout for the multi-entry case: %s", res.Stdout)
	}
}

// TestWalletProtect_SingleUnprotected_KeepsOldBareShape confirms the one
// case that must NOT change: an explicit --token (or auto-pick with
// exactly one eligible) still gets today's exact bare {"protected": {...}}
// object, no array, no "id" — every existing script's own parsing stays
// correct.
func TestWalletProtect_SingleUnprotected_KeepsOldBareShape(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	f.mustJSON("wallet", "init")
	f.mustJSON("receive", mintCashGift(t, hub, 2_000))

	res := f.run("wallet", "protect", "--yes")
	if res.ExitCode != 0 {
		t.Fatalf("wallet protect with exactly one unprotected holding: exit=%d\nstderr=%s", res.ExitCode, res.Stderr)
	}
	if strings.Contains(res.Stdout, `"id"`) {
		t.Errorf("single-success shape carries an \"id\" field — want the unchanged bare object: %s", res.Stdout)
	}
}
