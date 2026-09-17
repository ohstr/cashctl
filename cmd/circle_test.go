package cmd

import "testing"

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
