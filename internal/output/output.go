// Package output provides cashctl's dual human/JSON rendering, its error
// classification/exit-code contract (mirroring ncli's own cli/common
// conventions so an agent or script that already knows one knows both),
// and NWC-error-to-plain-language translation for human mode.
package output

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// CurrencyUnit is the unit label cashctl uses for an amount a human
// *types* — flag/positional descriptions, --help text,
// input-validation errors. cashctl's CLI surface is loki-only end to
// end: every amount a human types (parsed via ParseAmount) or reads
// (rendered via FormatAmount) is loki. mloki — the actual wire/ledger
// granularity everything is stored and computed in — never appears
// anywhere in that surface; it's an internal implementation detail
// ParseAmount/FormatAmount convert across at the boundary. Hardcoded for
// now, but centralized here so a future per-deployment unit preference is
// a one-line change instead of a hunt through every call site.
const CurrencyUnit = "loki"

// mlokiPerLoki is cashctl's fixed unit ratio: 1 loki == 1000 mloki (the
// "milli" prefix is literal). The only place this ratio is spelled out —
// ParseAmount and FormatAmount are the only things that should ever
// multiply/divide by it.
const mlokiPerLoki = 1000

// ParseAmount parses a human-typed loki amount — FormatAmount's inverse
// — into whole mloki, cashctl's actual wire/ledger granularity. Accepts
// an optional single "." followed by 1-3 fractional digits (mloki is
// loki's finest representable unit, so anything past 3 decimal places
// isn't a real amount). Every --amount/--max-amount flag and every
// positional amount argument goes through this, so a human never has to
// type or think in mloki (see CurrencyUnit's own doc comment).
func ParseAmount(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("amount is required")
	}
	whole, frac, hasFrac := strings.Cut(s, ".")
	if hasFrac && (frac == "" || len(frac) > 3 || strings.Contains(frac, ".")) {
		return 0, fmt.Errorf("%q is not a valid amount in loki — up to 3 decimal places", s)
	}
	if whole == "" {
		whole = "0"
	}
	wholeLoki, err := strconv.ParseUint(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a valid amount in loki", s)
	}
	var fracMloki uint64
	if hasFrac {
		fracMloki, err = strconv.ParseUint(frac+strings.Repeat("0", 3-len(frac)), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not a valid amount in loki", s)
		}
	}
	return wholeLoki*mlokiPerLoki + fracMloki, nil
}

// FormatAmount renders an mloki amount — the unit every amount cashctl
// receives, computes, or stores actually is — as the "<N>[.<frac>] loki"
// string cashctl prints in human-readable text, ParseAmount's inverse.
// This is the one place the mloki -> loki conversion happens on the way
// out, so a future unit change (or a change to how much sub-unit
// precision to show) is a one-function fix instead of a hunt through
// every fmt.Printf call site. --json output stays in mloki, unconverted,
// on purpose: mloki is the wire/ledger granularity an amount can
// actually take, so it's what a script gets back; loki is only ever a
// friendlier way for a human to *read* that same number.
func FormatAmount(mloki int64) string {
	neg := mloki < 0
	if neg {
		mloki = -mloki
	}
	whole, frac := mloki/mlokiPerLoki, mloki%mlokiPerLoki
	s := strconv.FormatInt(whole, 10)
	if frac != 0 {
		s += "." + strings.TrimRight(fmt.Sprintf("%03d", frac), "0")
	}
	if neg {
		s = "-" + s
	}
	return s + " loki"
}

// PrintJSON writes v as indented JSON to stdout — the shared success-path
// result renderer every --json command uses, so stdout only ever carries
// the operation's actual result, never narration or errors (see
// EmitError, which always writes to stderr instead).
func PrintJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "cashctl: failed to encode JSON output: %s\n", err)
	}
}

// EmitError prints err to stderr — never stdout, in either mode, so a
// script parsing stdout's JSON result never has to distinguish a success
// shape from a failure shape on the same stream. Under --json this is a
// {"error","code","retryable","input"?,"nwc_code"?} object; otherwise a
// plain "Error: ..." line.
func EmitError(cmd *cobra.Command, err error) {
	if err == nil {
		return
	}
	jsonMode := false
	if cmd != nil {
		jsonMode, _ = cmd.Flags().GetBool("json")
	}
	ce := AsCLIError(err)
	if jsonMode {
		// RawMessage (when set — an NWC decline, see nwc_errors.go) is the
		// wallet's own specific text; ce.Err.Error() here would be the same
		// deliberately-generic bucket sentence human mode prints, discarding
		// exactly the detail a --json consumer is most likely to want.
		errMessage := ce.Err.Error()
		if ce.RawMessage != "" {
			errMessage = ce.RawMessage
		}
		payload := map[string]any{
			"error":     errMessage,
			"code":      string(ce.Code),
			"retryable": retryableCodes[ce.Code],
		}
		if ce.Input != "" {
			payload["input"] = ce.Input
		}
		if ce.NWCCode != "" {
			payload["nwc_code"] = ce.NWCCode
		}
		enc := json.NewEncoder(os.Stderr)
		enc.SetIndent("", "  ")
		_ = enc.Encode(payload)
		return
	}
	fmt.Fprintf(os.Stderr, "Error: %s\n", ce.Err.Error())
}

// Sanitize replaces ASCII control characters (0x00-0x1F, 0x7F — every
// ANSI escape sequence starts with one, ESC 0x1B) with U+FFFD before s
// reaches a terminal. Apply to any text from outside cashctl's own
// control (a Hub's label, a relay URL, get_info fields, an NWC error
// message, ...) before printing — otherwise it could rewrite or hide
// what's shown. Replaced, not deleted, so tampering stays visible.
func Sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			b.WriteRune('�')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Linef prints a human-only narration line (a progress note, a status
// update) to stdout — a no-op under --json, since a JSON consumer only
// wants the final structured result, never narration mixed into the same
// stream it's parsing as JSON.
func Linef(jsonMode bool, format string, args ...any) {
	if jsonMode {
		return
	}
	fmt.Printf(format+"\n", args...)
}
