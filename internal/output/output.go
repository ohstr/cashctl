// Package output provides cashctl's dual human/JSON rendering, its error
// classification/exit-code contract (mirroring ncli's own cli/common
// conventions so an agent or script that already knows one knows both),
// and NWC-error-to-plain-language translation for human mode.
package output

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// CurrencyUnit is the unit label cashctl prints after amounts in
// human-readable text (CLI output, --help text, docs) — hardcoded for now,
// but centralized here so a future per-deployment unit preference (e.g.
// "FLC") is a one-line change instead of a hunt through every fmt.Printf
// call site. Doesn't touch --json field names or on-disk storage, which
// stay "mloki" (the actual sub-unit amounts are stored/transmitted in) —
// this is a display label only.
const CurrencyUnit = "loki"

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
