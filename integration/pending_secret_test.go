//go:build integration

package integration

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// These tests cover the fix for a real fund-loss bug: protectBearerReceipt
// generates a bearer target's replacement secret purely locally (NIP-CASH
// never returns it — only a one-way commitment ever crosses the wire), so
// it can and now does persist that candidate as PendingBearerSecret BEFORE
// placing the wire call that's meant to confirm it. A kill or lost response
// during that call can leave genuinely ambiguous which of the two secrets
// the Hub actually accepted — NIP-CASH has no read-only way to ask
// (nipcashclient.CheckClaim's own doc comment) — so cashctl can only find
// out by trying: resolveCredential's callers (redeem, transfer) retry once
// with PendingBearerSecret when the usual secret is declined as wrong.
//
// Reproducing the exact kill-timing race against a live Hub is inherently
// flaky (a ~250ms window in a ~400ms operation, per the campaign's own
// report). These tests instead reconstruct the ambiguous ON-DISK STATE
// directly — deterministic, and it's the actual data shape a kill leaves
// behind, whichever secret really won — and confirm cashctl recovers the
// funds from it either way.

// currentBearerSecret reads entryID's live bearer_secret column directly —
// receive's own --json output never includes it (Entry.BearerSecret is
// json:"-"), so this is the only way a test can learn the real secret
// protect just established.
func currentBearerSecret(t *testing.T, f *fixture, entryID string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.configDir, "cashctl.db"))
	if err != nil {
		t.Fatalf("open cashctl.db directly: %v", err)
	}
	defer db.Close()
	var secret string
	if err := db.QueryRow(`SELECT bearer_secret FROM entries WHERE id = ?`, entryID).Scan(&secret); err != nil {
		t.Fatalf("querying bearer_secret: %v", err)
	}
	return secret
}

// corruptBearerSecret opens f's cashctl.db directly and rewrites the given
// held entry's bearer_secret/pending_bearer_secret columns — simulating the
// on-disk state left by an interrupted protect step, without needing to
// actually race a kill against the network.
func corruptBearerSecret(t *testing.T, f *fixture, entryID, bearerSecret, pendingSecret string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.configDir, "cashctl.db"))
	if err != nil {
		t.Fatalf("open cashctl.db directly: %v", err)
	}
	defer db.Close()
	res, err := db.Exec(`UPDATE entries SET bearer_secret = ?, pending_bearer_secret = ? WHERE id = ?`, bearerSecret, pendingSecret, entryID)
	if err != nil {
		t.Fatalf("corrupt entries row: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("corrupt entries row: matched %d rows, want 1", n)
	}
}

// receiveAndScrambleBearerSecret receives gift (protect runs normally,
// under --json, and rekeys it to a fresh, genuinely live secret), then
// simulates an interrupted rekey's on-disk aftermath: bearer_secret is
// overwritten with a dead value, and the real, still-live secret protect
// just established is moved into pending_bearer_secret instead — exactly
// the shape a kill between protectRekeyOnly's write-ahead save and its own
// final promotion leaves behind, regardless of which save actually landed.
func receiveAndScrambleBearerSecret(t *testing.T, f *fixture, gift string) (id string) {
	t.Helper()
	recv := f.mustJSON("receive", gift)
	secured, _ := recv["secured"].(map[string]any)
	if secured["status"] != "rekeyed" {
		t.Fatalf("secured.status = %v, want rekeyed (test fixture assumption)", secured)
	}
	id = entryID(t, recv["entry"])
	real := currentBearerSecret(t, f, id)
	corruptBearerSecret(t, f, id, strings.Repeat("d", 64), real)
	return id
}

// TestPendingSecretFallback_RedeemRecoversWhenPendingSecretIsTheRealOne is
// the direct regression test: an entry whose bearer_secret column is stale
// (the OLD, now-dead secret an interrupted rekey retired) but whose
// pending_bearer_secret column holds the secret the Hub actually accepted
// must still redeem for real, not report "wrong secret" and give up.
func TestPendingSecretFallback_RedeemRecoversWhenPendingSecretIsTheRealOne(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)

	const amount = uint64(8_000)
	receiveAndScrambleBearerSecret(t, f, mintBearerGift(t, hub, amount))

	inv := makeHubInvoice(t, hub, amount)
	res := f.run("redeem", "--invoice", inv, "--yes")
	if res.ExitCode != 0 {
		t.Fatalf("redeem with a dead bearer_secret but a live pending_bearer_secret: exit %d — the fallback did not recover the real secret\nstdout: %s\nstderr: %s",
			res.ExitCode, res.Stdout, res.Stderr)
	}
	out := mustDecodeJSON(t, "redeem", res.Stdout)
	if pre, _ := out["preimage"].(string); pre == "" {
		t.Errorf("redeem succeeded but no preimage: %v", out)
	}
}

// TestPendingSecretFallback_TransferRecoversWhenPendingSecretIsTheRealOne is
// the same recovery, via transfer instead of redeem — the other single-
// entry spend path that goes through resolveCredential.
func TestPendingSecretFallback_TransferRecoversWhenPendingSecretIsTheRealOne(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)

	receiveAndScrambleBearerSecret(t, f, mintBearerGift(t, hub, 6_000))

	res := f.run("transfer", fakeHex32(t), "--yes")
	if res.ExitCode != 0 {
		t.Fatalf("transfer with a dead bearer_secret but a live pending_bearer_secret: exit %d — the fallback did not recover the real secret\nstdout: %s\nstderr: %s",
			res.ExitCode, res.Stdout, res.Stderr)
	}
}

// TestPendingSecretFallback_DoesNotMaskAGenuinelyWrongSecret confirms the
// fallback isn't a blanket "ignore bearer_secret" escape hatch: when
// NEITHER value is live any more (both genuinely wrong/spent), the spend
// must still fail, with the original decline reported.
func TestPendingSecretFallback_DoesNotMaskAGenuinelyWrongSecret(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)

	recv := f.mustJSON("receive", mintBearerGift(t, hub, 4_000))
	id := entryID(t, recv["entry"])
	corruptBearerSecret(t, f, id, strings.Repeat("1", 64), strings.Repeat("2", 64))

	res := f.run("transfer", fakeHex32(t), "--yes")
	if res.ExitCode == 0 {
		t.Fatalf("transfer succeeded with two genuinely wrong secrets: %s", res.Stdout)
	}
	if e := parseErrorReport(t, res); e.Code != "not_found" {
		t.Errorf("code = %q, want not_found (the original decline, not something the retry attempt invented)", e.Code)
	}
}

// TestReceive_ProtectRekeyOnly_HappyPath_LeavesNoPendingSecret pins the
// unchanged common case: a normal, uninterrupted protect leaves BearerSecret
// as the fresh secret and PendingBearerSecret empty — the write-ahead field
// is write-only scaffolding for the interrupted case, invisible otherwise.
func TestReceive_ProtectRekeyOnly_HappyPath_LeavesNoPendingSecret(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)

	recv := f.mustJSON("receive", mintBearerGift(t, hub, 5_000))
	secured, _ := recv["secured"].(map[string]any)
	if secured["status"] != "rekeyed" {
		t.Fatalf("secured.status = %v, want rekeyed", secured)
	}

	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.configDir, "cashctl.db"))
	if err != nil {
		t.Fatalf("open cashctl.db directly: %v", err)
	}
	defer db.Close()
	var pending sql.NullString
	if err := db.QueryRow(`SELECT pending_bearer_secret FROM entries WHERE id = ?`, entryID(t, recv["entry"])).Scan(&pending); err != nil {
		t.Fatalf("querying pending_bearer_secret: %v", err)
	}
	if pending.Valid && pending.String != "" {
		t.Errorf("pending_bearer_secret = %q after an uninterrupted protect, want empty", pending.String)
	}
}
