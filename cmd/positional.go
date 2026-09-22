package cmd

import (
	"fmt"
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
		return "", output.InvocationError(cmd, fmt.Errorf(
			"got both a positional argument (%q) and --%s (%q) with different values — pass only one", positional, flagName, flagValue))
	}
	if positional != "" {
		return positional, nil
	}
	return flagValue, nil
}

// resolvePositionalOrFlagAmount is resolvePositionalOrFlag's counterpart
// for an amount flag (cash_transfer's --amount, circle join's
// --max-amount) — both the positional and the flag are loki strings a
// human typed (cashctl's CLI surface is loki-only, see CurrencyUnit's own
// doc comment); this parses whichever one(s) are set via
// output.ParseAmount and returns the result in mloki, cashctl's actual
// wire/ledger unit.
func resolvePositionalOrFlagAmount(cmd *cobra.Command, positional, flagName, flagValue string) (uint64, error) {
	positional = strings.TrimSpace(positional)
	flagValue = strings.TrimSpace(flagValue)
	if positional == "" && flagValue == "" {
		return 0, nil
	}
	if positional != "" && flagValue != "" && positional != flagValue {
		return 0, output.InvocationError(cmd, fmt.Errorf(
			"got both a positional amount (%q) and --%s (%q) with different values — pass only one", positional, flagName, flagValue))
	}
	raw := positional
	if raw == "" {
		raw = flagValue
	}
	parsed, err := output.ParseAmount(raw)
	if err != nil {
		return 0, output.InvalidInputError(cmd, raw, err)
	}
	return parsed, nil
}
