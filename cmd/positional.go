package cmd

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/output"
)

// resolvePositionalOrFlag lets a value be supplied either as a bare
// positional argument (the casual path — no flag name to remember) or via
// an explicit flag, which keeps working completely unchanged for scripted/
// agentic use. Never both with conflicting values: that's treated as a
// usage mistake rather than silently preferring one. positional is "" when
// no positional argument was given; flagValue is "" when the flag wasn't
// set — either or both may be empty, in which case the result is "".
// Both are trimmed before anything else, same as decode.go/cash_receive.go.
func resolvePositionalOrFlag(cmd *cobra.Command, positional, flagName, flagValue string) (string, error) {
	positional = strings.TrimSpace(positional)
	flagValue = strings.TrimSpace(flagValue)
	if positional != "" && flagValue != "" && positional != flagValue {
		return "", output.UsageError(cmd, fmt.Errorf(
			"got both a positional argument (%q) and --%s (%q) with different values — pass only one", positional, flagName, flagValue))
	}
	if positional != "" {
		return positional, nil
	}
	return flagValue, nil
}

// resolvePositionalOrFlagUint64 is resolvePositionalOrFlag's counterpart
// for a numeric flag whose zero value means "not set" (cash_transfer's
// --split, in millis) — used for transfer's optional positional amount.
func resolvePositionalOrFlagUint64(cmd *cobra.Command, positional, flagName string, flagValue uint64) (uint64, error) {
	if positional == "" {
		return flagValue, nil
	}
	parsed, err := strconv.ParseUint(positional, 10, 64)
	if err != nil {
		return 0, output.InvalidInputError(cmd, positional, fmt.Errorf("amount must be a whole number of %s", output.CurrencyUnit))
	}
	if flagValue != 0 && flagValue != parsed {
		return 0, output.UsageError(cmd, fmt.Errorf(
			"got both a positional amount (%d) and --%s (%d) with different values — pass only one", parsed, flagName, flagValue))
	}
	return parsed, nil
}
