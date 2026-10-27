package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/ohstr/nmilat/nipcw"

	"github.com/ohstr/cashctl/internal/output"
	"github.com/ohstr/cashctl/internal/testmint"
)

// validCircleHub returns a real, checksum-valid circlehub1... string (the
// hand-typed "circlehub1qqs2u2jj" fixture used elsewhere in this file has
// an invalid bech32 checksum, so dial.Sniff would classify it as
// KindUnknown — fine for the amount-vs-non-amount cases those tests
// cover, but not for looksLikeHubArg's own shape check below, which needs
// a genuinely valid one).
func validCircleHub(t *testing.T) string {
	t.Helper()
	hubPriv, _ := testmint.Key(t)
	return testmint.CircleHub(t, hubPriv, "")
}

// --- disambiguateJoinArgs: the positional-args shape-sniffing that lets
// `cashctl join <hub> <amount>` and `cashctl join <amount> <hub>` both
// resolve the same way, mirroring disambiguateTransferArgs's own coverage
// in cash_transfer_test.go for the identical reasoning.

func TestDisambiguateJoinArgs_NoArgs(t *testing.T) {
	hub, amount := disambiguateJoinArgs(nil)
	if hub != "" || amount != "" {
		t.Errorf("disambiguateJoinArgs(nil) = (%q, %q), want (\"\", \"\")", hub, amount)
	}
}

func TestDisambiguateJoinArgs_SingleNonNumericArgIsHub(t *testing.T) {
	for _, arg := range []string{
		"circlehub1qqs2u2jj",
		"nostr+walletconnect://a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1",
	} {
		hub, amount := disambiguateJoinArgs([]string{arg})
		if hub != arg {
			t.Errorf("disambiguateJoinArgs([%q]) hub = %q, want %q", arg, hub, arg)
		}
		if amount != "" {
			t.Errorf("disambiguateJoinArgs([%q]) amount = %q, want empty", arg, amount)
		}
	}
}

func TestDisambiguateJoinArgs_SingleNumericArgIsMaxAmount(t *testing.T) {
	// Not a useful invocation on its own (join always needs a hub), but
	// the shape-sniffing itself must still classify a bare number as an
	// amount, never mistake it for a hub connection — runCircleJoin's own
	// "a Circle Hub connection is required" check is what actually
	// rejects this case.
	hub, amount := disambiguateJoinArgs([]string{"100"})
	if hub != "" {
		t.Errorf("disambiguateJoinArgs([\"100\"]) hub = %q, want empty", hub)
	}
	if amount != "100" {
		t.Errorf("disambiguateJoinArgs([\"100\"]) amount = %q, want \"100\"", amount)
	}
}

func TestDisambiguateJoinArgs_TwoArgsHubThenAmount(t *testing.T) {
	hub, amount := disambiguateJoinArgs([]string{"circlehub1qqs2u2jj", "100"})
	if hub != "circlehub1qqs2u2jj" || amount != "100" {
		t.Errorf("disambiguateJoinArgs(2 args) = (%q, %q), want (\"circlehub1qqs2u2jj\", \"100\")", hub, amount)
	}
}

func TestDisambiguateJoinArgs_TwoArgsAmountThenHubAlsoWorks(t *testing.T) {
	hub, amount := disambiguateJoinArgs([]string{"100", "circlehub1qqs2u2jj"})
	if hub != "circlehub1qqs2u2jj" || amount != "100" {
		t.Errorf("disambiguateJoinArgs(2 args, amount-first) = (%q, %q), want (\"circlehub1qqs2u2jj\", \"100\")", hub, amount)
	}
}

func TestDisambiguateJoinArgs_FractionalAmountRecognized(t *testing.T) {
	hub, amount := disambiguateJoinArgs([]string{"circlehub1qqs2u2jj", "0.5"})
	if hub != "circlehub1qqs2u2jj" || amount != "0.5" {
		t.Errorf("disambiguateJoinArgs(fractional) = (%q, %q), want (\"circlehub1qqs2u2jj\", \"0.5\")", hub, amount)
	}
}

// TestDisambiguateJoinArgs_MalformedAmountBlamesTheAmountNotTheHub is the
// regression guard for the bug found live: `join 0.0001 <hub>` used to
// blame the 232-char hub connection string as "not a valid amount" —
// neither "0.0001"-shaped typos nor the hub string parse as an amount, so
// the old position-only fallback (args[0] fails ParseAmount -> assume
// args[0] is the hub) always picked wrong when the malformed amount
// landed in arg[0].
func TestDisambiguateJoinArgs_MalformedAmountBlamesTheAmountNotTheHub(t *testing.T) {
	hub := validCircleHub(t)
	for _, badAmount := range []string{"0.0001", "1e3", "5x"} {
		t.Run("amount_first="+badAmount, func(t *testing.T) {
			gotHub, gotAmount := disambiguateJoinArgs([]string{badAmount, hub})
			if gotHub != hub {
				t.Errorf("hub = %q, want the real hub connection, not the malformed amount", gotHub)
			}
			if gotAmount != badAmount {
				t.Errorf("amount = %q, want %q (the actually-malformed one)", gotAmount, badAmount)
			}
		})
		t.Run("hub_first="+badAmount, func(t *testing.T) {
			gotHub, gotAmount := disambiguateJoinArgs([]string{hub, badAmount})
			if gotHub != hub {
				t.Errorf("hub = %q, want the real hub connection, not the malformed amount", gotHub)
			}
			if gotAmount != badAmount {
				t.Errorf("amount = %q, want %q (the actually-malformed one)", gotAmount, badAmount)
			}
		})
	}
}

// TestRootExample_JoinCarriesMaxAmount guards the root command's own
// Example against the shape runCircleJoin actually enforces: a cap is
// mandatory (NIP-CW has no "0 means unlimited" convention), so a `join`
// example without one is a copy-pasteable call that always exits 2.
// Asserted structurally rather than against a fixed string, so rewording
// the placeholders doesn't spuriously fail.
func TestRootExample_JoinCarriesMaxAmount(t *testing.T) {
	for _, line := range strings.Split(RootCmd.Example, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "cashctl" || fields[1] != "join" {
			continue
		}
		if got := len(fields) - 2; got < 2 {
			t.Errorf("root Example %q passes %d argument(s) to join, want 2 (hub connection and max amount)", line, got)
		}
		return
	}
	t.Skip("root Example no longer shows a join line — nothing to guard")
}

// newTestJoinCmd carries the flags runCircleJoin reads directly off cmd —
// the same set newCircleJoinCmd registers, plus the root's globals.
func newTestJoinCmd() *cobra.Command {
	c := &cobra.Command{}
	c.Flags().Bool("json", false, "")
	c.Flags().Bool("yes", false, "")
	c.Flags().String("hub", "", "")
	c.Flags().String("max-amount", "", "")
	c.Flags().Duration("expiry", 0, "")
	c.Flags().String("budget-renewal", "", "")
	c.Flags().String("as", "", "")
	c.Flags().String("max-receive", "", "")
	c.Flags().Bool("allow-unverified", false, "")
	c.Flags().String("backup-file", "", "")
	return c
}

// TestRunCircleJoin_ZeroCapIsNotTreatedAsMissing pins that an explicitly
// supplied 0 and an omitted amount are different mistakes. They used to
// share one message, so a user who had just typed `0` was told they were
// missing the argument they had typed. Someone passing 0 has almost
// certainly assumed a "0 means unlimited" convention NIP-CW does not have,
// which is what the message must say.
func TestRunCircleJoin_ZeroCapIsNotTreatedAsMissing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		flags   map[string]string
		wantSub string
	}{
		{"positional zero", []string{"circlehub1qqq", "0"}, nil, "0 isn't a spend cap"},
		{"flag zero", []string{"circlehub1qqq"}, map[string]string{"max-amount": "0"}, "0 isn't a spend cap"},
		{"omitted", []string{"circlehub1qqq"}, nil, "a max amount is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestJoinCmd()
			for k, v := range tc.flags {
				if err := c.Flags().Set(k, v); err != nil {
					t.Fatal(err)
				}
			}
			err := runCircleJoin(c, tc.args)
			if err == nil {
				t.Fatal("runCircleJoin() = nil error, want a refusal")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// A bare `join` names both required arguments, so following the hint
// does not fail again on the missing max amount.
func TestRunCircleJoin_MissingHubHintNamesEveryMissingArgument(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		want   []string
		absent string
	}{
		{"bare join", nil, []string{"<hub-connection> <max-amount>", "--hub", "--max-amount"}, ""},
		{"amount only", []string{"100"}, []string{"<hub-connection>", "--hub"}, "--max-amount"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runCircleJoin(newTestJoinCmd(), tc.args)
			if output.AsCLIError(err).Code != output.CodeUsage {
				t.Fatalf("err = %v, want a usage error", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("hint %q does not name %q", err, w)
				}
			}
			if tc.absent != "" && strings.Contains(err.Error(), tc.absent) {
				t.Errorf("hint %q asks for %q, which was given", err, tc.absent)
			}
		})
	}
}

// join reports the cap the Hub resolved, saying when it differs from the
// one asked for, and the in-circle fee (NIP-CW §Response).
func TestJoinCapSummary_ShowsTheResolvedCapAndFee(t *testing.T) {
	got := joinCapSummary(500_000, &nipcw.CreateCircleWalletResponse{MaxAmountMillis: 400_000, FeesPpm: 1000}, "never")
	for _, want := range []string{output.FormatAmount(400_000), "you asked for " + output.FormatAmount(500_000), "1000 ppm"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q lacks %q", got, want)
		}
	}
	same := joinCapSummary(500_000, &nipcw.CreateCircleWalletResponse{MaxAmountMillis: 500_000}, "never")
	if strings.Contains(same, "asked for") || strings.Contains(same, "ppm") {
		t.Errorf("summary %q mentions a difference or fee that is not there", same)
	}
	if unknown := joinCapSummary(500_000, &nipcw.CreateCircleWalletResponse{}, "never"); !strings.Contains(unknown, output.FormatAmount(500_000)) {
		t.Errorf("with no resolved cap the requested one should show: %q", unknown)
	}
	if exp := joinCapSummary(500_000, &nipcw.CreateCircleWalletResponse{ExpiresAt: 1798761600}, "never"); !strings.Contains(exp, "expires 2027-01-01 00:00 UTC") {
		t.Errorf("summary %q lacks the expiry the Hub set", exp)
	}
	if strings.Contains(same, "expires") {
		t.Errorf("summary %q names an expiry the Hub did not set", same)
	}
}
