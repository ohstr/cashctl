package pocket

import (
	"context"
	"reflect"
	"testing"

	"github.com/ohstr/nmilat/nipCP"
)

func mkAmt(w string, rev uint64, state nipCP.State, id string, amount uint64, chain string) nipCP.Entry {
	e := mk(w, rev, state, id, 100)
	e.Amount = amount
	e.Chain = chain
	return e
}

func foldAll(t *testing.T, c *Cache, es []nipCP.Entry) {
	t.Helper()
	for _, e := range es {
		if _, err := c.Fold(e); err != nil {
			t.Fatalf("Fold: %v", err)
		}
	}
}

func TestBalanceSumsLivePerChain(t *testing.T) {
	c := openTestCache(t)
	foldAll(t, c, []nipCP.Entry{
		mkAmt("a1", 1, nipCP.StateLive, "r1", 100, "bitcoin"),
		mkAmt("a2", 1, nipCP.StateLive, "r2", 50, "bitcoin"),
		mkAmt("a3", 1, nipCP.StateSpent, "r3", 999, "bitcoin"), // not live -> excluded
		mkAmt("a4", 1, nipCP.StateLive, "r4", 7, "litecoin"),
	})
	bal, err := c.Balance()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]uint64{"bitcoin": 150, "litecoin": 7}
	if !reflect.DeepEqual(bal, want) {
		t.Fatalf("balance = %v, want %v", bal, want)
	}
}

func TestBalanceFollowsStateChange(t *testing.T) {
	c := openTestCache(t)
	foldAll(t, c, []nipCP.Entry{mkAmt("w1", 1, nipCP.StateLive, "r1", 100, "bitcoin")})
	if bal, _ := c.Balance(); bal["bitcoin"] != 100 {
		t.Fatalf("live balance = %d, want 100", bal["bitcoin"])
	}
	// A higher-rev spent supersedes the live entry; it drops out of the balance.
	foldAll(t, c, []nipCP.Entry{mkAmt("w1", 2, nipCP.StateSpent, "r2", 100, "bitcoin")})
	if bal, _ := c.Balance(); bal["bitcoin"] != 0 {
		t.Fatalf("after spend balance = %d, want 0", bal["bitcoin"])
	}
}

func TestRebuildEqualsIncremental(t *testing.T) {
	all := coreSet()
	c := openTestCache(t)
	foldAll(t, c, all)
	incremental, incConf := winnersFromCache(t, c)

	if err := c.Rebuild(all); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	rebuilt, rebConf := winnersFromCache(t, c)

	if !reflect.DeepEqual(incremental, rebuilt) || !reflect.DeepEqual(incConf, rebConf) {
		t.Fatalf("rebuild != incremental:\n inc=%v conf=%v\n reb=%v conf=%v", incremental, incConf, rebuilt, rebConf)
	}
}

func TestCursorMonotonic(t *testing.T) {
	c := openTestCache(t)
	if v, _ := c.Cursor(); v != 0 {
		t.Fatalf("fresh cursor = %d, want 0", v)
	}
	if err := c.SetCursor(100); err != nil {
		t.Fatal(err)
	}
	if v, _ := c.Cursor(); v != 100 {
		t.Fatalf("cursor = %d, want 100", v)
	}
	if err := c.SetCursor(50); err != nil { // backwards: ignored
		t.Fatal(err)
	}
	if v, _ := c.Cursor(); v != 100 {
		t.Fatalf("cursor after backwards set = %d, want 100", v)
	}
	if err := c.SetCursor(200); err != nil {
		t.Fatal(err)
	}
	if v, _ := c.Cursor(); v != 200 {
		t.Fatalf("cursor = %d, want 200", v)
	}
}

// TestIncrementalFilterCoversBackdateWindow guards risk 13: a pull from the
// persisted cursor must reach back the NIP-59 2-day window, so a wrap published
// now but back-dated up to 2 days is still caught. The window lives in nmilat's
// IncrementalFilter (SyncOverlap); this pins that our cursor feeds it.
func TestIncrementalFilterCoversBackdateWindow(t *testing.T) {
	const cursor = 1_000_000_000
	f := nipCP.IncrementalFilter("abc", cursor)
	if got, want := int64(f.Since), int64(cursor)-nipCP.SyncOverlap; got != want {
		t.Fatalf("Since = %d, want cursor - 2d = %d", got, want)
	}
	if nipCP.SyncOverlap != 2*24*60*60 {
		t.Fatalf("SyncOverlap = %d, want 2 days", nipCP.SyncOverlap)
	}
}

func TestDeviceStore(t *testing.T) {
	c := openTestCache(t)
	ctx := context.Background()
	id1, err := c.DeviceID(ctx)
	if err != nil || len(id1) != 32 { // 16 bytes hex
		t.Fatalf("DeviceID = %q err=%v", id1, err)
	}
	id2, _ := c.DeviceID(ctx)
	if id1 != id2 {
		t.Fatalf("DeviceID not stable: %q vs %q", id1, id2)
	}
	if ok, _ := c.DerivedPlaced(ctx, "deadbeef"); ok {
		t.Fatalf("unknown derived id reported placed")
	}
	if err := c.RecordDerived(ctx, "deadbeef", 123); err != nil {
		t.Fatal(err)
	}
	if ok, _ := c.DerivedPlaced(ctx, "deadbeef"); !ok {
		t.Fatalf("recorded derived id not reported placed")
	}
}
