package cmd

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	relayclient "github.com/ohstr/nmilat/relay/client"

	"github.com/ohstr/nmilat/nipcw"
)

// validCircleHub returns a real, checksum-valid circlehub1... string (the
// hand-typed "circlehub1qqs2u2jj" fixture used elsewhere in this file has
// an invalid bech32 checksum, so dial.Sniff would classify it as
// KindUnknown — fine for the amount-vs-non-amount cases those tests
// cover, but not for looksLikeHubArg's own shape check below, which needs
// a genuinely valid one).
func validCircleHub(t *testing.T) string {
	t.Helper()
	s, err := nipcw.EncodeCircleHubConnection(nipcw.CircleHubConnection{
		WalletPubkey: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Secret:       "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	})
	if err != nil {
		t.Fatalf("EncodeCircleHubConnection: %v", err)
	}
	return s
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

// TestIsIdentityEventReplayErr covers the exact match this fix depends on
// — see its own doc comment for why it keys on message text, not just the
// BAD_REQUEST code (which is used for plenty of other join declines too,
// e.g. an over-cap max-amount).
func TestIsIdentityEventReplayErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"exact match", &relayclient.WalletError{Code: "BAD_REQUEST", Message: "identity_event has already been used"}, true},
		{"wrapped", fmt.Errorf("join: %w", &relayclient.WalletError{Code: "BAD_REQUEST", Message: "identity_event has already been used"}), true},
		{"different BAD_REQUEST reason", &relayclient.WalletError{Code: "BAD_REQUEST", Message: "max_amount exceeds per_wallet_max_mloki"}, false},
		{"same message, different code", &relayclient.WalletError{Code: "RESTRICTED", Message: "identity_event has already been used"}, false},
		{"not a WalletError at all", errors.New("identity_event has already been used"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isIdentityEventReplayErr(tt.err); got != tt.want {
				t.Errorf("isIdentityEventReplayErr(%v) = %v, want %v", tt.err, got, tt.want)
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
