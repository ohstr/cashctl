package cmd

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/ohstr/cashctl/internal/output"
)

// The exit-code table in `cashctl --help` matches the codes cashctl really uses.
func TestRootHelp_ListsEveryExitCode(t *testing.T) {
	for code, exit := range output.ExitCodes() {
		line := regexp.MustCompile(fmt.Sprintf(`(?m)^  %d +%s `, exit, regexp.QuoteMeta(string(code))))
		if !line.MatchString(RootCmd.Long) {
			t.Errorf("root help does not list exit %d as %q", exit, code)
		}
	}
}

// `cashctl --help` names every field of the --json error object, since AGENTS.md
// doesn't ship with the binary.
func TestRootHelp_DescribesJSONErrorShape(t *testing.T) {
	for _, field := range []string{"error", "code", "retryable", "input", "nwc_code", "recovery"} {
		if !regexp.MustCompile(`"` + field + `"`).MatchString(RootCmd.Long) {
			t.Errorf("root help does not name the --json error field %q", field)
		}
	}
}
