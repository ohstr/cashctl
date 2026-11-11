package pocket

import (
	"context"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	btcec "github.com/flokiorg/go-flokicoin/crypto"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nipCP"
	"github.com/ohstr/nmilat/utils"
)

func testKey(t *testing.T) (priv, pub string) {
	t.Helper()
	k, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	priv = hex.EncodeToString(k.Serialize())
	pub, err = utils.GetPublicKey(priv)
	if err != nil {
		t.Fatalf("pub: %v", err)
	}
	return priv, pub
}

func validBill(w string) nipCP.Entry {
	return nipCP.Entry{
		W: w, Rev: 1, State: nipCP.StateLive, CheckedAt: 1_700_000_000,
		Amount: 100, Chain: "bitcoin", Cash: "satscash1test",
		Identity: nipCP.EntryIdentity{Type: nipCP.IdentityPubkey, Value: strings.Repeat("cc", 32)},
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "pocket.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestStoreRoundTrip seals two real pocket entries, publishes the gift wraps to
// the in-process event store, queries them back by #p (FullSyncFilter), and
// reopens them — the full canonical-store path the sync will use.
func TestStoreRoundTrip(t *testing.T) {
	priv, pub := testKey(t)
	s := openTestStore(t)
	ctx := context.Background()

	wraps := map[string]bool{}
	for _, w := range []string{strings.Repeat("aa", 32), strings.Repeat("bb", 32)} {
		wrap, _, err := nipCP.Seal(validBill(w), priv)
		if err != nil {
			t.Fatalf("Seal: %v", err)
		}
		if err := s.Publish(ctx, "", wrap); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		wraps[w] = true
	}

	got, err := s.Query(ctx, "", nipCP.FullSyncFilter(pub))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("query returned %d events, want 2", len(got))
	}
	entries, err := nipCP.OpenAll(got, priv)
	if err != nil {
		t.Fatalf("OpenAll: %v", err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.W] = true
	}
	if !seen[strings.Repeat("aa", 32)] || !seen[strings.Repeat("bb", 32)] {
		t.Fatalf("reopened wallets = %v, want both aa.. and bb..", seen)
	}
}

// TestStoreDedup: re-publishing the same wrap (idempotent sync) never yields a
// duplicate on query.
func TestStoreDedup(t *testing.T) {
	priv, pub := testKey(t)
	s := openTestStore(t)
	ctx := context.Background()
	wrap, _, err := nipCP.Seal(validBill(strings.Repeat("aa", 32)), priv)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Publish(ctx, "", wrap); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}
	got, err := s.Query(ctx, "", nipCP.FullSyncFilter(pub))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("query after triple publish returned %d, want 1", len(got))
	}
}

// TestStoreSinceFilter: a since in the future excludes existing wraps.
func TestStoreSinceFilter(t *testing.T) {
	priv, pub := testKey(t)
	s := openTestStore(t)
	ctx := context.Background()
	wrap, _, err := nipCP.Seal(validBill(strings.Repeat("aa", 32)), priv)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Publish(ctx, "", wrap); err != nil {
		t.Fatal(err)
	}
	f := nip01.SubscriptionFilter{
		Kinds: []int{wrap.Kind},
		Tags:  map[string][]string{"p": {pub}},
		Since: wrap.CreatedAt + 100_000,
	}
	got, err := s.Query(ctx, "", f)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("future-since query returned %d, want 0", len(got))
	}
}
