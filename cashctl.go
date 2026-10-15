// Command cashctl is a wallet CLI for NIP-CASH cash and NIP-CW circle
// wallets — see README.md and AGENTS.md.
package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/ohstr/cashctl/cmd"
	"github.com/ohstr/cashctl/internal/output"
)

func main() {
	os.Exit(run())
}

// run executes RootCmd and returns the process exit code — a single,
// central sink for every command's failure (see internal/output's error
// contract), matching ncli's own main()/EmitError/ExitCode pattern.
func run() int {
	executed, err := cmd.RootCmd.ExecuteC()
	if err != nil {
		err = classifyRootErr(executed, err)
		emitFor(executed, err)
		return output.ExitCode(err)
	}
	return 0
}

// classifyRootErr catches an error straight from cobra's own command/flag
// resolution (an unknown command or unknown flag) — these fail inside
// ExecuteC itself, before any RunE/output.ExactArgs et al. runs, so
// they'd otherwise surface as an unclassified exit 1 instead of a usage
// mistake's exit 2. Worse, ParseFlags never completes on this path
// either, so cmd.Flags().GetBool("json") would read --json's zero value
// even when it was passed — fixed up here by re-scanning os.Args
// directly and setting the flag before handing off to output.UsageError,
// so this one failure mode still honors --json like every other error.
func classifyRootErr(cmd *cobra.Command, err error) error {
	var ce *output.CLIError
	if errors.As(err, &ce) {
		return err // already classified further down the call stack
	}
	if cmd != nil && jsonRequested(os.Args[1:]) {
		_ = cmd.Flags().Set("json", "true")
	}
	return output.InvocationError(cmd, withHelpHint(cmd, err))
}

// withHelpHint ends an unknown flag or command error with the flag it most
// likely meant and the --help to read, which --json mode otherwise never
// shows.
func withHelpHint(cmd *cobra.Command, err error) error {
	if cmd == nil {
		return err
	}
	msg := err.Error()
	name, isFlag := strings.CutPrefix(msg, "unknown flag: --")
	if !isFlag && !strings.HasPrefix(msg, "unknown command ") && !strings.HasPrefix(msg, "unknown shorthand flag: ") {
		return err
	}
	hint := ""
	if isFlag {
		if guess := closestFlag(cmd, name); guess != "" {
			hint = fmt.Sprintf("; did you mean --%s?", guess)
		}
	}
	return fmt.Errorf("%s%s\nSee `%s --help`.", strings.TrimRight(msg, "\n"), hint, cmd.CommandPath())
}

// closestFlag is the flag of cmd within two edits of name, or one name
// starts with the other; "" when none is.
func closestFlag(cmd *cobra.Command, name string) string {
	best, bestDist := "", 3
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Hidden {
			return
		}
		d := editDistance(name, f.Name)
		if d > 2 && (strings.HasPrefix(f.Name, name) || strings.HasPrefix(name, f.Name)) {
			d = 2
		}
		if d < bestDist {
			best, bestDist = f.Name, d
		}
	})
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// jsonRequested reports whether args turn --json on, the way cobra itself
// would have parsed it had it got that far: bare --json, or --json=<bool>
// (strconv.ParseBool's spellings — true/1/t/T/TRUE/True), the last one
// winning, and nothing past a "--" terminator counting. Matching only the
// literal "--json" meant `--json=true` got a plain-text error on a stream a
// script had explicitly asked to keep machine-readable.
func jsonRequested(args []string) bool {
	on := false
	for _, a := range args {
		if a == "--" {
			break
		}
		if a == "--json" {
			on = true
		} else if v, ok := strings.CutPrefix(a, "--json="); ok {
			if b, err := strconv.ParseBool(v); err == nil {
				on = b
			}
		}
	}
	return on
}

// emitFor is a small indirection so tests (see main_test.go) can exercise
// run()'s exit-code logic without cobra's own os.Exit-adjacent side
// effects getting in the way.
func emitFor(cmd *cobra.Command, err error) {
	output.EmitError(cmd, err)
}
