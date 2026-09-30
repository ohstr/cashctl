package cmd

// Audit C, SDK-abstraction role, surface 2, the structural half.
//
// C-SDK-2 was not "validateClaimedAmount is wrong". The guard was correct and had a
// doc comment naming the exact attacker. The defect was that it was called from ONE of
// the places that persist a Hub-supplied amount, and the report named two of the
// others. Walking the package for this invariant turned up FOUR sites, not two:
//
//	cash_receive.go   (had the guard)
//	redeem_private.go recordQuote
//	cash_redeem.go    resolveRedeemQuote
//	cash_redeem.go    resolveAmount               ← not in the report; the widest of them,
//	                                               reached by redeem, transfer AND consolidate
//	cash_transfer.go  printAndSaveTransferResult  ← not in the report; a brand-new entry
//
// So a per-site regression test would have locked in the report's own blind spot. This
// one asserts the invariant instead: any function that writes an amount onto a ledger
// entry must, in the same function, reference one of the bounds. It fails on a fifth
// site added later, which is the failure mode that actually happened.
//
// It is a source-structure test, and it is worth being honest about the limits: it
// cannot tell that the bound was applied to the right value, only that the function
// knows the bound exists. The value-level assertions are the matrix in
// auditC_sdk_quote_unvalidated_test.go. These two together are the finding's regression;
// neither is sufficient alone.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// theBounds are the ways a function may legitimately show that it bounded a
// Hub-supplied amount before recording it.
var theBounds = []string{
	"validateQuotedAmounts", // the redeem/quote paths and resolveAmount
	"validateClaimedAmount", // receive, which additionally demands exact provenance
	"maxSaneAmountMillis",   // an inline bound, for a path that must degrade rather than refuse
}

func TestAuditC_EveryLedgerAmountWriteIsBounded(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	fset := token.NewFileSet()
	checked := 0

	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}

			var writes []token.Position
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				assign, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for _, lhs := range assign.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "AmountMillis" {
						continue
					}
					// Only a write onto a ledger ENTRY counts. A plain identifier
					// receiver whose name says "entry" is what every such site looks
					// like; a write into a local quote or an outcome slot (an IndexExpr
					// receiver, or a `q`) is not persisted state and is excluded by
					// construction rather than by an allowlist.
					recv, ok := sel.X.(*ast.Ident)
					if !ok || !strings.Contains(strings.ToLower(recv.Name), "entry") {
						continue
					}
					writes = append(writes, fset.Position(assign.Pos()))
				}
				return true
			})

			if len(writes) == 0 {
				continue
			}
			checked++

			// Matched against the identifiers the function actually USES, via the AST,
			// not against its source text. The first version of this test searched the
			// text and passed against both mutants, because the explanatory comments
			// sitting above each call site name the guards — so removing the calls and
			// leaving the comments satisfied it. A test that survives its own mutant is
			// not evidence.
			bounded := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				ident, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				for _, bound := range theBounds {
					if ident.Name == bound {
						bounded = true
						return false
					}
				}
				return true
			})
			if !bounded {
				t.Errorf("%s writes a Hub-supplied amount onto a ledger entry at %v "+
					"without referencing any of %v — a hostile relay (a token's own RelayURLs "+
					"are dialed automatically) can then persist an arbitrary \"verified\" balance, "+
					"and one figure past math.MaxInt64 makes `wallet balance` report a negative "+
					"total for every held bill",
					fn.Name.Name, writes, theBounds)
			}
		}
	}

	// A floor, so the test cannot pass by finding nothing — the shape it is guarding
	// against includes someone renaming the variable out of its reach.
	if checked < 4 {
		t.Fatalf("found only %d functions writing a ledger amount, want at least 4 "+
			"(recordQuote, resolveRedeemQuote, resolveAmount, printAndSaveTransferResult, "+
			"plus receive's own) — the walk has stopped seeing the sites it is meant to check", checked)
	}
	t.Logf("%d functions write a ledger amount; all bounded", checked)
}
