package config

import (
	"encoding/json"
	"os"
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
	for _, key := range []string{"name", "value", "added_at", "last_known_balance_mloki", "last_known_balance_at"} {
		if _, ok := got[key]; !ok {
			t.Errorf("marshaled Connection is missing snake_case key %q: %s", key, data)
		}
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
