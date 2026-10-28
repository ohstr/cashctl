package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The provenance gate holds only if nothing dials a bill around it: every bill
// goes through dialBill (cmd/dial.go). These source checks fail on a new
// nipcashclient.Connect anywhere else in this package, on the legacy mint
// scheme coming back anywhere in cmd or internal, and on the test-only signing
// helpers reaching shipped code.
func TestDialLint_OnlyDialBillConnectsToABill(t *testing.T) {
	for _, f := range goFiles(t, ".", false) {
		if filepath.Base(f) == "dial.go" {
			continue
		}
		src := readSource(t, f)
		if strings.Contains(src, "nipcashclient.Connect(") {
			t.Errorf("%s calls nipcashclient.Connect: dial bills through dialBill so their provenance is checked first", f)
		}
		if strings.Contains(src, `"get_balance"`) && strings.Contains(src, "nipcash") {
			t.Errorf("%s names get_balance beside a bill: a bill is never a wallet", f)
		}
	}
}

func TestDialLint_NoLegacyMintScheme(t *testing.T) {
	for _, dir := range []string{".", filepath.Join("..", "internal")} {
		for _, f := range goFiles(t, dir, true) {
			if strings.HasSuffix(f, "dial_lint_test.go") {
				continue
			}
			src := readSource(t, f)
			for _, banned := range []string{"SchemeLegacy", "LegacyMintPayload", "lokicash-mint"} {
				if strings.Contains(src, banned) {
					t.Errorf("%s names %s: there is no legacy mint scheme", f, banned)
				}
			}
		}
	}
}

func TestDialLint_TestSigningStaysInTests(t *testing.T) {
	for _, dir := range []string{".", filepath.Join("..", "internal")} {
		for _, f := range goFiles(t, dir, false) {
			if strings.Contains(f, "testmint") {
				continue
			}
			if strings.Contains(readSource(t, f), "internal/testmint") {
				t.Errorf("%s imports internal/testmint, which signs with local keys and is for tests only", f)
			}
		}
	}
}

// goFiles lists the .go files under dir, tests included only when withTests.
func goFiles(t *testing.T, dir string, withTests bool) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != dir && dir == "." {
				return filepath.SkipDir // this package only
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || (!withTests && strings.HasSuffix(path, "_test.go")) {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatalf("no Go files under %s: the check would pass vacuously", dir)
	}
	return out
}

func readSource(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
