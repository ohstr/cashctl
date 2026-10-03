// Package output provides cashctl's dual human/JSON rendering, its error
// classification/exit-code contract (mirroring ncli's own cli/common
// conventions so an agent or script that already knows one knows both),
// and NWC-error-to-plain-language translation for human mode.
package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
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
	// Every downstream consumer of this value eventually treats it as an
	// mloki quantity that fits in an int64 — FormatAmount's own signature,
	// nipcash's wire types, wallet_ops.go's invoice amount cast, ... — so
	// this is the one place on the whole CLI to catch an amount that
	// would otherwise silently wrap: unchecked, wholeLoki*mlokiPerLoki
	// alone overflows uint64 well within a typeable number of digits
	// (18446744073709552 loki used to become 384 mloki and actually
	// move), and a value between MaxInt64 and MaxUint64 would separately
	// wrap NEGATIVE the moment any downstream int64(...) cast touches it.
	// Bounding at MaxInt64 up front makes both classes of wrap
	// unreachable rather than relying on every cast site to notice.
	const maxAmountMloki = uint64(math.MaxInt64)
	if wholeLoki > maxAmountMloki/mlokiPerLoki {
		return 0, fmt.Errorf("%q is too large — the largest amount cashctl accepts is %s", s, FormatAmount(math.MaxInt64))
	}
	total := wholeLoki*mlokiPerLoki + fracMloki
	if total > maxAmountMloki {
		return 0, fmt.Errorf("%q is too large — the largest amount cashctl accepts is %s", s, FormatAmount(math.MaxInt64))
	}
	return total, nil
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

// NonNil returns s, or an empty (non-nil) slice if s is nil. encoding/json
// renders a nil slice as `null` and an empty one as `[]` — and a --json
// consumer iterating a collection (`jq '.[]'`, a typed decoder) needs the
// latter for "nothing here", not a value it has to special-case. Wrap every
// collection put into --json output that can legitimately be empty.
func NonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// PrintJSON writes v as indented JSON to stdout — the shared success-path
// result renderer every --json command uses, so stdout only ever carries
// the operation's actual result, never narration or errors (see
// EmitError, which always writes to stderr instead).
func PrintJSON(v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cashctl: failed to encode JSON output: %s\n", err)
		return
	}
	// Sanitized generically, over the encoded tree, rather than per command.
	// Every --json shape carries Hub-chosen strings somewhere (a label, a relay
	// URL, an identity_value, a wallet's own error text), and doing it at each
	// call site is what failed: `decode` sanitized a Hub label for human output
	// and emitted it raw into --json four lines below, and `cash status`
	// sanitized identity_type/identity_value while `transfer` put the same two
	// fields in raw. Doing it here covers every existing shape and every future
	// one, with no per-command edits to forget.
	//
	// Worth being exact about the threat, because the bytes look safe:
	// encoding/json escapes ESC to \u001b, so stdout is inert until something
	// parses it — and `jq -r '.label'` is precisely that something, turning it
	// back into a live escape sequence on the operator's terminal. C1 and bidi
	// are not escaped at all, so those reach a terminal with no parsing step.
	if safe, sErr := sanitizeJSONBytes(raw); sErr == nil {
		raw = safe
	}
	// Hardening must never be why a correct result fails to print: on any error
	// above, raw is still the faithful encoding and goes out as-is.
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		buf.Reset()
		buf.Write(raw)
	}
	buf.WriteByte('\n')
	if _, err := os.Stdout.Write(buf.Bytes()); err != nil {
		fmt.Fprintf(os.Stderr, "cashctl: failed to write JSON output: %s\n", err)
	}
}

// sanitizeJSONBytes re-encodes raw with every string sanitized.
//
// UseNumber is not optional. Decoding into `any` without it turns every number
// into a float64, and an amount is bounded at math.MaxInt64 — so any figure above
// 2^53 would come back out having silently lost precision. Losing a digit off an
// amount would be a far worse bug than the one this function exists to fix.
// json.Number re-marshals as its original literal, so numbers round-trip exactly.
func sanitizeJSONBytes(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, err
	}
	return json.Marshal(sanitizeJSONValue(tree))
}

// sanitizeJSONValue walks a decoded JSON tree sanitizing every string, map KEYS
// included — a hostile key reaches a terminal through `jq` just as readily as a
// hostile value, and a Hub gets to choose keys wherever a map is keyed by
// something it supplied.
//
// SanitizeText, not Sanitize: a JSON string value may legitimately hold newlines
// (multi-line advice, a composed recovery line), and a newline cannot rewrite a
// terminal the way CR and the escape introducers can.
func sanitizeJSONValue(v any) any {
	switch t := v.(type) {
	case string:
		return SanitizeText(t)
	case []any:
		for i, e := range t {
			t[i] = sanitizeJSONValue(e)
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[SanitizeText(k)] = sanitizeJSONValue(e)
		}
		return out
	default:
		return v
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
		// Sanitized here rather than relying on PrintJSON's generic round trip:
		// EmitError writes its own encoder to stderr, and these fields are few
		// and known, so doing it explicitly keeps it auditable. SanitizeText for
		// the message, which is composed and may legitimately be multi-line
		// (noWalletConfiguredMsg is three lines); strict Sanitize for the single
		// discrete values, where a newline would mean nothing good.
		payload := map[string]any{
			"error":     SanitizeText(errMessage),
			"code":      Sanitize(string(ce.Code)),
			"retryable": retryableCodes[ce.Code],
		}
		if ce.Input != "" {
			payload["input"] = Sanitize(ce.Input)
		}
		if ce.NWCCode != "" {
			payload["nwc_code"] = Sanitize(ce.NWCCode)
		}
		// Sanitized but never capped (see CLIError.Recovery): a truncated
		// <token>#<secret> is as useless as a redacted one. Sanitize cannot
		// shorten it — it substitutes rune-for-rune — so this is safe here in a
		// way a length bound would not be.
		if ce.Recovery != "" {
			payload["recovery"] = Sanitize(ce.Recovery)
		}
		enc := json.NewEncoder(os.Stderr)
		enc.SetIndent("", "  ")
		_ = enc.Encode(payload)
		return
	}
	msg := ce.Err.Error()
	// RawMessage, appended (not substituted) when it says more than the
	// generic bucket sentence above it: a translated NWC code's text is
	// deliberately generic ("The wallet hit an internal error. Try
	// again.") and used to be the ONLY thing human mode ever printed for
	// it — actively misleading on INTERNAL/OTHER above all, since those
	// are catch-all buckets for whatever didn't fit a more specific code,
	// not necessarily a transient condition worth retrying. Equal check:
	// an unrecognized NWC code has no translation, so RawMessage IS
	// ce.Err.Error() already (see NWCError/NWCErrorForCashToken) —
	// appending it there would just repeat the same sentence twice.
	if ce.RawMessage != "" && ce.RawMessage != msg {
		msg = fmt.Sprintf("%s (%s)", msg, ce.RawMessage)
	}
	// SanitizeText at the sink: msg is cashctl's own sentence with a wallet's
	// own text appended, and until now nothing on this path sanitized either, so
	// a Hub could rewrite or hide what the user was shown. Newline-permitting,
	// because several of cashctl's own messages are deliberately multi-line.
	fmt.Fprintf(os.Stderr, "%s %s\n", errorPrefix(isColorTerminal(os.Stderr)), SanitizeText(msg))
	// On its own line, with no "Error:" prefix, so it can be selected and
	// copied as-is — this is the one error whose text the user has to act on
	// character-for-character.
	if ce.Recovery != "" {
		fmt.Fprintf(os.Stderr, "%s\n", Sanitize(ce.Recovery))
	}
	// ShowUsage (InvocationError, see its own doc comment): a genuine
	// malformed-invocation error — wrong arg count, unknown flag/command, a
	// missing or conflicting flag — follows the "Error: ..." line with the
	// command's own full --help content — the same content
	// `cashctl <cmd> --help` prints, reused via cmd.Help() rather than
	// hand-duplicating cobra's own template, redirected to stderr
	// (cmd.SetOut) to keep AGENTS.md's "narration/errors to stderr always"
	// contract — a plain --help invocation (exit 0, not an error) is
	// unaffected and still goes to stdout via cobra's own default. Never
	// for every CodeUsage error (funds-fragmented and friends stay exactly
	// as terse as before) and never in --json mode (handled by the early
	// return above; an agent has no use for any of this human framing).
	if ce.ShowUsage && cmd != nil {
		fmt.Fprintln(os.Stderr)
		cmd.SetOut(os.Stderr)
		_ = cmd.Help()
	}
}

// errorPrefix returns "Error:", wrapped in ANSI red when colored is true —
// factored out from EmitError so the wrapping itself is testable without
// depending on a real terminal.
func errorPrefix(colored bool) string {
	if !colored {
		return "Error:"
	}
	return "\x1b[31mError:\x1b[0m"
}

// isColorTerminal reports whether w is a real terminal that should get
// ANSI color codes — false whenever output is piped, redirected, or
// NO_COLOR is set (https://no-color.org), so a script or agent capturing
// cashctl's stderr text always gets byte-identical plain text, never raw
// escape codes mixed into what it parses.
func isColorTerminal(w *os.File) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	return term.IsTerminal(int(w.Fd()))
}

// Sanitize replaces ASCII control characters (0x00-0x1F, 0x7F — every
// ANSI escape sequence starts with one, ESC 0x1B) with U+FFFD before s
// reaches a terminal. Apply to any text from outside cashctl's own
// control (a Hub's label, a relay URL, get_info fields, an NWC error
// message, ...) before printing — otherwise it could rewrite or hide
// what's shown. Replaced, not deleted, so tampering stays visible.
func Sanitize(s string) string { return sanitizeRunes(s, nil) }

// SanitizeText is Sanitize for COMPOSED text — an error message, a JSON string
// value — rather than for one discrete value. It permits tab and newline, and
// nothing else extra.
//
// It has to exist because cashctl composes multi-line text of its own:
// noWalletConfiguredMsg is three lines, so running the strict Sanitize over an
// error message would turn cashctl's own newlines into U+FFFD and wreck the
// output it was meant to protect.
//
// Carriage return is deliberately NOT permitted. A bare CR returns the cursor to
// the start of the line, so it lets injected text overwrite what was already
// printed — the same spoofing the escape sequences are stripped for — and nothing
// in cashctl emits one.
func SanitizeText(s string) string { return sanitizeRunes(s, isLayoutWhitespace) }

// isLayoutWhitespace is the leniency SanitizeText adds: whitespace that shapes a
// layout without being able to rewrite one.
func isLayoutWhitespace(r rune) bool { return r == '\t' || r == '\n' }

// sanitizeRunes substitutes U+FFFD for every unsafe rune that permitted does not
// explicitly allow. A nil permitted means "allow nothing extra", which is
// Sanitize's original behaviour exactly.
func sanitizeRunes(s string, permitted func(rune) bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case permitted != nil && permitted(r):
			b.WriteRune(r)
		case isUnsafeControlRune(r):
			b.WriteRune('�')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isUnsafeControlRune reports whether r is a character cashctl never wants
// to pass through to a terminal raw. Beyond the original C0 controls
// (0x00-0x1F) and DEL (0x7F):
//   - C1 controls (U+0080-U+009F) — confirmed live: a terminal that honors
//     8-bit C1 in UTF-8 mode (xterm, some VTE builds) executes CSI (U+009B),
//     OSC (U+009D) and ST (U+009C) from this range exactly like their
//     familiar ESC-prefixed C0 equivalents, so a hostile relay URL embedded
//     in a token/hub string could clear the screen or set the window title
//     at `decode`/`receive` time — the exact gap Sanitize's own C0 check
//     was written to close, just one code point range short of it.
//   - U+2028/U+2029 (Unicode line/paragraph separator) — some terminals
//     and log viewers treat these as a hard line break, letting text
//     inject a fake extra line the same way a literal newline would.
//   - U+202A-U+202E, U+2066-U+2069 (bidi format controls) — can reorder
//     how the SAME bytes visually display, letting an attacker-chosen
//     label/URL read as something other than what it actually is.
func isUnsafeControlRune(r rune) bool {
	switch {
	case r < 0x20 || r == 0x7f:
		return true
	case r >= 0x80 && r <= 0x9f:
		return true
	case r == 0x2028 || r == 0x2029:
		return true
	case r >= 0x202a && r <= 0x202e:
		return true
	case r >= 0x2066 && r <= 0x2069:
		return true
	default:
		return false
	}
}

// Linef prints a human-readable RESULT line to stdout — text mode's
// counterpart to PrintJSON, a no-op under --json (a JSON consumer only wants
// the final structured result, never text mixed into the stream it's
// parsing). For progress, previews, warnings and pick-lists use Notef
// instead: AGENTS.md sends narration to stderr always, so a script piping a
// command's stdout gets only what the command produced.
func Linef(jsonMode bool, format string, args ...any) {
	if jsonMode {
		return
	}
	fmt.Printf(format+"\n", args...)
}

// Notef prints a human-only NARRATION line — a progress note, a preview, a
// warning, a pick-list — to stderr, a no-op under --json. Narration goes to
// stderr always (AGENTS.md) so stdout carries only the command's result and
// `cashctl ... > file` or `| jq` never captures a prompt or a spinner frame.
func Notef(jsonMode bool, format string, args ...any) {
	if jsonMode {
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}
