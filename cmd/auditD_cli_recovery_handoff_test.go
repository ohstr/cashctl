package cmd

// Audit D, CLI surface, finding 6 (D-CLI-6).
//
// reportUnsavedResult exists for exactly one situation, and its own doc comment
// states the stakes: a Hub-side mutation CONFIRMED to have happened whose
// follow-up ledger.Save() then failed, leaving money that is "about to become
// undiscoverable the instant this process exits unless it's printed now". For a
// cash-mode entry the thing that makes it reachable again is
// entryRecoveryString's <token>#<cash_secret>.
//
// A cash secret is hex.EncodeToString of 32 random bytes — exactly 64 hex
// characters — so giftSecretPattern (`#[0-9a-fA-F]{64}\b`) matched it, and
// wrapCLIError's catch-all RedactSecretInput rewrote the handoff to
// `#<redacted>`. The one message whose entire purpose is handing over the
// secret printed everything except the secret, in human mode and --json alike.
//
// The catch-all redaction is right and stays; a deliberate handoff needs its own
// channel past it. These tests pin the behaviour, not the mechanism, so they
// stay honest if that channel is ever reshaped.
//
// Both halves are asserted deliberately:
//   - the secret survives, and
//   - it survives STILL PAIRED with its token.
//
// The pairing is the part that is easy to lose and easy to wave away. D-CLI-1's
// write-ahead park means `wallet show` usually still prints the bare secret
// (unredacted — a bare 64-hex has no `#`, so giftSecretPattern misses it and
// secretLikePattern is whole-string-anchored), so "the secret is recoverable
// elsewhere" is true. But a secret with no token does not tell you which bill
// it opens, and this message is the only place the two appear together.
//
// Mutants this must fail against: dropping the Recovery channel so the handoff
// goes back through RedactSecretInput; length-capping the recovery text; and
// emitting the secret without its token.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// cashRecoveryFixture builds an entry shaped like a real cash-mode holding: a
// bech32-ish token and a secret that is exactly 64 hex characters, which is the
// length that made giftSecretPattern match.
func cashRecoveryFixture(t *testing.T) (e *ledger.Entry, token, secret string) {
	t.Helper()
	token = "lokicash1" + strings.Repeat("q", 120)
	secret = strings.Repeat("a1b2c3d4", 8)
	if len(secret) != 64 {
		t.Fatalf("fixture secret is %d chars, want exactly 64 — the length the redaction pattern keys on", len(secret))
	}
	return &ledger.Entry{Token: token, CashSecret: secret}, token, secret
}

func TestAuditD_CLI_RecoveryHandoffKeepsItsSecret_Human(t *testing.T) {
	e, token, secret := cashRecoveryFixture(t)
	cmd := testCmdWithFlags(false, false)

	err := reportUnsavedResult(cmd, errors.New("disk full"), "Consolidate", entryRecoveryHint(e))
	_, stderr := captureStreams(t, func() { output.EmitError(cmd, err) })

	if strings.Contains(stderr, "<redacted>") {
		t.Errorf("the recovery handoff was redacted — this message exists to hand over the secret:\n%s", stderr)
	}
	if !strings.Contains(stderr, secret) {
		t.Errorf("the cash secret is absent from the recovery message; it is the only thing that can spend the bill:\n%s", stderr)
	}
	if !strings.Contains(stderr, token+"#"+secret) {
		t.Errorf("the secret is not paired with its token, so there is no way to tell which bill it opens:\n%s", stderr)
	}
}

func TestAuditD_CLI_RecoveryHandoffKeepsItsSecret_JSON(t *testing.T) {
	e, token, secret := cashRecoveryFixture(t)
	cmd := testCmdWithFlags(true, false)

	err := reportUnsavedResult(cmd, errors.New("disk full"), "Consolidate", entryRecoveryHint(e))
	_, stderr := captureStreams(t, func() { output.EmitError(cmd, err) })

	// Parsed rather than substring-matched: --json is a contract, and a handoff
	// that only survived in some unparseable scrap of text would not be usable
	// by the agent this mode exists for.
	var payload map[string]any
	if jsonErr := json.Unmarshal([]byte(stderr), &payload); jsonErr != nil {
		t.Fatalf("EmitError's --json output didn't parse: %v\noutput: %s", jsonErr, stderr)
	}
	if !strings.Contains(stderr, token+"#"+secret) {
		t.Errorf("the token#secret handoff is absent from the --json failure shape:\n%s", stderr)
	}
	// It must be reachable as a value, not merely present somewhere in the bytes.
	var found bool
	for _, v := range payload {
		if s, ok := v.(string); ok && strings.Contains(s, token+"#"+secret) {
			found = true
		}
	}
	if !found {
		t.Errorf("no --json field carries the handoff as its value; an agent has nothing to read:\n%s", stderr)
	}
}

// TestAuditD_CLI_RecoveryHandoffIsNeverTruncated guards the other way a handoff
// dies: a length cap. A truncated token#secret is destroyed money exactly as a
// redacted one is, so the recovery channel must never be bounded, however
// reasonable a cap looks on ordinary error text.
func TestAuditD_CLI_RecoveryHandoffIsNeverTruncated(t *testing.T) {
	_, token, secret := cashRecoveryFixture(t)
	cmd := testCmdWithFlags(false, false)

	// Two handoffs in one message, which is what transfer does (the recipient's
	// token plus the sender's own remainder) and the longest this text gets.
	hint := "Save these: the recipient's own: " + token + "#" + secret +
		"; your own remainder: " + token + "#" + secret
	err := reportUnsavedResult(cmd, errors.New("disk full"), "Transfer", hint)
	_, stderr := captureStreams(t, func() { output.EmitError(cmd, err) })

	if got := strings.Count(stderr, token+"#"+secret); got != 2 {
		t.Errorf("found %d intact handoffs, want 2 — a cap or a redaction ate one:\n%s", got, stderr)
	}
	for _, marker := range []string{"…", "...", "truncated"} {
		if strings.Contains(stderr, marker) {
			t.Errorf("recovery text shows a truncation marker %q; a partial handoff is unusable:\n%s", marker, stderr)
		}
	}
}
