package extwallet

import (
	"os/exec"
	"strings"
	"testing"
)

// The URI form stays at the edge: no package that holds or moves cash may
// depend on this one (T-111). cmd is checked file by file in
// cmd/extwallet_isolation_test.go.
func TestIsolation_CashPackagesDoNotReachExtwallet(t *testing.T) {
	for _, pkg := range []string{
		"github.com/ohstr/cashctl/internal/ledger",
		"github.com/ohstr/cashctl/internal/store",
		"github.com/ohstr/cashctl/internal/dial",
		"github.com/ohstr/cashctl/internal/credential",
		"github.com/ohstr/cashctl/integration/fakehub",
	} {
		out, err := exec.Command("go", "list", "-deps", pkg).CombinedOutput()
		if err != nil {
			t.Fatalf("go list -deps %s: %v\n%s", pkg, err, out)
		}
		if strings.Contains(string(out), "cashctl/internal/extwallet") {
			t.Errorf("%s depends on internal/extwallet: a cash path must never read the URI form", pkg)
		}
	}
}
