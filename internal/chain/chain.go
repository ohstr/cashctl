// Package chain is cashctl's compile-time native-chain identity. cashctl is
// built for one chain — Flokicoin by default — and everything a user sees or
// that defines cashctl's "own" bills (the display unit, the sub-unit ratio, the
// cash-token HRP, the chain name) reads from [Native]. A fork targeting another
// chain — a community's own coin — reassigns [Native] once, here, and
// recompiles; no call site changes.
//
// This is the code-level seam, not a runtime or external-config toggle: a
// cashctl build ships as exactly one chain's client. It is also the boundary a
// later injectable chain *library* (address/network params, a second
// LNClient) is meant to plug into; this package is the labels-and-units half of
// that, kept deliberately small — loki/mloki are sat/msat-equivalents, so the
// money arithmetic is already chain-neutral and no money logic lives here.
package chain

// Chain is cashctl's native-chain identity: the parameters that make a build "a
// Flokicoin wallet" rather than some other chain's. Labels and units only.
type Chain struct {
	// Name is the chain's NIP-CASH name ("flokicoin") — the value cashctl
	// stamps on a bill's chain field and matches nmilat's ChainForHRP against.
	Name string

	// Unit is the unit a human types and reads ("loki"). Its SubunitRatio-th
	// part (mloki) is the wire/ledger granularity and is never shown to a human.
	Unit string

	// SubunitRatio is sub-units per Unit (1000: 1 loki == 1000 mloki). Invariant
	// across the chains cashctl can target (any "milli" sub-unit), but part of
	// the identity so a fork that differs has one place to say so.
	SubunitRatio uint64

	// HRP is the cash-token human-readable prefix of a native bill ("lokicash" →
	// lokicash1…). cashctl's ledger holds only bills of this HRP.
	HRP string
}

// Flokicoin is cashctl's built-in native chain.
var Flokicoin = Chain{
	Name:         "flokicoin",
	Unit:         "loki",
	SubunitRatio: 1000,
	HRP:          "lokicash",
}

// Native is the chain cashctl is built for. Flokicoin by default; a fork
// targeting another chain reassigns this once (here) and recompiles — every
// unit label, sub-unit ratio and cash-token HRP in cashctl follows from it.
var Native = Flokicoin
