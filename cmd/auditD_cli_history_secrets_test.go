package cmd

// Audit D, CLI surface, finding 5 (D-CLI-5) — what the action log keeps.
//
// `cashctl transfer <amount> nconnection1<...>` used to write that connection
// string into the local action log, where `wallet history` reprints it for the
// life of the ledger. The chain was: toValue is the raw --to value, targetClause
// interpolates it raw for a connection target, and that string is the detail of
// AppendHistory("transfer", ...).
//
// An nconnection1... carries its own dialing secret as a single TLV-packed blob
// with no substring that is safe to reveal — which is exactly why
// RedactSecretInput strips those wholesale. The user typing it is not new
// exposure; persisting it and reprinting it later is.
//
// The fix is narrow on purpose: the live confirm prompt and success line still
// show the value as typed, because showing someone what they just typed is what
// makes a confirmation mean anything. Only the durable record changes.
//
// RED-FIRST NOTE, stated honestly: the detail was composed inline at the
// AppendHistory call site, so there was no seam to assert against before the fix
// existed. Red-first here was established by MUTATION instead — reverting
// historyTargetClause to targetClause fails these tests — and by reading the
// pre-fix call site directly. That is weaker than compiling a test against the
// unfixed tree, which is why it is written down rather than implied.

import (
	"strings"
	"testing"

	"github.com/ohstr/cashctl/internal/credential"
	"github.com/ohstr/cashctl/internal/output"
)

const (
	testNconnection = "nconnection1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq"
	testPubkeyHex   = "abababababababababababababababababababababababababababababababab"
)

func TestAuditD_CLI_HistoryNeverKeepsAnNconnectionString(t *testing.T) {
	// The ordinary path: an nconnection resolved through an Identity Authority,
	// where Resolved holds the public platform + IA description.
	resolved := credential.ResolvedTarget{
		Kind:     credential.TargetKindConnection,
		Input:    testNconnection,
		Resolved: "web connection; Identity Authority: " + testPubkeyHex,
	}
	got := historyTargetClause(testNconnection, resolved)
	if strings.Contains(got, testNconnection) {
		t.Errorf("history detail keeps the raw nconnection string: %q", got)
	}
	if !strings.Contains(got, "Identity Authority") {
		t.Errorf("history detail dropped the public description that makes the record useful: %q", got)
	}

	// The unresolved path (no IA was needed or recorded): the value must still
	// not be stored verbatim.
	unresolved := credential.ResolvedTarget{Kind: credential.TargetKindConnection, Input: testNconnection}
	got = historyTargetClause(testNconnection, unresolved)
	if strings.Contains(got, testNconnection) {
		t.Errorf("unresolved connection target kept verbatim in history: %q", got)
	}
	if !strings.Contains(got, "nconnection1") {
		t.Errorf("history detail should still say WHAT kind of target it was: %q", got)
	}
}

// TestAuditD_CLI_HistoryKeepsPublicTargetsIntact is the other half, and the more
// likely way to get this wrong: over-redacting. RedactSecretInput blanks a bare
// 64-hex string entirely, and a pubkey target IS bare 64-hex, so applying
// redaction to every kind would erase a perfectly public destination from the
// record. The explicit connection:<platform>:<external-id>:<ia-pubkey> form
// carries no secret either and must survive as typed.
func TestAuditD_CLI_HistoryKeepsPublicTargetsIntact(t *testing.T) {
	cases := []struct {
		name   string
		to     string
		target credential.ResolvedTarget
		want   string
	}{
		{"bare pubkey", testPubkeyHex, credential.ResolvedTarget{Kind: credential.TargetKindPubkey}, testPubkeyHex},
		{"nip-05", "alice@example.com", credential.ResolvedTarget{Kind: credential.TargetKindPubkey}, "alice@example.com"},
		{
			"explicit connection form",
			"connection:web:alice:" + testPubkeyHex,
			credential.ResolvedTarget{Kind: credential.TargetKindConnection},
			"connection:web:alice:",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := historyTargetClause(tc.to, tc.target)
			if !strings.Contains(got, tc.want) {
				t.Errorf("history detail = %q, want it to still contain the public target %q", got, tc.want)
			}
		})
	}
}

// TestAuditD_CLI_HistoryCashTargetInterpolatesNothing guards the trap in the
// other direction: ResolvedTarget.Resolved for a CASH target is
// "Generated a cash secret: <secret>", so a blanket "just use Resolved" would
// write a spending secret into the action log.
func TestAuditD_CLI_HistoryCashTargetInterpolatesNothing(t *testing.T) {
	secret := strings.Repeat("a1b2c3d4", 8)
	cash := credential.ResolvedTarget{
		Kind:     credential.TargetKindCash,
		Input:    "cash",
		Resolved: "Generated a cash secret: " + secret,
	}
	got := historyTargetClause("cash", cash)
	if strings.Contains(got, secret) {
		t.Errorf("a cash target wrote its spending secret into the history detail: %q", got)
	}
}

// TestAuditD_CLI_HistoryDetailsCarryNothingSecretShaped is the reflective half.
// Rather than naming fields, it runs each composed detail back through
// RedactSecretInput — already exported — and requires it to come back unchanged.
// "Unchanged" means none of the compiled secret patterns matched, so this reuses
// the single policy instead of growing a second detector that could drift from
// it, and it keeps working for a target shape added in a future release.
func TestAuditD_CLI_HistoryDetailsCarryNothingSecretShaped(t *testing.T) {
	corpus := []struct {
		to     string
		target credential.ResolvedTarget
	}{
		{testNconnection, credential.ResolvedTarget{Kind: credential.TargetKindConnection, Resolved: "web connection; Identity Authority: " + testPubkeyHex}},
		{testNconnection, credential.ResolvedTarget{Kind: credential.TargetKindConnection}},
		{"connection:web:alice:" + testPubkeyHex, credential.ResolvedTarget{Kind: credential.TargetKindConnection}},
		{testPubkeyHex, credential.ResolvedTarget{Kind: credential.TargetKindPubkey}},
		{"alice@example.com", credential.ResolvedTarget{Kind: credential.TargetKindPubkey}},
		{"cash", credential.ResolvedTarget{Kind: credential.TargetKindCash, Resolved: "Generated a cash secret: " + strings.Repeat("ff", 32)}},
	}
	for _, c := range corpus {
		detail := "transferred 5 loki " + historyTargetClause(c.to, c.target)
		if redacted := output.RedactSecretInput(detail); redacted != detail {
			t.Errorf("history detail is secret-shaped.\n  detail:   %q\n  redacted: %q", detail, redacted)
		}
	}
}

func TestAuditD_CLI_AsCredentialFallsBackToTheEnvironment(t *testing.T) {
	cmd := testCmdWithFlags(false, false)
	cmd.Flags().String("as", "", "")

	if got := asCredentialValue(cmd); got != "" {
		t.Errorf("with neither flag nor env set, got %q, want empty", got)
	}

	t.Setenv(asCredentialEnv, "cash:fromenv")
	if got := asCredentialValue(cmd); got != "cash:fromenv" {
		t.Errorf("with only the env var set, got %q, want the env value", got)
	}

	// The flag wins: an explicit per-invocation value must never be silently
	// overridden by something left in the environment.
	if err := cmd.Flags().Set("as", "cash:fromflag"); err != nil {
		t.Fatal(err)
	}
	if got := asCredentialValue(cmd); got != "cash:fromflag" {
		t.Errorf("with both set, got %q, want the flag to win", got)
	}
}
