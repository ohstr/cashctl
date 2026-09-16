package ledger

import (
	"errors"
	"fmt"
	"sort"
)

// SelectionPlan describes how transfer should reach exactly Target,
// picked from a caller's held tokens (docs/ux-review.md Part 2). Exactly
// one of Entry or ConsolidateFirst is set.
type SelectionPlan struct {
	// Entry is the single held token to act on, when one alone covers
	// Target (cases 1-2 below).
	Entry *Entry
	// ConsolidateFirst is a same-minter subset that must be merged (via
	// cash_consolidate) before transferring from the result, when no
	// single token covers Target on its own (case 3 below).
	ConsolidateFirst []Entry
	// Split is true when the selected/consolidated token's amount
	// exceeds Target (a split-transfer is needed); false when it equals
	// Target exactly (a plain full transfer).
	Split bool
}

// ErrFundsFragmented is wrapped into the error SelectForAmount returns
// when no single minter's held tokens sum to Target — a real, coherent
// "send as two separate transfers to two different minters' wallets"
// option exists in principle, but isn't offered silently under one
// confirmation (see docs/ux-review.md Part 2, case 4).
var ErrFundsFragmented = errors.New("funds are fragmented across separate Hubs")

// SelectForAmount picks a plan to reach exactly target from held (already
// filtered to StatusHeld — see Ledger.Held), preferring, in order:
//
//  1. one held token whose amount equals target exactly — full transfer.
//  2. the smallest held token that still covers target — split-transfer
//     it (best-fit, not largest-available, to avoid needlessly
//     fragmenting a big token when a smaller one would do).
//  3. the smallest same-minter subset (by token count) that sums to
//     cover target — consolidate it first, then (split-)transfer from
//     the result. Only pubkey-mode entries with a known MinterPubkey are
//     eligible for this grouping: cash_consolidate only accepts
//     pubkey-identified sources, and an entry with no MinterPubkey can't
//     be reliably grouped with anything (see docs/ux-review.md Part 2's
//     "Why 'same minter' needs its own field").
//
// Cases 1-2 consider every held entry with a known AmountMillis,
// regardless of identity mode — a plain single-token transfer or split
// doesn't need cash_consolidate at all, so the pubkey-mode/minter-known
// restriction doesn't apply there.
func SelectForAmount(held []Entry, target uint64) (*SelectionPlan, error) {
	var bestFit *Entry
	for i := range held {
		e := &held[i]
		if e.AmountMillis == nil {
			continue
		}
		switch {
		case *e.AmountMillis == target:
			return &SelectionPlan{Entry: e}, nil
		case *e.AmountMillis > target:
			if bestFit == nil || *bestFit.AmountMillis > *e.AmountMillis {
				bestFit = e
			}
		}
	}
	if bestFit != nil {
		return &SelectionPlan{Entry: bestFit, Split: true}, nil
	}

	groups := GroupByMinter(GroupableForConsolidation(held))
	var totalHeld uint64
	for _, e := range held {
		if e.AmountMillis != nil {
			totalHeld += *e.AmountMillis
		}
	}

	for _, minter := range sortedKeys(groups) {
		subset, sum, ok := smallestCoveringSubset(groups[minter], target)
		if ok {
			return &SelectionPlan{ConsolidateFirst: subset, Split: sum > target}, nil
		}
	}

	return nil, fmt.Errorf("%w: you hold %d total, but no single minter's tokens sum to the %d you're sending",
		ErrFundsFragmented, totalHeld, target)
}

// SumAmounts totals every entry's known AmountMillis (entries with none
// contribute 0) — used to describe a planned consolidation before it
// executes, from already-cached local data, no network call needed.
func SumAmounts(entries []Entry) uint64 {
	var sum uint64
	for _, e := range entries {
		if e.AmountMillis != nil {
			sum += *e.AmountMillis
		}
	}
	return sum
}

// GroupableForConsolidation returns the subset of held eligible to be
// grouped for auto-consolidation: pubkey-mode (not bearer-, not
// connection-key-bound — cash_consolidate only accepts pubkey-identified
// sources) with a known MinterPubkey (the only client-side "same minter"
// signal cashctl has) and a known amount (nothing to sum otherwise).
func GroupableForConsolidation(held []Entry) []Entry {
	var out []Entry
	for _, e := range held {
		if e.AmountMillis == nil || e.MinterPubkey == nil {
			continue
		}
		isBearer := e.IdentityRequired != nil && !*e.IdentityRequired
		if isBearer || e.ConnectionKeyPlatform != "" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// GroupByMinter buckets entries (already filtered to consolidation-
// eligible ones — see GroupableForConsolidation) by their own
// MinterPubkey. Every entry passed in is assumed to have one set; panics
// otherwise (guaranteed by GroupableForConsolidation's own filter, not
// re-checked here).
func GroupByMinter(entries []Entry) map[string][]Entry {
	groups := make(map[string][]Entry)
	for _, e := range entries {
		groups[*e.MinterPubkey] = append(groups[*e.MinterPubkey], e)
	}
	return groups
}

func sortedKeys(groups map[string][]Entry) []string {
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// smallestCoveringSubset greedily accumulates group's largest entries
// first until their sum covers target — minimizing token count (and, in
// the common case, overshoot) rather than arbitrarily consolidating every
// held token from that minter. Returns ok=false if even the whole group
// doesn't sum to target.
func smallestCoveringSubset(group []Entry, target uint64) (subset []Entry, sum uint64, ok bool) {
	sorted := make([]Entry, len(group))
	copy(sorted, group)
	sort.Slice(sorted, func(i, j int) bool { return *sorted[i].AmountMillis > *sorted[j].AmountMillis })

	for _, e := range sorted {
		subset = append(subset, e)
		sum += *e.AmountMillis
		if sum >= target {
			return subset, sum, true
		}
	}
	return nil, 0, false
}
