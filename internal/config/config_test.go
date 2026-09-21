package config

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/store"
)

// TestConnection_JSONFieldNames guards the --json contract `connect list`/
// `wallet show` depend on (cmd/connect.go, cmd/wallet.go embed
// []Connection directly into their --json output): Connection is no
// longer the on-disk shape now that storage is SQLite, but its JSON tags
// still matter, and are easy to lose by accident in a storage-layer
// refactor that doesn't touch cmd/ at all.
func TestConnection_JSONFieldNames(t *testing.T) {
	balance := int64(5000)
	c := Connection{Name: "work", Value: "v", AddedAt: "t", LastKnownBalanceMloki: &balance, LastKnownBalanceAt: "u"}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	for _, key := range []string{"name", "added_at", "last_known_balance_mloki", "last_known_balance_at"} {
		if _, ok := got[key]; !ok {
			t.Errorf("marshaled Connection is missing snake_case key %q: %s", key, data)
		}
	}
}

// TestConnection_JSONOmitsValue is the regression test for a real secret
// leak: `connect list`/`wallet show` embed a whole []Connection directly
// into --json output, so a plain json tag on Value put every registered
// wallet's own pairing secret (an NWC URI's secret=, or the equivalent
// embedded in a bech32 hub string) in the documented way to list wallets.
// Text mode never showed it either — --json must not, on the same value.
func TestConnection_JSONOmitsValue(t *testing.T) {
	c := Connection{Name: "work", Value: "nostr+walletconnect://pub?relay=wss://x&secret=deadbeef", AddedAt: "t"}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if strings.Contains(string(data), "deadbeef") || strings.Contains(string(data), "\"value\"") {
		t.Errorf("marshaled Connection leaks Value: %s", data)
	}
}

func withTempConfigDir(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	appdir.SetOverride(tmp)
	t.Cleanup(func() { appdir.SetOverride("") })
}

func TestLoad_EmptyOnFreshInstall(t *testing.T) {
	withTempConfigDir(t)

	s, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !s.IsEmpty() {
		t.Error("Load() on a fresh install is not empty")
	}
	if _, ok := s.DefaultConnection(); ok {
		t.Error("DefaultConnection() on a fresh install returned ok=true")
	}
}

func TestAddAndSaveRoundTrip(t *testing.T) {
	withTempConfigDir(t)

	s, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if err := s.Add("lightning:default", "nostr+walletconnect://abc?relay=wss://relay.example&secret=xyz"); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	reloaded, err := Load()
	if err != nil {
		t.Fatalf("Load() (reloaded) error = %v", err)
	}
	c, ok := reloaded.Find("lightning:default")
	if !ok {
		t.Fatal("Find() after reload = not found")
	}
	if c.Value != "nostr+walletconnect://abc?relay=wss://relay.example&secret=xyz" {
		t.Errorf("Value = %q, unexpected", c.Value)
	}
}

// TestSaveAndLoad_LastKnownBalanceRoundTrip covers the one Connection
// field group no other Save/Load test touches — SetLastKnownBalance's
// nullable int64/string pair (`wallet balance`'s own stranded-wallet
// fallback depends on these actually surviving a save/load cycle, not
// just an in-memory Find right after SetLastKnownBalance is called).
func TestSaveAndLoad_LastKnownBalanceRoundTrip(t *testing.T) {
	withTempConfigDir(t)

	s, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if err := s.Add("work", "nostr+walletconnect://work"); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := s.Add("untouched", "nostr+walletconnect://untouched"); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	s.SetLastKnownBalance("work", 42_000)
	if err := s.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	reloaded, err := Load()
	if err != nil {
		t.Fatalf("Load() (reloaded) error = %v", err)
	}
	work, ok := reloaded.Find("work")
	if !ok {
		t.Fatal("Find(work) after reload = not found")
	}
	if work.LastKnownBalanceMloki == nil || *work.LastKnownBalanceMloki != 42_000 {
		t.Errorf("LastKnownBalanceMloki = %v, want 42000", work.LastKnownBalanceMloki)
	}
	if work.LastKnownBalanceAt == "" {
		t.Error("LastKnownBalanceAt is empty after reload, want the timestamp SetLastKnownBalance set")
	}

	untouched, ok := reloaded.Find("untouched")
	if !ok {
		t.Fatal("Find(untouched) after reload = not found")
	}
	if untouched.LastKnownBalanceMloki != nil {
		t.Errorf("LastKnownBalanceMloki = %v, want nil (never set) for a connection SetLastKnownBalance was never called on", *untouched.LastKnownBalanceMloki)
	}
	if untouched.LastKnownBalanceAt != "" {
		t.Errorf("LastKnownBalanceAt = %q, want empty for a connection SetLastKnownBalance was never called on", untouched.LastKnownBalanceAt)
	}
}

func TestSaveAndLoad_DefaultRoundTrip(t *testing.T) {
	withTempConfigDir(t)

	s, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if err := s.Add("work", "nostr+walletconnect://work"); err != nil {
		t.Fatalf("Add(work) error = %v", err)
	}
	if err := s.Add("personal", "nostr+walletconnect://personal"); err != nil {
		t.Fatalf("Add(personal) error = %v", err)
	}
	if err := s.SetDefault("personal"); err != nil {
		t.Fatalf("SetDefault() error = %v", err)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	reloaded, err := Load()
	if err != nil {
		t.Fatalf("Load() (reloaded) error = %v", err)
	}
	if reloaded.Default != "personal" {
		t.Errorf("Default = %q, want %q", reloaded.Default, "personal")
	}
	c, ok := reloaded.DefaultConnection()
	if !ok || c.Name != "personal" {
		t.Errorf("DefaultConnection() = %+v, %v, want the personal connection", c, ok)
	}
}

func TestAdd_DuplicateNameRejected(t *testing.T) {
	s := &Store{}
	if err := s.Add("a", "v1"); err != nil {
		t.Fatalf("first Add() error = %v", err)
	}
	if err := s.Add("a", "v2"); err == nil {
		t.Fatal("expected ErrDuplicateName, got nil")
	} else if err != ErrDuplicateName {
		t.Errorf("error = %v, want ErrDuplicateName", err)
	}
}

func TestRemove_ClearsDefaultIfItWasTheOneRemoved(t *testing.T) {
	s := &Store{}
	_ = s.Add("a", "v1")
	_ = s.SetDefault("a")

	if !s.Remove("a") {
		t.Fatal("Remove() = false, want true")
	}
	if _, ok := s.DefaultConnection(); ok {
		t.Error("DefaultConnection() still resolves after removing the default connection")
	}
	if s.Default != "" {
		t.Errorf("Default = %q, want cleared", s.Default)
	}
}

func TestRemove_UnrelatedDefaultUntouched(t *testing.T) {
	s := &Store{}
	_ = s.Add("a", "v1")
	_ = s.Add("b", "v2")
	_ = s.SetDefault("a")

	s.Remove("b")
	if s.Default != "a" {
		t.Errorf("Default = %q, want unchanged %q", s.Default, "a")
	}
}

func TestSetDefault_UnknownNameErrors(t *testing.T) {
	s := &Store{}
	if err := s.SetDefault("does-not-exist"); err == nil {
		t.Fatal("expected an error setting default to an unknown connection")
	}
}

func TestIsEmpty(t *testing.T) {
	s := &Store{}
	if !s.IsEmpty() {
		t.Error("IsEmpty() = false on a brand new Store")
	}
	_ = s.Add("a", "v")
	if s.IsEmpty() {
		t.Error("IsEmpty() = true after Add")
	}
}

func TestSuggestName_UsesHintWhenAvailable(t *testing.T) {
	s := &Store{}
	got := s.SuggestName("circle", "Ada's Family Circle")
	want := "circle:ada-s-family-circle"
	if got != want {
		t.Errorf("SuggestName() = %q, want %q", got, want)
	}
}

func TestSuggestName_FallsBackToNumberOnEmptyHint(t *testing.T) {
	s := &Store{}
	got := s.SuggestName("circle", "")
	if got != "circle:1" {
		t.Errorf("SuggestName() = %q, want %q", got, "circle:1")
	}
}

func TestSuggestName_AvoidsCollision(t *testing.T) {
	s := &Store{}
	_ = s.Add("circle:family", "v1")
	got := s.SuggestName("circle", "Family")
	if got == "circle:family" {
		t.Errorf("SuggestName() collided with an existing name: %q", got)
	}
	if got != "circle:1" {
		t.Errorf("SuggestName() = %q, want fallback %q", got, "circle:1")
	}
}

func TestSuggestName_NumericFallbackAvoidsCollision(t *testing.T) {
	s := &Store{}
	_ = s.Add("circle:1", "v1")
	_ = s.Add("circle:2", "v2")
	got := s.SuggestName("circle", "")
	if got != "circle:3" {
		t.Errorf("SuggestName() = %q, want %q", got, "circle:3")
	}
}

func TestSave_FilePermissions(t *testing.T) {
	withTempConfigDir(t)

	s, _ := Load()
	_ = s.Add("a", "secret-bearing-value")
	if err := s.Save(); err != nil {
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

// --- Save is a diff against what Load read, not a wholesale rewrite: two
// processes' Load-mutate-Save cycles must not erase each other's work.

func mustLoad(t *testing.T) *Store {
	t.Helper()
	s, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	return s
}

func mustSave(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
}

func names(s *Store) []string {
	var out []string
	for _, c := range s.Connections {
		out = append(out, c.Name)
	}
	return out
}

func TestSave_ConcurrentDisjointAddsDoNotClobber(t *testing.T) {
	// Both loaded the same (empty) state before either saved — the old
	// DELETE-everything-and-reinsert Save let whichever went second erase
	// the other's connection outright.
	withTempConfigDir(t)
	a, b := mustLoad(t), mustLoad(t)
	if err := a.Add("alpha", "nostr+walletconnect://alpha"); err != nil {
		t.Fatal(err)
	}
	if err := b.Add("bravo", "nostr+walletconnect://bravo"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, a)
	mustSave(t, b)

	got := names(mustLoad(t))
	if len(got) != 2 || got[0] != "alpha" || got[1] != "bravo" {
		t.Errorf("connections after two concurrent adds = %v, want [alpha bravo]", got)
	}
}

func TestSave_ConcurrentSameNameAddIsADuplicateNotAnOverwrite(t *testing.T) {
	withTempConfigDir(t)
	a, b := mustLoad(t), mustLoad(t)
	if err := a.Add("work", "nostr+walletconnect://first"); err != nil {
		t.Fatal(err)
	}
	if err := b.Add("work", "nostr+walletconnect://second"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, a)
	if err := b.Save(); !errors.Is(err, ErrDuplicateName) {
		t.Fatalf("second Save() of the same name = %v, want ErrDuplicateName", err)
	}
	c, _ := mustLoad(t).Find("work")
	if c == nil || c.Value != "nostr+walletconnect://first" {
		t.Errorf("the first writer's value was overwritten: %+v", c)
	}
}

func TestSave_UnchangedStoreDoesNotResurrectAConnectionAnotherProcessRemoved(t *testing.T) {
	withTempConfigDir(t)
	seed := mustLoad(t)
	_ = seed.Add("gone", "nostr+walletconnect://gone")
	mustSave(t, seed)

	a, b := mustLoad(t), mustLoad(t)
	a.Remove("gone")
	mustSave(t, a)
	mustSave(t, b) // b never touched anything: must be a no-op, not a re-insert

	if got := names(mustLoad(t)); len(got) != 0 {
		t.Errorf("connections = %v, want none — the stale, unchanged Save resurrected a removed connection", got)
	}
}

func TestSave_RemoveOnlyDeletesWhatItLoaded(t *testing.T) {
	withTempConfigDir(t)
	seed := mustLoad(t)
	_ = seed.Add("old", "nostr+walletconnect://old")
	mustSave(t, seed)

	a, b := mustLoad(t), mustLoad(t)
	_ = b.Add("fresh", "nostr+walletconnect://fresh")
	mustSave(t, b)
	a.Remove("old")
	mustSave(t, a)

	got := names(mustLoad(t))
	if len(got) != 1 || got[0] != "fresh" {
		t.Errorf("connections = %v, want [fresh] — removing 'old' must not take a concurrent add with it", got)
	}
}

func TestSave_DefaultOnlyTouchedWhenThisProcessChangedIt(t *testing.T) {
	withTempConfigDir(t)
	seed := mustLoad(t)
	_ = seed.Add("x", "nostr+walletconnect://x")
	_ = seed.Add("y", "nostr+walletconnect://y")
	_ = seed.SetDefault("x")
	mustSave(t, seed)

	a, b := mustLoad(t), mustLoad(t)
	_ = b.SetDefault("y")
	mustSave(t, b)
	_ = a.Add("z", "nostr+walletconnect://z") // a never touched the default
	mustSave(t, a)

	if got := mustLoad(t).Default; got != "y" {
		t.Errorf("Default = %q, want y — a stale process reasserted the default it had loaded", got)
	}
}

func TestSave_ChangedBalanceIsPersistedAndSurvivesRepeatedSaves(t *testing.T) {
	withTempConfigDir(t)
	s := mustLoad(t)
	_ = s.Add("w", "nostr+walletconnect://w")
	mustSave(t, s)

	s.SetLastKnownBalance("w", 42_000)
	mustSave(t, s)
	mustSave(t, s) // a second Save of the same Store must diff against the first

	c, _ := mustLoad(t).Find("w")
	if c == nil || c.LastKnownBalanceMloki == nil || *c.LastKnownBalanceMloki != 42_000 {
		t.Errorf("balance not persisted: %+v", c)
	}
}
