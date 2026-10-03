package cmd

// Audit D, CLI surface, finding 3 (D-CLI-3), the structural half.
//
// The value-level half lives in internal/output: PrintJSON and EmitError
// sanitize, so anything that goes through them is safe. This test is what makes
// that mean something, by asserting the invariant that there is no other way out:
//
//	no function in package cmd writes to a terminal except through internal/output.
//
// WHY THIS INVARIANT AND NOT "EVERY HUB FIELD IS SANITIZED". The obvious test —
// enumerate the Hub-controlled fields and require a Sanitize call near each — is
// the exact shape that already failed once here. auditC_ledger_amount_invariant_test.go
// records it: the audit report named two sites that persisted a Hub-supplied
// amount, walking the package found four, and "a per-site regression test would
// have locked in the report's own blind spot". An enumeration of Hub-controlled
// strings would be strictly worse than the amount case, because AmountMillis is
// one identifier while "a string the Hub chose" is unbounded and grows with every
// SDK field.
//
// So it is inverted. Make the number of output boundaries small, then assert
// structurally that nothing bypasses them. The chain is: every print goes through
// internal/output; internal/output sanitizes; therefore every print is sanitized.
//
// This invariant is decidable from the syntax alone, which is the property that
// matters. "This string came from the network" is not decidable that way, cannot
// be checked without taint analysis, and can be satisfied by a comment claiming
// it — which this round found three untrue examples of. "Does this file call
// fmt.Printf" has exactly one failure mode: someone adds a new direct print. That
// is the failure mode that actually happens.
//
// Cost, paid once: 102 call sites across 14 files converted to output's
// sanitizing wrappers. That is also why the wrappers exist as drop-in renames
// rather than everything being rewritten into Linef/Notef — see their doc
// comment.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// writingFmtFuncs are the fmt functions that put bytes on a stream. Sprintf and
// Errorf are deliberately absent: they format without writing, so they cannot
// reach a terminal on their own and their results flow into output's wrappers
// like any other string.
var writingFmtFuncs = map[string]bool{
	"Print": true, "Printf": true, "Println": true,
	"Fprint": true, "Fprintf": true, "Fprintln": true,
}

// fmtWriteAllowlist names the functions permitted to call fmt directly, with the
// reason. Keep this list as short as it is.
var fmtWriteAllowlist = map[string]string{
	// The spinner emits cashctl's OWN control characters — \r to redraw a frame
	// in place and \033[K to erase the line. Routing those through the
	// sanitizing wrappers would replace exactly those bytes with U+FFFD and
	// leave a trail of broken frames instead of an animation. Its `message`
	// argument, which a caller builds from a wallet name or a Hub label, IS
	// sanitized separately at the top of the function.
	"withSpinner": "writes its own \\r and \\033[K animation frames",
}

func TestAuditD_CLI_NothingOutsideOutputWritesToATerminal(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	fset := token.NewFileSet()
	var checkedFiles, checkedFuncs int

	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checkedFiles++

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			checkedFuncs++
			if reason, allowed := fmtWriteAllowlist[fn.Name.Name]; allowed {
				t.Logf("allowlisted: %s in %s (%s)", fn.Name.Name, name, reason)
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "fmt" || !writingFmtFuncs[sel.Sel.Name] {
					return true
				}
				t.Errorf("%s: %s calls fmt.%s directly — every write to a terminal in package cmd must go through internal/output, which sanitizes it (use output.%s, or output.Linef/Notef). %s",
					fset.Position(call.Pos()), fn.Name.Name, sel.Sel.Name, sel.Sel.Name,
					"If this genuinely needs raw control characters, add it to fmtWriteAllowlist with a reason.")
				return true
			})
		}
	}

	// A floor, so the test cannot pass by having walked nothing — the way a
	// structural test quietly stops testing anything is a glob or a build tag
	// that no longer matches.
	if checkedFiles < 10 {
		t.Errorf("only walked %d files in package cmd; the glob is probably wrong", checkedFiles)
	}
	if checkedFuncs < 100 {
		t.Errorf("only walked %d functions in package cmd; the walk is probably wrong", checkedFuncs)
	}
}
