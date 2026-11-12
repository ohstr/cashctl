package pocket

import (
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/ohstr/nmilat/nipCP"
)

// mk builds a decoded entry for the fold tests. The fold reads only w, rev,
// state, rumor id and rumor created_at; amount/chain are set so the row's NOT
// NULL columns are satisfied.
func mk(w string, rev uint64, state nipCP.State, id string, createdAt int64) nipCP.Entry {
	return nipCP.Entry{
		W: w, Rev: rev, State: state, RumorID: id, RumorCreatedAt: createdAt,
		Amount: 100, Chain: "bitcoin", CheckedAt: createdAt,
	}
}

// coreSet exercises every branch of resolution: a higher rev superseding a
// lower one; at equal rev a final state beating a non-final one; and at equal
// rev/final the lowest rumor id winning. aa and bb each hold two distinct
// rumors at their winning rev (conflicts); cc is uncontested.
func coreSet() []nipCP.Entry {
	return []nipCP.Entry{
		mk("aa", 1, nipCP.StateLive, "r1", 100),  // superseded by rev 2
		mk("aa", 2, nipCP.StateLive, "r2", 200),  // rev 2, non-final
		mk("aa", 2, nipCP.StateSpent, "r3", 150), // rev 2, final -> beats r2
		mk("bb", 1, nipCP.StateLive, "r4", 100),  // ties r5; lowest id wins
		mk("bb", 1, nipCP.StateLive, "r5", 100),
		mk("cc", 1, nipCP.StateLive, "r6", 100),
	}
}

type winner struct {
	rev   uint64
	state nipCP.State
	id    string
}

func winnersFromResolve(all []nipCP.Entry) (map[string]winner, []string) {
	w, conflicts := nipCP.Resolve(all)
	out := map[string]winner{}
	for k, e := range w {
		out[k] = winner{e.Rev, e.State, e.RumorID}
	}
	return out, conflicts
}

func winnersFromCache(t *testing.T, c *Cache) (map[string]winner, []string) {
	t.Helper()
	entries, conflicts, err := c.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	out := map[string]winner{}
	for _, e := range entries {
		out[e.W] = winner{e.Rev, e.State, e.RumorID}
	}
	sort.Strings(conflicts)
	return out, conflicts
}

func openTestCache(t *testing.T) *Cache {
	t.Helper()
	c, err := OpenCache(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatalf("OpenCache: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestFoldMatchesResolveEveryOrder is the headline property: because resolution
// is a total order, folding events one at a time in ANY order must converge on
// the same winner and conflicts that nipCP.Resolve computes over the whole set.
func TestFoldMatchesResolveEveryOrder(t *testing.T) {
	all := coreSet()
	wantWinners, wantConflicts := winnersFromResolve(all)
	sort.Strings(wantConflicts)

	c := openTestCache(t)
	n := 0
	permute(all, func(order []nipCP.Entry) {
		if err := c.Rebuild(nil); err != nil { // clear between permutations
			t.Fatalf("clear: %v", err)
		}
		for _, e := range order {
			if _, err := c.Fold(e); err != nil {
				t.Fatalf("Fold: %v", err)
			}
		}
		gotWinners, gotConflicts := winnersFromCache(t, c)
		if !reflect.DeepEqual(gotWinners, wantWinners) {
			t.Fatalf("order %d: winners = %v, want %v (order=%v)", n, gotWinners, wantWinners, ids(order))
		}
		if !reflect.DeepEqual(gotConflicts, wantConflicts) {
			t.Fatalf("order %d: conflicts = %v, want %v", n, gotConflicts, wantConflicts)
		}
		n++
	})
	if n != 720 { // 6! — confirms the permutation generator covered every order
		t.Fatalf("expected 720 permutations, ran %d", n)
	}
}

// TestFoldIdempotentAndUnrecognised: re-applying events is a no-op, and an
// entry whose state this build does not recognise is ignored (as Resolve does),
// without disturbing the winners.
func TestFoldIdempotentAndUnrecognised(t *testing.T) {
	c := openTestCache(t)
	all := coreSet()
	for _, e := range all {
		if _, err := c.Fold(e); err != nil {
			t.Fatal(err)
		}
	}
	before, beforeConf := winnersFromCache(t, c)

	// Re-fold everything, plus a duplicate rumor and an unrecognised-state entry.
	replay := append(append([]nipCP.Entry(nil), all...),
		mk("cc", 1, nipCP.StateLive, "r6", 100),       // exact duplicate
		mk("dd", 1, nipCP.State("frozen"), "r7", 100)) // unknown state -> ignored
	for _, e := range replay {
		if _, err := c.Fold(e); err != nil {
			t.Fatal(err)
		}
	}
	after, afterConf := winnersFromCache(t, c)

	if !reflect.DeepEqual(before, after) {
		t.Fatalf("replay changed winners: %v -> %v", before, after)
	}
	if !reflect.DeepEqual(beforeConf, afterConf) {
		t.Fatalf("replay changed conflicts: %v -> %v", beforeConf, afterConf)
	}
	if _, ok := after["dd"]; ok {
		t.Fatalf("unrecognised-state entry was not ignored")
	}
}

func ids(es []nipCP.Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.RumorID
	}
	return out
}

// permute calls f with every permutation of a (Heap's algorithm). f receives a
// fresh copy, so it may retain it.
func permute(a []nipCP.Entry, f func([]nipCP.Entry)) {
	a = append([]nipCP.Entry(nil), a...)
	n := len(a)
	c := make([]int, n)
	f(append([]nipCP.Entry(nil), a...))
	for i := 0; i < n; {
		if c[i] < i {
			if i%2 == 0 {
				a[0], a[i] = a[i], a[0]
			} else {
				a[c[i]], a[i] = a[i], a[c[i]]
			}
			f(append([]nipCP.Entry(nil), a...))
			c[i]++
			i = 0
		} else {
			c[i] = 0
			i++
		}
	}
}
