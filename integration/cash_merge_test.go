//go:build integration

package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/ohstr/nmilat/nipcash"
)

// I-26: cash notes merge through a throwaway key, with no identity needed.

// cashNotes mints notes on hub and receives them into f (receive re-keys each).
func cashNotes(t *testing.T, f *fixture, hub adminCreateAppResponse, amounts ...uint64) {
	t.Helper()
	for _, a := range amounts {
		if res := f.run("receive", mintCashGift(t, hub, a)); res.ExitCode != 0 {
			t.Fatalf("receive: exit %d\n%s", res.ExitCode, res.Stderr)
		}
	}
}

func liveSlices(t *testing.T, admin *adminClient, hub adminCreateAppResponse) []adminCashWalletClaim {
	t.Helper()
	rows, err := admin.listCashWalletClaims(hub.ID)
	if err != nil {
		t.Fatal(err)
	}
	var live []adminCashWalletClaim
	for _, r := range rows {
		if r.Status == "unclaimed" {
			live = append(live, r)
		}
	}
	return live
}

func TestMerge_CashNotesWithoutAnIdentity(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t) // never runs init: there is no identity
	cashNotes(t, f, hub, 20_000, 30_000, 10_000)

	if res := f.run("consolidate"); res.ExitCode != 0 {
		t.Fatalf("consolidate: exit %d\n%s", res.ExitCode, res.Stderr)
	}
	held, _ := f.mustJSON("wallet", "show")["held_tokens"].([]any)
	if len(held) != 1 || mloki(held[0].(map[string]any), "amount_millis") != 60_000 {
		t.Fatalf("want one 60-loki held note after the merge, got %v", held)
	}
	live := liveSlices(t, admin, hub)
	if len(live) != 1 || live[0].IdentityType != "cash" || live[0].AmountMloki != 60_000 {
		t.Errorf("want one live 60-loki cash slice on the Hub, got %+v", live)
	}
}

func TestMerge_ExactSumToARecipient(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	cashNotes(t, f, hub, 20_000, 30_000)
	_, pHex := newIdentity(t)

	if res := f.run("transfer", "50", pHex, "--yes"); res.ExitCode != 0 {
		t.Fatalf("transfer: exit %d\n%s", res.ExitCode, res.Stderr)
	}
	live := liveSlices(t, admin, hub)
	if len(live) != 1 || live[0].IdentityType != "pubkey" || live[0].IdentityValue != pHex || live[0].AmountMloki != 50_000 {
		t.Errorf("want P holding all 50 loki, got %+v", live)
	}
	if n := heldCount(t, f); n != 0 {
		t.Errorf("held %d token(s) after sending everything", n)
	}
}

func TestMerge_OvershootSplitsAndTheRemainderIsSpendable(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	cashNotes(t, f, hub, 20_000, 30_000)
	_, pHex := newIdentity(t)

	if res := f.run("transfer", "40", pHex, "--yes"); res.ExitCode != 0 {
		t.Fatalf("transfer: exit %d\n%s", res.ExitCode, res.Stderr)
	}
	held, _ := f.mustJSON("wallet", "show")["held_tokens"].([]any)
	if len(held) != 1 || mloki(held[0].(map[string]any), "amount_millis") != 10_000 {
		t.Fatalf("want the 10-loki remainder held, got %v", held)
	}
	// The change is a cash note again, not left on the merge's key.
	if req, _ := held[0].(map[string]any)["identity_required"].(bool); req {
		t.Errorf("the change is not a cash note: %v", held[0])
	}
	if pm := f.mustJSON("wallet", "show")["pending_merge"]; pm != nil {
		t.Errorf("the change is reported as a merge left midway: %v", pm)
	}
	var change int
	for _, s := range liveSlices(t, admin, hub) {
		if s.IdentityType == "cash" && s.AmountMloki == 10_000 {
			change++
		}
	}
	if change != 1 {
		t.Errorf("want the 10-loki change as a live cash slice, got %+v", liveSlices(t, admin, hub))
	}
	id := entryID(t, held[0])
	if res := f.run("redeem", "--token", id, "--invoice", makeHubInvoice(t, hub, 10_000)); res.ExitCode != 0 {
		t.Errorf("the remainder could not be spent: exit %d\n%s", res.ExitCode, res.Stderr)
	}
}

// Notes from two Hubs cannot merge: the Hub refuses, and both are cash notes
// again — re-keyed, never left on the throwaway key.
func TestMerge_RefusedMergeLeavesCashNotes(t *testing.T) {
	admin := adminOrSkip(t)
	hubA, hubB := setUpCashHub(t, admin), setUpCashHub(t, admin)
	f := newFixture(t)
	giftA, giftB := mintCashGift(t, hubA, 20_000), mintCashGift(t, hubB, 30_000)
	for _, g := range []string{giftA, giftB} {
		if res := f.run("receive", g); res.ExitCode != 0 {
			t.Fatalf("receive: %s", res.Stderr)
		}
	}
	held, _ := f.mustJSON("wallet", "show")["held_tokens"].([]any)
	res := f.run("consolidate", entryID(t, held[0]), entryID(t, held[1]))
	if res.ExitCode == 0 {
		t.Fatal("a cross-Hub merge succeeded")
	}
	for _, c := range []struct {
		hub  adminCreateAppResponse
		gift string
	}{{hubA, giftA}, {hubB, giftB}} {
		_, secret := nipcash.SplitCashSliceString(c.gift)
		raw, _ := hex.DecodeString(secret)
		sum := sha256.Sum256(raw)
		live := liveSlices(t, admin, c.hub)
		if len(live) != 1 || live[0].IdentityType != "cash" || live[0].IdentityValue == hex.EncodeToString(sum[:]) {
			t.Errorf("want one live cash slice under a fresh secret, got %+v", live)
		}
	}
	if n := heldCount(t, f); n != 2 {
		t.Errorf("held %d, want both notes still held", n)
	}
	// A refused merge created no bill, so nothing is left reported as an
	// interrupted send.
	if sends, _ := f.mustJSON("wallet", "show")["unfinished_cash_sends"].([]any); len(sends) != 0 {
		t.Errorf("a refused merge left unfinished_cash_sends: %v", sends)
	}
	// Each held note must be the live slice, not the tombstoned one it was
	// spun off from when it moved back to cash.
	for _, h := range f.mustJSON("wallet", "show")["held_tokens"].([]any) {
		id := entryID(t, h)
		res := f.run("cash", "status", "--token", id)
		if res.ExitCode != 0 {
			t.Errorf("cash status %s after the refused merge: exit %d, want a live note\n%s", id, res.ExitCode, res.Stderr)
			continue
		}
		status := mustDecodeJSON(t, "cash status", res.Stdout)
		if recips, _ := status["recipients"].([]any); status["error"] != nil || len(recips) == 0 {
			t.Errorf("cash status %s after the refused merge: want a live roster, got %s", id, res.Stdout)
		}
	}
}
