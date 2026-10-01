//go:build integration

package integration

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCashConsolidate_TwoHubsOnOneNodeBothMerge is the payoff of grouping by Cash
// Hub rather than by minter, and it is the normal shape rather than an edge case:
// one Lightning node, several Hubs, so every bill shares a minter.
//
// Before the hub-group fingerprint existed, cashctl had nothing in a token that
// named the issuing Hub, so `consolidate` with no arguments grouped by minter, put
// all four bills in one group, and the Hub refused the lot — an ordinary tidy-up
// failing with a protocol error the holder had not caused. Now each Hub is its own
// group and both merge.
//
// This also makes the multi-group path REACHABLE for the first time on a
// single-node stack, which is what the non-abort test below depends on.
func TestCashConsolidate_TwoHubsOnOneNodeBothMerge(t *testing.T) {
	admin, hubA := plainClientHub(t)
	hubB := setUpCashHub(t, admin)

	f := newFixture(t)
	initResp := f.mustJSON("wallet", "init")
	myPubHex, err := npubToHex(initResp["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	const a1, a2 = uint64(15_000), uint64(25_000)
	const b1, b2 = uint64(35_000), uint64(45_000)

	receive := func(token string) {
		t.Helper()
		if res := f.run("receive", token); res.ExitCode != 0 {
			t.Fatalf("receive: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
		}
	}
	receive(mintPubkeyTokenFromHub(t, hubA, myPubHex, a1))
	receive(mintPubkeyTokenFromHub(t, hubA, myPubHex, a2))
	receive(mintPubkeyTokenFromHub(t, hubB, myPubHex, b1))
	receive(mintPubkeyTokenFromHub(t, hubB, myPubHex, b2))

	res := f.run("consolidate", "--json", "--yes")
	if res.ExitCode != 0 {
		t.Fatalf("bare consolidate across two Hubs of one node must succeed, both groups merging: exit %d\nstdout: %s\nstderr: %s",
			res.ExitCode, res.Stdout, res.Stderr)
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(res.Stdout), &resp); err != nil {
		t.Fatalf("consolidate --json produced no readable report: %v\n%s", err, res.Stdout)
	}
	rows, _ := resp["consolidated"].([]any)
	if len(rows) != 2 {
		t.Fatalf("want one row per Hub (2), got %d: %v", len(rows), resp)
	}

	// Each Hub's bills merged into their own total, and the two Hubs are reported
	// distinguishably — a shared minter would have collapsed them into one name.
	totals := map[uint64]bool{}
	hubs := map[string]bool{}
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if status, _ := row["status"].(string); status != "ok" {
			t.Fatalf("group did not merge: %v", row)
		}
		hub, _ := row["hub"].(string)
		if hub == "" {
			t.Error("a group must name its Cash Hub, or a multi-group run is unreadable")
		}
		hubs[hub] = true
		newEntry, _ := row["new_entry"].(map[string]any)
		amt, _ := newEntry["amount_millis"].(float64)
		totals[uint64(amt)] = true
	}
	if len(hubs) != 2 {
		t.Errorf("both groups reported the same Hub (%v); they are different Hubs", hubs)
	}
	if !totals[a1+a2] {
		t.Errorf("no group merged to %d (hub A's total): %v", a1+a2, totals)
	}
	if !totals[b1+b2] {
		t.Errorf("no group merged to %d (hub B's total): %v", b1+b2, totals)
	}
}

// TestCashConsolidate_OneGroupFailingDoesNotAbortTheOthers is the non-abort
// contract, with a REAL failure, end to end.
//
// It had unit coverage only, through the consolidateItemsFn seam, and could not be
// exercised against a live stack at all: grouping by minter produced exactly one
// group on a single-node deployment, so there was never a second group to keep
// going. Per-Hub grouping makes it reachable.
//
// A seam proves the loop continues when a stub returns an error. It cannot prove the
// failure a real Hub produces is even reached, that it is classified rather than
// escaping as a fatal error, or that the successful group's ledger writes survive
// alongside it.
//
// The failure is genuine and needs no fault injection, and — crucially — is knowable
// only to the Hub: the second Hub's per-wallet ceiling admits each of its bills alone
// but not their merged total, so it declines while cashctl has no local basis to
// predict it. A failure the client could foresee would test selection validation
// instead (an earlier attempt broke a group by pre-spending a source, and cashctl
// correctly rejected the whole selection locally, so the healthy group never ran).
func TestCashConsolidate_OneGroupFailingDoesNotAbortTheOthers(t *testing.T) {
	admin, hubOK := plainClientHub(t)

	const bad1, bad2 = uint64(35_000), uint64(45_000)
	// Ceiling below the merged total, above each bill: only the consolidation fails.
	hubBad := setUpCashHubOpts(t, admin, cashHubOpts{PerWalletMaxMloki: 50_000})

	f := newFixture(t)
	initResp := f.mustJSON("wallet", "init")
	myPubHex, err := npubToHex(initResp["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	const ok1, ok2 = uint64(15_000), uint64(25_000)
	ids := map[string]string{}
	receive := func(label, token string) {
		t.Helper()
		resp := f.mustJSON("receive", token)
		entry, _ := resp["entry"].(map[string]any)
		id, _ := entry["id"].(string)
		if id == "" {
			t.Fatalf("receive produced no entry id: %v", resp)
		}
		ids[label] = id
	}
	receive("ok1", mintPubkeyTokenFromHub(t, hubOK, myPubHex, ok1))
	receive("ok2", mintPubkeyTokenFromHub(t, hubOK, myPubHex, ok2))
	receive("bad1", mintPubkeyTokenFromHub(t, hubBad, myPubHex, bad1))
	receive("bad2", mintPubkeyTokenFromHub(t, hubBad, myPubHex, bad2))

	res := f.run("consolidate", "--json", "--yes")

	// Nonzero exit is CORRECT for a partial run, and stdout still carries the full
	// report — the documented contract. So the report is read first, rather than
	// treating a nonzero exit as "nothing happened".
	var resp map[string]any
	if err := json.Unmarshal([]byte(res.Stdout), &resp); err != nil {
		t.Fatalf("a partial run must still print its full report (exit %d): %v\nstdout: %s\nstderr: %s",
			res.ExitCode, err, res.Stdout, res.Stderr)
	}
	rows, _ := resp["consolidated"].([]any)
	if len(rows) != 2 {
		t.Fatalf("want one row per Hub (2), got %d: %v", len(rows), resp)
	}

	var okRows, failedRows int
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		switch status, _ := row["status"].(string); status {
		case "ok":
			okRows++
			newEntry, _ := row["new_entry"].(map[string]any)
			if amt, _ := newEntry["amount_millis"].(float64); uint64(amt) != ok1+ok2 {
				t.Errorf("the successful group merged to %v, want %d", newEntry["amount_millis"], ok1+ok2)
			}
		case "failed":
			failedRows++
			if code, _ := row["code"].(string); code == "" {
				t.Errorf("a failed group must carry a classified code: %v", row)
			}
			errText, _ := row["error"].(string)
			if errText == "" {
				t.Errorf("a failed group must carry an error: %v", row)
			}
			// The Hub's own reason, not a generic decline — this is the failure
			// cashctl could not have predicted.
			if !strings.Contains(errText, "ceiling") && !strings.Contains(errText, "exceeds") {
				t.Errorf("want the Hub's own ceiling reason, got %q", errText)
			}
		default:
			t.Errorf("unexpected group status %q: %v", status, row)
		}
	}
	if okRows != 1 || failedRows != 1 {
		t.Fatalf("want exactly one ok and one failed group, got ok=%d failed=%d: %v", okRows, failedRows, rows)
	}

	// The successful group's work must have SURVIVED the other's failure — the whole
	// point of not aborting. Read from the local ledger, which is what a later
	// command sees.
	show := f.mustJSON("wallet", "show")
	held, _ := show["held_tokens"].([]any)
	stillHeld := map[string]uint64{}
	var sawMerged bool
	for _, raw := range held {
		e, _ := raw.(map[string]any)
		id, _ := e["id"].(string)
		amt, _ := e["amount_millis"].(float64)
		stillHeld[id] = uint64(amt)
		if uint64(amt) == ok1+ok2 {
			sawMerged = true
		}
	}
	if !sawMerged {
		t.Errorf("the merged bill of %d is not held after the other group failed: %v", ok1+ok2, held)
	}
	for _, label := range []string{"ok1", "ok2"} {
		if _, ok := stillHeld[ids[label]]; ok {
			t.Errorf("merged source %s (%s) must no longer be held", label, ids[label])
		}
	}
	// And the refused group's sources are untouched — a failed group must not
	// consume what it could not merge.
	for label, want := range map[string]uint64{"bad1": bad1, "bad2": bad2} {
		got, ok := stillHeld[ids[label]]
		if !ok {
			t.Errorf("refused group's source %s must remain held: %v", label, held)
		} else if got != want {
			t.Errorf("refused group's source %s = %d, want %d untouched", label, got, want)
		}
	}
}
