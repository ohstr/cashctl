package pocket

// Adversarial attack sweep on the pocket's funds surface (#18 campaign).
// Each test is labelled DEFENDED (the guard holds) or, where a gap was found,
// documents the fix that closed it. Concurrency (#14) and serve-AUTH are proven
// by concurrency_test.go and cmd/TestCashPocketServe_...; crash/stranding by the
// parameterized recovery/write-ahead/settle suite (green toggle-on). This file
// covers the sync-IMPORT attack surface (sig forgery, foreign binding, state
// forgery, rev-rollback) that reverse-projects pulled 9525s into spendable bills.

import (
	"context"
	"testing"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nip59"
	"github.com/ohstr/nmilat/nipCP"
	"github.com/ohstr/nmilat/nipcash"

	"github.com/ohstr/cashctl/internal/billform"
	"github.com/ohstr/cashctl/testsupport/testmint"
)

// openPocketWithKey opens a throwaway pocket bound to a KNOWN private key, so a
// test can mint entries bound to the matching pubkey and import them.
func openPocketWithKey(t *testing.T, priv string) *Pocket {
	t.Helper()
	p, err := Open(context.Background(), t.TempDir(), priv, "flokicoin")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// heldPocketEntry builds a pubkey-bound held-token nipCP.Entry for userPub at rev
// with the given state, backed by a real (offline-signed) mint bill. Each call
// mints a fresh bill, so its wallet pubkey W is unique.
func heldPocketEntry(t *testing.T, userPub string, rev uint64, state nipCP.State) (nipCP.Entry, string) {
	t.Helper()
	amt := uint64(5000)
	tok := testmint.Bill(t, nipcash.Token{HRP: "lokicash", AttestedAmountMillis: &amt}, "")
	w, err := billform.WalletPubkey(tok)
	if err != nil {
		t.Fatalf("wallet pubkey: %v", err)
	}
	return nipCP.Entry{
		W: w, Rev: rev, Cash: tok, Amount: amt, Chain: "flokicoin",
		Identity: nipCP.EntryIdentity{Type: nipCP.IdentityPubkey, Value: userPub},
		State:    state,
	}, w
}

// Attack 4a — FOREIGN-BOUND bill served to the user: DEFENDED.
// A relay serves a held bill bound to SOMEONE ELSE's pubkey; entryFromPocket must
// refuse it (the user's key can't sign for it), so it never becomes spendable.
func TestAttack_ForeignBoundBillNotImportable(t *testing.T) {
	_, userPub := testKey(t)
	_, attackerPub := testKey(t)
	e, _ := heldPocketEntry(t, attackerPub, 1, nipCP.StateLive) // bound to the attacker
	if _, ok := entryFromPocket(e, userPub); ok {
		t.Fatal("FINDING: a foreign-pubkey-bound bill was importable as the user's own (theft/double-spend surface)")
	}
}

// Attack 4b — non-LIVE bill forged as importable: DEFENDED.
// A spent/expired/reclaimed/pending entry must never reverse-project into the
// spendable ledger, no matter which key it is bound to.
func TestAttack_NonLiveBillNotImportable(t *testing.T) {
	_, userPub := testKey(t)
	for _, st := range []nipCP.State{nipCP.StateSpent, nipCP.StateExpired, nipCP.StateReclaimed, nipCP.StatePending} {
		e, _ := heldPocketEntry(t, userPub, 1, st)
		if _, ok := entryFromPocket(e, userPub); ok {
			t.Fatalf("FINDING: a %q bill was importable as spendable", st)
		}
	}
}

// Attack 4c — FORGED / FOREIGN-SEALED wrap injected by a malicious relay: DEFENDED.
// The pull only accepts wraps the user themselves sealed. nmilat's Seal is
// encrypt-to-self (the wrap's recipient is the signer), so a wrap sealed with an
// attacker key cannot even be decrypted with the user's key; and were one crafted
// to decrypt, nipCP.Open rejects it because the seal signer != user. Either way
// OpenAll discards it, so nothing foreign is injected on a hostile relay.
func TestAttack_ForeignSealedWrapSkipped(t *testing.T) {
	userPriv, userPub := testKey(t)
	attackerPriv, _ := testKey(t)

	// Attacker seals a well-formed held-token wrap (addressed to the user) with ITS key.
	e, _ := heldPocketEntry(t, userPub, 1, nipCP.StateLive)
	e.CheckedAt = 1700000000
	wrap, _, err := nipCP.Seal(e, attackerPriv)
	if err != nil {
		t.Fatalf("attacker seal: %v", err)
	}
	if wrap.Kind != nip59.KindGiftWrap {
		t.Fatalf("not a gift wrap: kind %d", wrap.Kind)
	}

	opened, err := nipCP.OpenAll([]*nip01.Event{wrap}, userPriv)
	if err != nil {
		t.Fatalf("OpenAll: %v", err)
	}
	if len(opened) != 0 {
		t.Fatalf("FINDING: a wrap sealed by a foreign key was opened as the user's entry (%d) — sync injection surface", len(opened))
	}
}

// Attack 4d — REV-ROLLBACK / stale-live replay: FINDING (Hub-backstopped, MED) — FIXED.
// A hostile/stale relay replays the user's OWN old rev-1 `live` wrap for bill w
// alongside the current rev-2 `spent` wrap. nipCP.Sync returns RAW opened entries
// (sync.go: out.Entries = entries, unresolved) and the sync caller passed them
// straight to ImportHeld, which imported the first *live* entry per w — so the
// superseded bill became phantom-spendable (defeating rev-resolution at the import
// layer). No double-spend resulted (the Hub rejects spending an already-spent
// bill), but it is a real correctness/UX gap on the funds surface.
// FIX (project.go ImportHeld): resolve to the per-wallet winner BEFORE importing,
// so a `spent` winner shadows the stale `live`. This test asserts the fixed,
// safe behaviour and a positive control that genuine live bills still import.
func TestAttack_SyncRollback_StaleLiveNotImportable(t *testing.T) {
	priv, userPub := testKey(t)
	ctx := context.Background()

	liveRev1, w := heldPocketEntry(t, userPub, 1, nipCP.StateLive)
	spentRev2 := liveRev1
	spentRev2.Rev = 2
	spentRev2.State = nipCP.StateSpent

	// Resolution says the winner is SPENT.
	winners, _ := nipCP.Resolve([]nipCP.Entry{spentRev2, liveRev1})
	if got := winners[w].State; got != nipCP.StateSpent {
		t.Fatalf("resolution winner = %q, want spent", got)
	}

	// Under attack: both wraps are served. ImportHeld must resolve first and import
	// NOTHING (the winner is spent). Pre-fix this imported the stale rev-1 live as
	// spendable (the finding).
	pAttack := openPocketWithKey(t, priv)
	nAttack, err := pAttack.ImportHeld(ctx, []nipCP.Entry{spentRev2, liveRev1})
	if err != nil {
		t.Fatalf("import under attack: %v", err)
	}
	if nAttack != 0 {
		t.Fatalf("FINDING (rev-rollback): a stale rev-1 live superseded by a spent winner was imported as spendable (%d); ImportHeld must resolve before importing", nAttack)
	}

	// Positive control: a genuine live winner with no superseding spent DOES import,
	// so the fix did not over-reject.
	liveOnly, _ := heldPocketEntry(t, userPub, 1, nipCP.StateLive)
	pGood := openPocketWithKey(t, priv)
	nGood, err := pGood.ImportHeld(ctx, []nipCP.Entry{liveOnly})
	if err != nil {
		t.Fatalf("import live-only: %v", err)
	}
	if nGood != 1 {
		t.Fatalf("regression: a genuine live winner was not imported (%d)", nGood)
	}
}

// Attack 4e — REV-ROLLBACK via RELAY OMISSION: FINDING (review, MED) — FIXED.
// Sharper than 4d: the superseding `spent` is NOT in the pulled batch at all — a
// lossy/hostile relay returns ONLY the user's stale rev-1 `live` wrap and omits the
// rev-2 `spent` the device synced earlier. Resolving the batch in isolation (the
// pre-fix behaviour) could not see the spent, so the stale live imported as
// phantom-spendable. FIX (ImportHeld): resolve the batch UNIONED with the device's
// own pocket.db knowledge, so the known `spent` shadows the replayed `live`.
func TestAttack_SyncRollback_DeviceKnownSpentShadowsOmittedReplay(t *testing.T) {
	priv, userPub := testKey(t)
	ctx := context.Background()

	liveRev1, _ := heldPocketEntry(t, userPub, 1, nipCP.StateLive)
	liveRev1.CheckedAt = 1700000000
	spentRev2 := liveRev1
	spentRev2.Rev = 2
	spentRev2.State = nipCP.StateSpent
	spentRev2.CheckedAt = 1700000001

	// The device synced this bill earlier and stored BOTH revs in its OWN pocket.db;
	// the winner it already knows is SPENT.
	p := openPocketWithKey(t, priv)
	if err := p.Add(ctx, liveRev1); err != nil {
		t.Fatalf("store rev1 in pocket.db: %v", err)
	}
	if err := p.Add(ctx, spentRev2); err != nil {
		t.Fatalf("store rev2 in pocket.db: %v", err)
	}

	// A later sync from a lossy/hostile relay returns ONLY the stale rev-1 live wrap.
	// ImportHeld must resolve it against the device's own pocket.db and import NOTHING.
	n, err := p.ImportHeld(ctx, []nipCP.Entry{liveRev1})
	if err != nil {
		t.Fatalf("import under omission attack: %v", err)
	}
	if n != 0 {
		t.Fatalf("FINDING (relay-omission rev-rollback): a stale live the device itself knows is spent was imported as spendable (%d); ImportHeld must resolve against its own pocket.db, not the pulled batch alone", n)
	}

	// Positive control: a bill the device knows only as live still imports.
	liveOnly, _ := heldPocketEntry(t, userPub, 1, nipCP.StateLive)
	liveOnly.CheckedAt = 1700000000
	pGood := openPocketWithKey(t, priv)
	if err := pGood.Add(ctx, liveOnly); err != nil {
		t.Fatalf("store live-only: %v", err)
	}
	nGood, err := pGood.ImportHeld(ctx, []nipCP.Entry{liveOnly})
	if err != nil {
		t.Fatalf("import live-only: %v", err)
	}
	if nGood != 1 {
		t.Fatalf("regression: a genuine live bill (no known spent) was not imported (%d)", nGood)
	}
}
