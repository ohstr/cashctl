package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// cashCommandFile reports a file of the cash, circle, order, withdraw or
// discover commands, or the connection-string reader: none may read the
// nostr+walletconnect:// URI form (T-111).
func cashCommandFile(name string) bool {
	for _, prefix := range []string{"cash_", "circle", "order", "withdraw", "discover", "redeem_private", "receive_secure", "connstr", "dial."} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// uriFormPending are cash-path files that still read the URI form, each until a
// named change replaces it. Empty it as they land.
var uriFormPending = map[string]string{
	// Joins dial the circle over its URI until the signed circlehub1 (NIP-CW;
	// needs nmilat's signed circlehub1).
	"circle.go": "WP-10",
}

func TestExtwalletIsolation_CashFilesNeverReadTheURIForm(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || !cashCommandFile(f) {
			continue
		}
		checked++
		full, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var hits []string
		for _, imp := range full.Imports {
			if strings.Contains(imp.Path.Value, "internal/extwallet") {
				hits = append(hits, "imports extwallet")
			}
		}
		ast.Inspect(full, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "ParsePairingURI" {
				hits = append(hits, "calls ParsePairingURI")
			}
			return true
		})
		if len(hits) == 0 {
			continue
		}
		if wp, ok := uriFormPending[f]; ok {
			t.Logf("%s still reads the URI form (until %s)", f, wp)
			continue
		}
		t.Errorf("%s %s: no cash credential relates to the nostr+walletconnect:// form", f, strings.Join(hits, ", "))
	}
	if checked == 0 {
		t.Fatal("no cash command file found: the check would pass vacuously")
	}
}
