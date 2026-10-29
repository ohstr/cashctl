package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nipcash"

	"github.com/ohstr/cashctl/internal/testmint"
)

func installBillNetwork(t *testing.T, network string, err error) {
	t.Helper()
	prev := billNetworkFn
	billNetworkFn = func(context.Context, string) (string, error) { return network, err }
	t.Cleanup(func() { billNetworkFn = prev })
}

// NIP-CASH §Network and Chain: a client tells its user when network is not
// mainnet; a missing or unknown one reads as a test network (XQ-37).
func TestTestNetwork_EveryNetworkButMainnetIsReported(t *testing.T) {
	for network, want := range map[string]bool{"mainnet": false, "testnet": true, "signet": true, "regtest": true, "": true, "Mainnet": true, "mainnet ": true} {
		var got bool
		_, stderr := captureStreams(t, func() { got = testNetwork(false, network) })
		if got != want {
			t.Errorf("testNetwork(%q) = %v, want %v", network, got, want)
		}
		if said := strings.Contains(stderr, "network:"); said != want {
			t.Errorf("testNetwork(%q) said %q, want a line: %v", network, stderr, want)
		}
	}
}

func TestNoteBillNetwork_SaysWhenTheBillsHubIsNotOnMainnet(t *testing.T) {
	cases := []struct {
		network  string
		err      error
		wantNet  string
		wantTest bool
		line     string
	}{
		{"mainnet", nil, "mainnet", false, ""},
		{"regtest", nil, "regtest", true, "network: regtest — a test network"},
		{"signet", nil, "signet", true, "network: signet — a test network"},
		{"", nil, "", true, "network: unknown"},
		{"", errors.New("no answer"), "", true, "network: unknown"},
	}
	for _, c := range cases {
		installBillNetwork(t, c.network, c.err)
		var net string
		var test bool
		_, stderr := captureStreams(t, func() { net, test = noteBillNetwork(false, "lokicash1x") })
		if net != c.wantNet || test != c.wantTest {
			t.Errorf("%q/%v: got %q %v, want %q %v", c.network, c.err, net, test, c.wantNet, c.wantTest)
		}
		if c.line == "" && strings.Contains(stderr, "network:") || c.line != "" && !strings.Contains(stderr, c.line) {
			t.Errorf("%q/%v: stderr %q, want %q", c.network, c.err, stderr, c.line)
		}
	}
}

// The network read is gated like every dial: a copy whose provenance fails
// is never sent to its relays.
func TestBillNetwork_RefusesAFailingCopyBeforeDialling(t *testing.T) {
	relay, dialled := countingRelay(t)
	bill := testmint.Bill(t, nipcash.Token{RelayURLs: []string{relay}}, "")
	// A copy whose signature recovers no key at all.
	tok, _ := nipcash.Decode(testmint.Bill(t, nipcash.Token{RelayURLs: []string{relay}}, ""))
	tok.MintSignature = append([]byte(nil), tok.MintSignature...)
	tok.MintSignature[0] = 4
	copyOnRelay, err := nipcash.Encode(tok)
	if err != nil {
		t.Fatal(err)
	}
	real := billNetworkFnReal
	if _, err := real(context.Background(), copyOnRelay); !errors.Is(err, errProvenanceFailed) {
		t.Errorf("tampered copy: %v, want errProvenanceFailed", err)
	}
	if n := dialled.Load(); n != 0 {
		t.Errorf("the tampered copy's relay was contacted %d time(s)", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2e9)
	defer cancel()
	_, _ = real(ctx, bill)
	if dialled.Load() == 0 {
		t.Error("a bill that passes its provenance check was never asked for its network")
	}
}

func TestReceiveZap_ShowsTheBillsNetwork(t *testing.T) {
	for network, wantTest := range map[string]bool{"regtest": true, "mainnet": false} {
		f := newZapFixture(t)
		installZapHub(t, &fakeZapHub{res: f.mine(21000, false)})
		installBillNetwork(t, network, nil)
		stdout, err := runReceiveZap(t, []string{f.raw}, map[string]string{"trust-minter": "true"})
		if err != nil {
			t.Fatalf("%s: %v", network, err)
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
			t.Fatalf("%s: %q", network, stdout)
		}
		if doc["network"] != network || doc["test_network"] != wantTest {
			t.Errorf("%s: network %v test_network %v", network, doc["network"], doc["test_network"])
		}
	}
}
