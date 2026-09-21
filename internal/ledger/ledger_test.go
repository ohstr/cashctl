package ledger

import (
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/store"
)

func withTempConfigDir(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	appdir.SetOverride(tmp)
	t.Cleanup(func() { appdir.SetOverride("") })
}

func TestLoad_EmptyOnFreshInstall(t *testing.T) {
	withTempConfigDir(t)

	l, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(l.Entries) != 0 || len(l.Held()) != 0 {
		t.Error("fresh ledger is not empty")
	}
}

func TestAdd_GeneratesIDAndSetsDefaults(t *testing.T) {
	l := &Ledger{}
	entry, err := l.Add(Entry{Token: "lokicash1abc", WalletPubkey: "wp", Secret: "s"})
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if entry.ID == "" {
		t.Error("Add() did not assign an ID")
	}
	if entry.Status != StatusHeld {
		t.Errorf("Status = %q, want %q", entry.Status, StatusHeld)
	}
	if entry.Verified {
		t.Error("Verified = true on a freshly received token, want false")
	}
	if entry.ReceivedAt == "" {
		t.Error("ReceivedAt was not set")
	}
}

func TestAdd_RejectsDuplicateToken(t *testing.T) {
	l := &Ledger{}
	if _, err := l.Add(Entry{Token: "lokicash1abc"}); err != nil {
		t.Fatalf("first Add() error = %v", err)
	}
	if _, err := l.Add(Entry{Token: "lokicash1abc"}); !errors.Is(err, ErrAlreadyHeld) {
		t.Errorf("second Add() error = %v, want ErrAlreadyHeld", err)
	}
}

// bech32 is case-insensitive to decode, so the uppercase spelling of a held
// token is the same token — it used to slip in as a second entry and
// double-count the balance.
func TestAdd_RejectsDuplicateTokenDifferingOnlyInCase(t *testing.T) {
	l := &Ledger{}
	if _, err := l.Add(Entry{Token: "lokicash1abcdef"}); err != nil {
		t.Fatalf("first Add() error = %v", err)
	}
	if _, err := l.Add(Entry{Token: "LOKICASH1ABCDEF"}); !errors.Is(err, ErrAlreadyHeld) {
		t.Errorf("Add() of the uppercase spelling: error = %v, want ErrAlreadyHeld", err)
	}
	if len(l.Entries) != 1 {
		t.Errorf("ledger has %d entries, want 1", len(l.Entries))
	}
}

func TestAdd_StoresTokenLowercase(t *testing.T) {
	l := &Ledger{}
	e, err := l.Add(Entry{Token: "LOKICASH1ABCDEF"})
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if e.Token != "lokicash1abcdef" {
		t.Errorf("stored Token = %q, want it normalized to lowercase", e.Token)
	}
}

func TestFindByToken_IsCaseInsensitive_EvenForLegacyUppercaseRows(t *testing.T) {
	// A row saved before Add started normalizing is stored exactly as
	// pasted; lookups must still find it from either spelling.
	l := &Ledger{Entries: []Entry{{ID: "old", Token: "LOKICASH1LEGACY", Status: StatusHeld}}}
	for _, in := range []string{"LOKICASH1LEGACY", "lokicash1legacy"} {
		if _, ok := l.FindByToken(in); !ok {
			t.Errorf("FindByToken(%q) = not found, want the legacy uppercase row", in)
		}
	}
}

// A full-amount transfer reassigns a wallet in place, so the SAME token
// string can genuinely come back to a previous holder. entries.token is
// UNIQUE, so Add must reactivate the existing row, not reject it (and not
// try to insert a second one).
func TestAdd_ReactivatesANoLongerHeldToken(t *testing.T) {
	for _, gone := range []string{StatusTransferred, StatusRedeemed, StatusConsolidated} {
		l := &Ledger{}
		first, err := l.Add(Entry{Token: "lokicash1abc", AmountMillis: amountPtr(1000)})
		if err != nil {
			t.Fatalf("first Add() error = %v", err)
		}
		id := first.ID
		if err := l.SetStatus(id, gone); err != nil {
			t.Fatalf("SetStatus(%s) error = %v", gone, err)
		}
		back, err := l.Add(Entry{Token: "lokicash1abc", AmountMillis: amountPtr(2500), Verified: true})
		if err != nil {
			t.Fatalf("status %q: Add() of a token that came back error = %v, want it accepted", gone, err)
		}
		if back.ID != id {
			t.Errorf("status %q: reactivated ID = %q, want the existing row's %q", gone, back.ID, id)
		}
		if back.Status != StatusHeld {
			t.Errorf("status %q: reactivated Status = %q, want held", gone, back.Status)
		}
		if back.AmountMillis == nil || *back.AmountMillis != 2500 || !back.Verified {
			t.Errorf("status %q: reactivated entry didn't take the fresh fields: %+v", gone, back)
		}
		if len(l.Entries) != 1 {
			t.Errorf("status %q: ledger has %d entries, want the one row reused", gone, len(l.Entries))
		}
	}
}

func TestFindByToken(t *testing.T) {
	l := &Ledger{}
	added, _ := l.Add(Entry{Token: "lokicash1abc"})
	found, ok := l.FindByToken("lokicash1abc")
	if !ok {
		t.Fatal("FindByToken() = not found")
	}
	if found.ID != added.ID {
		t.Errorf("FindByToken() ID = %q, want %q", found.ID, added.ID)
	}
	if _, ok := l.FindByToken("lokicash1doesnotexist"); ok {
		t.Error("FindByToken() found a token that was never added")
	}
}

func TestHeld_ExcludesNonHeldStatuses(t *testing.T) {
	l := &Ledger{}
	a, _ := l.Add(Entry{Token: "t1"})
	_, _ = l.Add(Entry{Token: "t2"})
	if err := l.SetStatus(a.ID, StatusRedeemed); err != nil {
		t.Fatalf("SetStatus() error = %v", err)
	}

	held := l.Held()
	if len(held) != 1 {
		t.Fatalf("Held() returned %d entries, want 1", len(held))
	}
	if held[0].Token != "t2" {
		t.Errorf("Held()[0].Token = %q, want %q", held[0].Token, "t2")
	}
}

func TestSetStatus_UnknownIDErrors(t *testing.T) {
	l := &Ledger{}
	if err := l.SetStatus("tok-doesnotexist", StatusRedeemed); err == nil {
		t.Error("expected an error setting status on an unknown entry")
	}
}

func TestSetVerified(t *testing.T) {
	l := &Ledger{}
	e, _ := l.Add(Entry{Token: "t1"})
	if e.Verified {
		t.Fatal("precondition failed: entry starts verified")
	}
	if err := l.SetVerified(e.ID, true); err != nil {
		t.Fatalf("SetVerified() error = %v", err)
	}
	found, _ := l.Find(e.ID)
	if !found.Verified {
		t.Error("Verified was not updated")
	}
}

func TestNewID_NoCollisions(t *testing.T) {
	l := &Ledger{}
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		e, err := l.Add(Entry{Token: "t" + string(rune('a'+i%26)) + string(rune(i))})
		if err != nil {
			t.Fatalf("Add() error = %v", err)
		}
		if seen[e.ID] {
			t.Fatalf("duplicate ID generated: %q", e.ID)
		}
		seen[e.ID] = true
	}
}

func TestAppendHistory(t *testing.T) {
	l := &Ledger{}
	l.AppendHistory("receive", "received 20000 mloki")
	if len(l.History) != 1 {
		t.Fatalf("History length = %d, want 1", len(l.History))
	}
	if l.History[0].Action != "receive" || l.History[0].At == "" {
		t.Errorf("History[0] = %+v, unexpected", l.History[0])
	}
}

func TestSaveAndLoad_RoundTrip(t *testing.T) {
	withTempConfigDir(t)

	l, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	amount := uint64(20000)
	identityRequired := true
	if _, err := l.Add(Entry{
		Token: "lokicash1abc", WalletPubkey: "wp", Secret: "s",
		RelayURLs: []string{"wss://relay.example"}, AmountMillis: &amount,
		IdentityRequired: &identityRequired,
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	l.AppendHistory("receive", "test")
	if err := l.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	reloaded, err := Load()
	if err != nil {
		t.Fatalf("Load() (reloaded) error = %v", err)
	}
	if len(reloaded.Entries) != 1 || reloaded.Entries[0].Token != "lokicash1abc" {
		t.Fatalf("reloaded entries = %+v, unexpected", reloaded.Entries)
	}
	if reloaded.Entries[0].AmountMillis == nil || *reloaded.Entries[0].AmountMillis != amount {
		t.Errorf("AmountMillis did not round-trip")
	}
	if len(reloaded.History) != 1 {
		t.Errorf("History did not round-trip")
	}
}

// TestSaveAndLoad_IdentityRequiredFalseRoundTrip guards the nullable-bool-
// via-nullable-int SQL mapping specifically for false: a naive "is this
// truthy" check on the stored int (rather than checking sql.NullInt64's
// own Valid flag) would be unable to tell a real, stored `false` apart
// from "never set at all" (nil) — both look like zero. Only the true case
// was previously exercised (TestSaveAndLoad_RoundTrip).
func TestSaveAndLoad_IdentityRequiredFalseRoundTrip(t *testing.T) {
	withTempConfigDir(t)

	l, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	no := false
	if _, err := l.Add(Entry{Token: "lokicash1bearer", IdentityRequired: &no}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if _, err := l.Add(Entry{Token: "lokicash1unspecified"}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := l.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	reloaded, err := Load()
	if err != nil {
		t.Fatalf("Load() (reloaded) error = %v", err)
	}
	bearer, _ := reloaded.FindByToken("lokicash1bearer")
	if bearer.IdentityRequired == nil {
		t.Fatal("IdentityRequired = nil after reload, want a non-nil pointer to false")
	}
	if *bearer.IdentityRequired != false {
		t.Errorf("IdentityRequired = %v, want false", *bearer.IdentityRequired)
	}
	unspecified, _ := reloaded.FindByToken("lokicash1unspecified")
	if unspecified.IdentityRequired != nil {
		t.Errorf("IdentityRequired = %v, want nil (never set)", *unspecified.IdentityRequired)
	}
}

// TestSaveAndLoad_ConnectionKeyFieldsRoundTrip covers the one group of
// Entry fields TestSaveAndLoad_RoundTrip never touches — a
// connection-key-bound entry's platform/external-id/attestation
// reference/IA pubkey all need to survive the SQL round trip too, since
// resolveCredential's own connection-key error message is built from them.
func TestSaveAndLoad_ConnectionKeyFieldsRoundTrip(t *testing.T) {
	withTempConfigDir(t)

	l, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if _, err := l.Add(Entry{
		Token:                   "lokicash1connkey",
		ConnectionKeyPlatform:   "discord",
		ConnectionKeyExternalID: "482910",
		AttestationEventID:      "deadbeef",
		IAPubkey:                "cafebabe",
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := l.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	reloaded, err := Load()
	if err != nil {
		t.Fatalf("Load() (reloaded) error = %v", err)
	}
	e, ok := reloaded.FindByToken("lokicash1connkey")
	if !ok {
		t.Fatal("entry not found after reload")
	}
	if e.ConnectionKeyPlatform != "discord" || e.ConnectionKeyExternalID != "482910" ||
		e.AttestationEventID != "deadbeef" || e.IAPubkey != "cafebabe" {
		t.Errorf("connection-key fields did not round-trip: %+v", e)
	}
}

func TestSaveAndLoad_MinterPubkeyRoundTrip(t *testing.T) {
	withTempConfigDir(t)

	l, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	minter := "aaaa111122223333444455556666777788889999aaaabbbbccccddddeeee00"
	if _, err := l.Add(Entry{Token: "lokicash1signed", MinterPubkey: &minter}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if _, err := l.Add(Entry{Token: "lokicash1unsigned"}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := l.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	reloaded, err := Load()
	if err != nil {
		t.Fatalf("Load() (reloaded) error = %v", err)
	}
	signed, ok := reloaded.FindByToken("lokicash1signed")
	if !ok {
		t.Fatal("signed entry not found after reload")
	}
	if signed.MinterPubkey == nil || *signed.MinterPubkey != minter {
		t.Errorf("MinterPubkey = %v, want %q", signed.MinterPubkey, minter)
	}
	unsigned, ok := reloaded.FindByToken("lokicash1unsigned")
	if !ok {
		t.Fatal("unsigned entry not found after reload")
	}
	if unsigned.MinterPubkey != nil {
		t.Errorf("MinterPubkey = %q, want nil", *unsigned.MinterPubkey)
	}
}

func TestSaveAndLoad_PreservesInsertionOrderForSameTimestamp(t *testing.T) {
	withTempConfigDir(t)

	l, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	// All three entries get the exact same ReceivedAt (RFC3339, 1-second
	// precision) — order must still come from insertion order (rowid), not
	// from received_at/id, which would otherwise scramble same-second
	// entries non-deterministically.
	same := nowRFC3339()
	for _, token := range []string{"lokicash1a", "lokicash1b", "lokicash1c"} {
		e := Entry{Token: token}
		added, err := l.Add(e)
		if err != nil {
			t.Fatalf("Add(%s) error = %v", token, err)
		}
		added.ReceivedAt = same
	}
	if err := l.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	reloaded, err := Load()
	if err != nil {
		t.Fatalf("Load() (reloaded) error = %v", err)
	}
	if len(reloaded.Entries) != 3 {
		t.Fatalf("reloaded %d entries, want 3", len(reloaded.Entries))
	}
	wantOrder := []string{"lokicash1a", "lokicash1b", "lokicash1c"}
	for i, want := range wantOrder {
		if reloaded.Entries[i].Token != want {
			t.Errorf("Entries[%d].Token = %q, want %q (insertion order not preserved: %v)", i, reloaded.Entries[i].Token, want, reloaded.Entries)
		}
	}
}

// TestSave_ConcurrentDisjointWritesDoNotClobber reproduces, deterministically
// and without spawning real OS processes, the exact shape two genuinely
// concurrent `cashctl` invocations hit against the same --config-dir: two
// *Ledger handles both Load a common starting point, then each mutates a
// DIFFERENT entry and appends its own history line, and Save in turn.
// Before this package's diff-based Save, the second Save (lb) blindly
// rewrote the whole entries/history tables from its own stale in-memory
// copy — reverting la's status change back to "held" (a spent token
// resurrected) and erasing la's history line entirely, even though lb never
// touched that entry or knew la had. See docs/private/
// audit-round2-race-adversarial.md for the live, real-subprocess version of
// this same bug (integration/cash_race_adversarial_test.go).
func TestSave_ConcurrentDisjointWritesDoNotClobber(t *testing.T) {
	withTempConfigDir(t)

	seed, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	entryA, _ := seed.Add(Entry{Token: "lokicash1a"})
	entryB, _ := seed.Add(Entry{Token: "lokicash1b"})
	_, _ = seed.Add(Entry{Token: "lokicash1c"})
	idA, idB := entryA.ID, entryB.ID
	if err := seed.Save(); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}

	// Both "processes" Load before either one Saves — exactly the window a
	// live redeem/transfer/consolidate's own wire round trip opens up.
	la, err := Load()
	if err != nil {
		t.Fatalf("Load() (process A) error = %v", err)
	}
	lb, err := Load()
	if err != nil {
		t.Fatalf("Load() (process B) error = %v", err)
	}

	// Process A: redeems entry A.
	if err := la.SetStatus(idA, StatusRedeemed); err != nil {
		t.Fatalf("SetStatus(A) error = %v", err)
	}
	la.AppendHistory("redeem", "redeemed A")

	// Process B: consolidates entry B into a brand-new entry D — disjoint
	// from anything process A touched.
	if err := lb.SetStatus(idB, StatusConsolidated); err != nil {
		t.Fatalf("SetStatus(B) error = %v", err)
	}
	newEntry, err := lb.Add(Entry{Token: "lokicash1d"})
	if err != nil {
		t.Fatalf("Add(D) error = %v", err)
	}
	idD := newEntry.ID
	lb.AppendHistory("consolidate", "consolidated B into D")

	// A commits first, B second — B's Save must not undo anything A wrote.
	if err := la.Save(); err != nil {
		t.Fatalf("Save() (process A) error = %v", err)
	}
	if err := lb.Save(); err != nil {
		t.Fatalf("Save() (process B) error = %v", err)
	}

	reloaded, err := Load()
	if err != nil {
		t.Fatalf("Load() (final) error = %v", err)
	}
	got := map[string]string{}
	for _, e := range reloaded.Entries {
		got[e.Token] = e.Status
	}
	want := map[string]string{
		"lokicash1a": StatusRedeemed,     // must NOT be reverted to "held" by B's Save
		"lokicash1b": StatusConsolidated, // B's own change
		"lokicash1c": StatusHeld,         // untouched by either
		"lokicash1d": StatusHeld,         // must NOT vanish because A's Save ran first without knowing about it
	}
	for token, wantStatus := range want {
		if got[token] != wantStatus {
			t.Errorf("entry %q status = %q, want %q (all statuses: %v)", token, got[token], wantStatus, got)
		}
	}
	if _, ok := reloaded.Find(idD); !ok {
		t.Errorf("entry D (id %q) is missing entirely after both Saves — a concurrent writer's own new entry was lost", idD)
	}

	actions := map[string]int{}
	for _, h := range reloaded.History {
		actions[h.Action]++
	}
	if actions["redeem"] != 1 {
		t.Errorf(`history has %d "redeem" entries, want 1 (process A's history line must survive process B's later Save)`, actions["redeem"])
	}
	if actions["consolidate"] != 1 {
		t.Errorf(`history has %d "consolidate" entries, want 1`, actions["consolidate"])
	}
}

func TestSave_FilePermissions(t *testing.T) {
	withTempConfigDir(t)

	l, _ := Load()
	_, _ = l.Add(Entry{Token: "lokicash1bearer", Secret: "a-bearer-secret-is-money"})
	if err := l.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	p, err := store.Path()
	if err != nil {
		t.Fatalf("store.Path() error = %v", err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("cashctl.db permissions = %o, want 0600", perm)
	}
}

// TestSave_NilVsEmptyRelayURLsRoundTrip is round 3's self-review of round
// 2's own diff-based Save: reflect.DeepEqual(nil, []string{}) is false in
// Go, so a bare DeepEqual comparison between what Load saw (RelayURLs is
// always nil when empty — see load's own decode) and an in-memory entry
// whose RelayURLs was reassigned to a non-nil empty slice would judge the
// row "changed" and rewrite it wholesale — see entriesEqual's own doc
// comment for why that's more than a wasted write (it can re-clobber an
// unrelated field a concurrent process changed in the meantime). Not
// reachable by any command today (RelayURLs is write-once at Add time and
// nipcash's decoder never produces a non-nil empty slice), but this proves
// the actual behavior either way: no data corruption, and — thanks to
// entriesEqual's normalization — the "unchanged" optimization still fires
// rather than needlessly rewriting the row.
func TestSave_NilVsEmptyRelayURLsRoundTrip(t *testing.T) {
	withTempConfigDir(t)

	l, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	added, err := l.Add(Entry{Token: "lokicash1nilrelays"})
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if added.RelayURLs != nil {
		t.Fatalf("precondition failed: freshly-added entry has non-nil RelayURLs")
	}
	if err := l.Save(); err != nil {
		t.Fatalf("first Save() error = %v", err)
	}

	// Simulate the class of caller entriesEqual guards against: flip the
	// in-memory entry's RelayURLs from nil to a non-nil empty slice,
	// without any real change to what should be persisted, then Save
	// again on the SAME *Ledger (same baseline from the Load above).
	entry, ok := l.Find(added.ID)
	if !ok {
		t.Fatalf("Find(%q) = not found", added.ID)
	}
	entry.RelayURLs = []string{}
	if err := l.Save(); err != nil {
		t.Fatalf("second Save() error = %v", err)
	}

	reloaded, err := Load()
	if err != nil {
		t.Fatalf("Load() (reloaded) error = %v", err)
	}
	got, ok := reloaded.FindByToken("lokicash1nilrelays")
	if !ok {
		t.Fatal("entry vanished after the nil<->[]string{} round trip")
	}
	if len(got.RelayURLs) != 0 {
		t.Errorf("RelayURLs = %v, want empty/nil — no corruption expected either way", got.RelayURLs)
	}
}

// TestEntriesEqual_TreatsNilAndEmptyRelayURLsAsEqual is entriesEqual's own
// direct unit test — confirms the "unchanged" optimization actually still
// fires for a nil<->[]string{} flip, the specific case a bare
// reflect.DeepEqual would get wrong.
func TestEntriesEqual_TreatsNilAndEmptyRelayURLsAsEqual(t *testing.T) {
	a := Entry{ID: "tok-1", RelayURLs: nil}
	b := Entry{ID: "tok-1", RelayURLs: []string{}}
	if !entriesEqual(a, b) {
		t.Error("entriesEqual(nil, []string{}) = false, want true")
	}
	c := Entry{ID: "tok-1", RelayURLs: []string{"wss://relay.example"}}
	if entriesEqual(a, c) {
		t.Error("entriesEqual(nil, [non-empty]) = true, want false")
	}
}

// TestSave_ConcurrentSameTokenReceiveDoesNotDuplicate is round 3's other
// open question: round 2's fix made Save upsert-by-ID instead of rewriting
// the whole table — does that make it POSSIBLE for two processes racing to
// receive the exact same token to both succeed, each with its own
// generated ID, silently double-counting the token's balance locally? Both
// "processes" here Load before either Adds/Saves — the same window a live
// `cashctl receive`'s own network round trip (checkClaimWithCashHub) opens
// up. Answer: no — entries.token's own UNIQUE constraint (internal/
// store.go's schema, unrelated to round 2's own fix) means the loser's
// upsert-by-ID INSERT still collides on the token column, which isn't the
// ON CONFLICT clause's target (id), so SQLite reports a hard constraint
// violation instead of silently succeeding — the loser's Save() fails
// outright, its whole transaction (including its own history line) rolled
// back, never partially applied. Run at a fan-out well past 2 to also
// exercise real SQLite write-lock contention (see
// TestSave_ManyDisjointConcurrentWritersAllSucceed for why that alone used
// to fail even for entries that never conflict).
func TestSave_ConcurrentSameTokenReceiveDoesNotDuplicate(t *testing.T) {
	withTempConfigDir(t)

	seed, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if err := seed.Save(); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	saveErrs := make([]error, n)
	start := make(chan struct{})
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			// Load happens inside the goroutine, before the barrier, so
			// every process's Load genuinely races the others' Adds —
			// none of them have saved yet at this point, exactly like two
			// real `cashctl receive` invocations both mid-network-call.
			l, err := Load()
			if err != nil {
				saveErrs[i] = err
				return
			}
			if _, err := l.Add(Entry{Token: "lokicash1sametoken", WalletPubkey: "wp", Secret: "s"}); err != nil {
				saveErrs[i] = err
				return
			}
			l.AppendHistory("receive", "received the same token concurrently")
			<-start
			saveErrs[i] = l.Save()
		}()
	}
	close(start)
	wg.Wait()

	succeeded := 0
	for _, err := range saveErrs {
		if err == nil {
			succeeded++
			continue
		}
		// The loser's Save must surface as ErrAlreadyHeld specifically —
		// the same classification cmd/cash_receive.go's Add-time check
		// already produces for the non-concurrent case — not a raw SQL
		// constraint error a caller has no way to recognize.
		if !errors.Is(err, ErrAlreadyHeld) {
			t.Errorf("a losing concurrent Save's error = %v, want errors.Is(err, ErrAlreadyHeld)", err)
		}
	}
	if succeeded != 1 {
		t.Errorf("succeeded Saves = %d, want exactly 1 (errors: %v)", succeeded, saveErrs)
	}

	reloaded, err := Load()
	if err != nil {
		t.Fatalf("Load() (final) error = %v", err)
	}
	rows := 0
	for _, e := range reloaded.Entries {
		if e.Token == "lokicash1sametoken" {
			rows++
		}
	}
	if rows != 1 {
		t.Errorf("BUG: %d rows for the same token after a concurrent-receive race, want exactly 1 — double-counted balance", rows)
	}
	historyCount := 0
	for _, h := range reloaded.History {
		if h.Detail == "received the same token concurrently" {
			historyCount++
		}
	}
	if historyCount != 1 {
		t.Errorf("history has %d matching lines, want exactly 1 (a losing Save's history append must be rolled back too, not partially applied)", historyCount)
	}
}

// TestSave_ManyDisjointConcurrentWritersAllSucceed widens
// TestSave_ConcurrentDisjointWritesDoNotClobber's own 2-writer scenario to
// a fan-out (12) chosen specifically to reproduce real SQLite write-lock
// contention, not just the logical lost-update round 2 already fixed.
// internal/store.Open sets no busy_timeout, so before withBusyRetry this
// failed reliably: with no cross-process lock coordination at all, enough
// simultaneous writers land their tx.Begin()/INSERT/Commit close enough
// together that SQLite hands most of them SQLITE_BUSY ("database is
// locked") immediately rather than queuing them — even though every
// writer here touches a totally disjoint entry and there is no logical
// conflict whatsoever. For a real redeem/transfer/consolidate whose wire
// call already succeeded, that failure mode means genuine, already-moved
// money with zero local record — the same blast radius as round 2's
// original bug, reached by lock contention instead of a stale in-memory
// snapshot.
func TestSave_ManyDisjointConcurrentWritersAllSucceed(t *testing.T) {
	withTempConfigDir(t)

	seed, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if err := seed.Save(); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}

	const n = 12
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			l, err := Load()
			if err != nil {
				errs[i] = err
				return
			}
			if _, err := l.Add(Entry{Token: "lokicash1fanout" + string(rune('a'+i))}); err != nil {
				errs[i] = err
				return
			}
			l.AppendHistory("receive", "disjoint fanout entry")
			<-start
			errs[i] = l.Save()
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("writer %d Save() error = %v, want nil — every entry here is disjoint, none should ever fail", i, err)
		}
	}

	reloaded, err := Load()
	if err != nil {
		t.Fatalf("Load() (final) error = %v", err)
	}
	if len(reloaded.Entries) != n {
		t.Errorf("final entry count = %d, want %d — a disjoint concurrent write was lost", len(reloaded.Entries), n)
	}
	if len(reloaded.History) != n {
		t.Errorf("final history count = %d, want %d", len(reloaded.History), n)
	}
}

// TestSaveAndLoad_PendingBearerSecretRoundTrip covers the write-ahead field
// a rekey/protect step persists before its own wire call (see
// cmd/receive_secure.go) — must survive a reload exactly like BearerSecret
// itself, and stay empty (not corrupted into some sentinel) when never set.
func TestSaveAndLoad_PendingBearerSecretRoundTrip(t *testing.T) {
	withTempConfigDir(t)

	l, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if _, err := l.Add(Entry{Token: "lokicash1pending", BearerSecret: "old-secret", PendingBearerSecret: "new-secret"}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if _, err := l.Add(Entry{Token: "lokicash1nopending", BearerSecret: "only-secret"}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := l.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	reloaded, err := Load()
	if err != nil {
		t.Fatalf("Load() (reloaded) error = %v", err)
	}
	pending, _ := reloaded.FindByToken("lokicash1pending")
	if pending.PendingBearerSecret != "new-secret" {
		t.Errorf("PendingBearerSecret = %q, want %q", pending.PendingBearerSecret, "new-secret")
	}
	if pending.BearerSecret != "old-secret" {
		t.Errorf("BearerSecret = %q, want %q (unchanged until reconciled)", pending.BearerSecret, "old-secret")
	}
	noPending, _ := reloaded.FindByToken("lokicash1nopending")
	if noPending.PendingBearerSecret != "" {
		t.Errorf("PendingBearerSecret = %q, want empty (never set)", noPending.PendingBearerSecret)
	}
}

// TestSaveAndLoad_ExpiresAtRoundTrip covers the Hub-side redemption
// deadline cached at receive time (see cmd/wallet_balance.go's
// summarizeHeldTokens) — must survive a reload exactly, and stay nil (not
// zero) when never learned, the same nullable-pointer shape
// IdentityRequired's own round-trip test already guards for a different
// field.
func TestSaveAndLoad_ExpiresAtRoundTrip(t *testing.T) {
	withTempConfigDir(t)

	l, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	expiresAt := int64(1_700_000_000)
	if _, err := l.Add(Entry{Token: "lokicash1expiring", ExpiresAt: &expiresAt}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if _, err := l.Add(Entry{Token: "lokicash1noexpiry"}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := l.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	reloaded, err := Load()
	if err != nil {
		t.Fatalf("Load() (reloaded) error = %v", err)
	}
	expiring, _ := reloaded.FindByToken("lokicash1expiring")
	if expiring.ExpiresAt == nil || *expiring.ExpiresAt != expiresAt {
		t.Errorf("ExpiresAt = %v, want %d", expiring.ExpiresAt, expiresAt)
	}
	noExpiry, _ := reloaded.FindByToken("lokicash1noexpiry")
	if noExpiry.ExpiresAt != nil {
		t.Errorf("ExpiresAt = %v, want nil (never set)", *noExpiry.ExpiresAt)
	}
}

// TestSaveAndLoad_BearerProtectionRoundTrip covers the shared-vs-protected
// marker (see cmd/receive_secure.go, wallet.go's own `wallet show`) — must
// survive a reload exactly, and stay "" (n/a) for a pubkey-mode entry that
// never set it at all, the same shape ExpiresAt's own round-trip test
// guards for a different field.
func TestSaveAndLoad_BearerProtectionRoundTrip(t *testing.T) {
	withTempConfigDir(t)

	l, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if _, err := l.Add(Entry{Token: "lokicash1shared", BearerProtection: BearerShared}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if _, err := l.Add(Entry{Token: "lokicash1protected", BearerProtection: BearerProtected}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if _, err := l.Add(Entry{Token: "lokicash1pubkeymode"}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := l.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	reloaded, err := Load()
	if err != nil {
		t.Fatalf("Load() (reloaded) error = %v", err)
	}
	shared, _ := reloaded.FindByToken("lokicash1shared")
	if shared.BearerProtection != BearerShared {
		t.Errorf("BearerProtection = %q, want %q", shared.BearerProtection, BearerShared)
	}
	protected, _ := reloaded.FindByToken("lokicash1protected")
	if protected.BearerProtection != BearerProtected {
		t.Errorf("BearerProtection = %q, want %q", protected.BearerProtection, BearerProtected)
	}
	pubkeyMode, _ := reloaded.FindByToken("lokicash1pubkeymode")
	if pubkeyMode.BearerProtection != "" {
		t.Errorf("BearerProtection = %q, want empty (never set)", pubkeyMode.BearerProtection)
	}
}
