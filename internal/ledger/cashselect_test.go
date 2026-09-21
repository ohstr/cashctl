package ledger

import (
	"errors"
	"testing"
)

func amountPtr(v uint64) *uint64 { return &v }
func minterPtr(s string) *string { return &s }

const (
	minterA = "aaaa111122223333444455556666777788889999aaaabbbbccccddddeeee00"
	minterB = "bbbb111122223333444455556666777788889999aaaabbbbccccddddeeee00"
)

func TestSelectForAmount_ExactMatch(t *testing.T) {
	held := []Entry{
		{ID: "tok-1", AmountMillis: amountPtr(3000)},
		{ID: "tok-2", AmountMillis: amountPtr(5000)},
	}
	plan, err := SelectForAmount(held, 5000)
	if err != nil {
		t.Fatalf("SelectForAmount() error = %v", err)
	}
	if plan.Entry == nil || plan.Entry.ID != "tok-2" {
		t.Fatalf("plan.Entry = %+v, want tok-2", plan.Entry)
	}
	if plan.Split {
		t.Error("plan.Split = true, want false for an exact match")
	}
	if plan.ConsolidateFirst != nil {
		t.Error("plan.ConsolidateFirst is set, want nil for an exact match")
	}
}

func TestSelectForAmount_BestFitSplit(t *testing.T) {
	// Three tokens cover 4000: 5000, 9000, and 20000. Best-fit should pick
	// the smallest covering one (5000), not the largest (20000).
	held := []Entry{
		{ID: "tok-big", AmountMillis: amountPtr(20000)},
		{ID: "tok-small", AmountMillis: amountPtr(5000)},
		{ID: "tok-mid", AmountMillis: amountPtr(9000)},
	}
	plan, err := SelectForAmount(held, 4000)
	if err != nil {
		t.Fatalf("SelectForAmount() error = %v", err)
	}
	if plan.Entry == nil || plan.Entry.ID != "tok-small" {
		t.Fatalf("plan.Entry = %+v, want tok-small (best-fit, not largest-available)", plan.Entry)
	}
	if !plan.Split {
		t.Error("plan.Split = false, want true — the covering token exceeds the target")
	}
}

func TestSelectForAmount_ConsolidatesSameMinterSubset(t *testing.T) {
	held := []Entry{
		{ID: "tok-1", AmountMillis: amountPtr(2000), MinterPubkey: minterPtr(minterA)},
		{ID: "tok-2", AmountMillis: amountPtr(3000), MinterPubkey: minterPtr(minterA)},
		{ID: "tok-3", AmountMillis: amountPtr(1000), MinterPubkey: minterPtr(minterA)},
	}
	// No single token covers 4000, but 2+3 = 5000 does. Greedy-largest-first
	// should pick {tok-2 (3000), tok-1 (2000)} = 5000, skipping tok-3.
	plan, err := SelectForAmount(held, 4000)
	if err != nil {
		t.Fatalf("SelectForAmount() error = %v", err)
	}
	if plan.Entry != nil {
		t.Fatalf("plan.Entry = %+v, want nil — this should be a consolidate-first plan", plan.Entry)
	}
	if len(plan.ConsolidateFirst) != 2 {
		t.Fatalf("ConsolidateFirst = %+v, want 2 entries", plan.ConsolidateFirst)
	}
	got := map[string]bool{}
	for _, e := range plan.ConsolidateFirst {
		got[e.ID] = true
	}
	if !got["tok-1"] || !got["tok-2"] {
		t.Errorf("ConsolidateFirst = %+v, want {tok-1, tok-2}", plan.ConsolidateFirst)
	}
	if !plan.Split {
		t.Error("plan.Split = false, want true — consolidated sum (5000) exceeds target (4000)")
	}
}

func TestSelectForAmount_ConsolidatedSumExactMatch(t *testing.T) {
	held := []Entry{
		{ID: "tok-1", AmountMillis: amountPtr(2000), MinterPubkey: minterPtr(minterA)},
		{ID: "tok-2", AmountMillis: amountPtr(3000), MinterPubkey: minterPtr(minterA)},
	}
	plan, err := SelectForAmount(held, 5000)
	if err != nil {
		t.Fatalf("SelectForAmount() error = %v", err)
	}
	if len(plan.ConsolidateFirst) != 2 {
		t.Fatalf("ConsolidateFirst = %+v, want 2 entries", plan.ConsolidateFirst)
	}
	if plan.Split {
		t.Error("plan.Split = true, want false — consolidated sum equals target exactly")
	}
}

func TestSelectForAmount_ExcludesBearerAndConnectionKeyFromGrouping(t *testing.T) {
	nonBearer := false
	held := []Entry{
		{ID: "tok-bearer", AmountMillis: amountPtr(3000), MinterPubkey: minterPtr(minterA), IdentityRequired: &nonBearer},
		{ID: "tok-connkey", AmountMillis: amountPtr(3000), MinterPubkey: minterPtr(minterA), ConnectionKeyPlatform: "discord"},
		{ID: "tok-pubkey", AmountMillis: amountPtr(3000), MinterPubkey: minterPtr(minterA)},
	}
	// Only tok-pubkey is eligible for grouping; alone it can't reach 5000.
	_, err := SelectForAmount(held, 5000)
	if !errors.Is(err, ErrFundsFragmented) {
		t.Fatalf("SelectForAmount() error = %v, want ErrFundsFragmented (bearer/connection-key entries must be excluded from grouping)", err)
	}
}

func TestSelectForAmount_ExcludesNoMinterFromGrouping(t *testing.T) {
	held := []Entry{
		{ID: "tok-1", AmountMillis: amountPtr(3000)}, // no MinterPubkey
		{ID: "tok-2", AmountMillis: amountPtr(3000)}, // no MinterPubkey
	}
	_, err := SelectForAmount(held, 5000)
	if !errors.Is(err, ErrFundsFragmented) {
		t.Fatalf("SelectForAmount() error = %v, want ErrFundsFragmented (entries with no MinterPubkey must be excluded from grouping)", err)
	}
}

func TestSelectForAmount_DifferentMintersNotGroupedTogether(t *testing.T) {
	held := []Entry{
		{ID: "tok-1", AmountMillis: amountPtr(3000), MinterPubkey: minterPtr(minterA)},
		{ID: "tok-2", AmountMillis: amountPtr(3000), MinterPubkey: minterPtr(minterB)},
	}
	// 3000+3000=6000 would cover 5000, but they're from different minters
	// and must never be grouped together.
	_, err := SelectForAmount(held, 5000)
	if !errors.Is(err, ErrFundsFragmented) {
		t.Fatalf("SelectForAmount() error = %v, want ErrFundsFragmented (different minters must never be grouped)", err)
	}
}

// TestSelectForAmount_InsufficientFunds guards the fix distinguishing a
// plain overdraft from real fragmentation: held's total (1000) doesn't
// even reach target (5000) — there's only one token, one minter, nothing
// "fragmented across separate Hubs" about it. This used to return the
// exact same ErrFundsFragmented a genuinely-spread-across-minters case
// does; it must now say plainly that there isn't enough.
func TestSelectForAmount_InsufficientFunds(t *testing.T) {
	held := []Entry{
		{ID: "tok-1", AmountMillis: amountPtr(1000), MinterPubkey: minterPtr(minterA)},
	}
	_, err := SelectForAmount(held, 5000)
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("SelectForAmount() error = %v, want ErrInsufficientFunds", err)
	}
	if errors.Is(err, ErrFundsFragmented) {
		t.Errorf("SelectForAmount() error = %v, must NOT also be ErrFundsFragmented — this is a plain overdraft, not fragmentation", err)
	}
	var insuf *InsufficientFundsError
	if !errors.As(err, &insuf) || insuf.TotalHeld != 1000 || insuf.Target != 5000 {
		t.Errorf("error = %q, want *InsufficientFundsError{TotalHeld: 1000, Target: 5000}", err)
	}
}

func TestSelectForAmount_UnknownAmountEntriesIgnored(t *testing.T) {
	held := []Entry{
		{ID: "tok-unknown"}, // AmountMillis == nil
		{ID: "tok-known", AmountMillis: amountPtr(5000)},
	}
	plan, err := SelectForAmount(held, 5000)
	if err != nil {
		t.Fatalf("SelectForAmount() error = %v", err)
	}
	if plan.Entry == nil || plan.Entry.ID != "tok-known" {
		t.Fatalf("plan.Entry = %+v, want tok-known", plan.Entry)
	}
}

func TestSelectForAmount_FallsThroughToASecondMinterGroupThatCovers(t *testing.T) {
	held := []Entry{
		// minterA's tokens sum to 3000 — never enough for a 5000 target.
		{ID: "tok-a1", AmountMillis: amountPtr(1000), MinterPubkey: minterPtr(minterA)},
		{ID: "tok-a2", AmountMillis: amountPtr(2000), MinterPubkey: minterPtr(minterA)},
		// minterB's do cover it.
		{ID: "tok-b1", AmountMillis: amountPtr(2500), MinterPubkey: minterPtr(minterB)},
		{ID: "tok-b2", AmountMillis: amountPtr(2500), MinterPubkey: minterPtr(minterB)},
	}
	plan, err := SelectForAmount(held, 5000)
	if err != nil {
		t.Fatalf("SelectForAmount() error = %v", err)
	}
	if len(plan.ConsolidateFirst) != 2 {
		t.Fatalf("ConsolidateFirst = %+v, want 2 entries", plan.ConsolidateFirst)
	}
	for _, e := range plan.ConsolidateFirst {
		if *e.MinterPubkey != minterB {
			t.Errorf("ConsolidateFirst included %s (minter %s), want only minterB's non-covering group excluded", e.ID, *e.MinterPubkey)
		}
	}
}

func TestSelectForAmount_GroupNeedsAllThreeEntries(t *testing.T) {
	held := []Entry{
		{ID: "tok-1", AmountMillis: amountPtr(1000), MinterPubkey: minterPtr(minterA)},
		{ID: "tok-2", AmountMillis: amountPtr(1000), MinterPubkey: minterPtr(minterA)},
		{ID: "tok-3", AmountMillis: amountPtr(1000), MinterPubkey: minterPtr(minterA)},
	}
	plan, err := SelectForAmount(held, 2500)
	if err != nil {
		t.Fatalf("SelectForAmount() error = %v", err)
	}
	if len(plan.ConsolidateFirst) != 3 {
		t.Fatalf("ConsolidateFirst = %+v, want all 3 entries (2 alone = 2000 < 2500)", plan.ConsolidateFirst)
	}
	if !plan.Split {
		t.Error("plan.Split = false, want true — consolidated sum (3000) exceeds target (2500)")
	}
}

// TestSelectForAmount_MultipleCoveringMintersPicksLexicographicallyFirst
// pins down a real, previously-unspecified policy: when MORE THAN ONE
// minter's group independently covers target, sortedKeys' own
// lexicographic ordering (not "fewest tokens," "least overshoot," or
// "most-recently-received") decides which one SelectForAmount commits
// to — the loop returns on the first covering group it finds, in that
// order, never comparing candidates against each other. Without this
// test, that tie-break is only an accident of GroupByMinter's map
// iteration plus sortedKeys' sort.Strings — this pins it down as
// intentional, deterministic behavior a future refactor can't silently
// change (e.g. to a "biggest overshoot loses" heuristic) without a test
// failing to flag it.
func TestSelectForAmount_MultipleCoveringMintersPicksLexicographicallyFirst(t *testing.T) {
	held := []Entry{
		// Neither minter's tokens individually exceed target (5000), so
		// cases 1-2 can't resolve this alone — both groups must actually
		// reach case 3's grouping logic, each independently summing to
		// cover it. minterB sorts AFTER minterA lexicographically, but is
		// listed first here — proves the choice isn't "whichever happens
		// to appear earlier in `held`," only sortedKeys' own ordering.
		{ID: "tok-b1", AmountMillis: amountPtr(3000), MinterPubkey: minterPtr(minterB)},
		{ID: "tok-b2", AmountMillis: amountPtr(3000), MinterPubkey: minterPtr(minterB)},
		{ID: "tok-a1", AmountMillis: amountPtr(3000), MinterPubkey: minterPtr(minterA)},
		{ID: "tok-a2", AmountMillis: amountPtr(3000), MinterPubkey: minterPtr(minterA)},
	}
	plan, err := SelectForAmount(held, 5000)
	if err != nil {
		t.Fatalf("SelectForAmount() error = %v", err)
	}
	if len(plan.ConsolidateFirst) != 2 {
		t.Fatalf("ConsolidateFirst = %+v, want 2 entries", plan.ConsolidateFirst)
	}
	for _, e := range plan.ConsolidateFirst {
		if *e.MinterPubkey != minterA {
			t.Errorf("ConsolidateFirst included %s (minter %s), want only minterA's entries (minterA sorts first lexicographically; both minters cover target equally well)", e.ID, *e.MinterPubkey)
		}
	}
}

// TestSelectForAmount_EmptyHeldIsInsufficientFunds is SelectForAmount's
// own zero-value contract: no caller in this codebase invokes it with an
// empty held (runCashTransfer's own call site guards len(l.Held()) > 0
// first — see cash_transfer.go), but SelectForAmount is exported with no
// such precondition documented, so any future caller must get a sane
// classified error, not a panic or a nonsensical plan. 0 held is an
// overdraft (ErrInsufficientFunds), not fragmentation — there's nothing
// spread across separate Hubs when there's nothing at all.
func TestSelectForAmount_EmptyHeldIsInsufficientFunds(t *testing.T) {
	plan, err := SelectForAmount(nil, 5000)
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("SelectForAmount(nil, ...) error = %v, want ErrInsufficientFunds", err)
	}
	if plan != nil {
		t.Errorf("plan = %+v, want nil alongside an error", plan)
	}
	var insuf *InsufficientFundsError
	if !errors.As(err, &insuf) || insuf.TotalHeld != 0 {
		t.Errorf("error = %q, want a *InsufficientFundsError naming 0 as TotalHeld", err.Error())
	}
}

// TestSelectForAmount_AllUnknownAmountsIsInsufficientFunds is the other
// half of TestSelectForAmount_UnknownAmountEntriesIgnored (which mixes a
// known amount in): held tokens can exist locally with no cached amount
// yet at all (e.g. every entry is a cash_transfer remainder awaiting its
// first resolveAmount call) — this must report the same 0-total
// insufficient-funds error an empty held does, not a misleading
// "you hold N" using some other field, and not a panic from
// GroupByMinter's own "every entry has a MinterPubkey" assumption
// (GroupableForConsolidation's filter must exclude these before they ever
// reach it).
func TestSelectForAmount_AllUnknownAmountsIsInsufficientFunds(t *testing.T) {
	held := []Entry{
		{ID: "tok-1"}, // AmountMillis == nil
		{ID: "tok-2"}, // AmountMillis == nil
	}
	_, err := SelectForAmount(held, 5000)
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("SelectForAmount() error = %v, want ErrInsufficientFunds", err)
	}
	var insuf *InsufficientFundsError
	if !errors.As(err, &insuf) || insuf.TotalHeld != 0 {
		t.Errorf("error = %q, want a *InsufficientFundsError naming 0 as TotalHeld (unknown amounts contribute nothing)", err.Error())
	}
}

func TestSumAmounts(t *testing.T) {
	entries := []Entry{
		{AmountMillis: amountPtr(1000)},
		{AmountMillis: nil},
		{AmountMillis: amountPtr(2500)},
	}
	if got := SumAmounts(entries); got != 3500 {
		t.Errorf("SumAmounts() = %d, want 3500", got)
	}
}
