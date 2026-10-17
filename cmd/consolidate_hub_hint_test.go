package cmd

import (
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nipcash"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

func tokenOnHub(t *testing.T, wallet, hubGroup string) string {
	t.Helper()
	tok, err := nipcash.Encode(nipcash.Token{
		HRP: "lokicash", WalletPubkey: wallet, Secret: strings.Repeat("bb", 32),
		RelayURLs: []string{"wss://relay.example"}, HubGroup: []byte(hubGroup),
	})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// The Hub's "same Cash Hub" refusal of named sources says which Hub each one is on.
func TestWithSourceHubs_NamesEachSourcesHub(t *testing.T) {
	l := &ledger.Ledger{Entries: []ledger.Entry{
		{ID: "a", Status: ledger.StatusHeld, Token: tokenOnHub(t, strings.Repeat("a", 64), "hub1")},
		{ID: "b", Status: ledger.StatusHeld, Token: tokenOnHub(t, strings.Repeat("c", 64), "hub2")},
	}}
	refusal := output.NWCErrorForCashToken(&cobra.Command{}, &relayclient.WalletError{
		Code: "BAD_REQUEST", Message: "all sources must belong to the same Cash Hub",
	})
	if !isCrossHubDeclineClassified(refusal) {
		t.Fatal("test refusal is not recognised as cross-Hub")
	}
	err := withSourceHubs(refusal, l, []string{"a", "b"})
	msg := output.AsCLIError(err).Hint
	for _, want := range []string{"a on hub " + shortHub(ledger.HubGroupKey(l.Entries[0])), "b on hub " + shortHub(ledger.HubGroupKey(l.Entries[1]))} {
		if !strings.Contains(msg, want) {
			t.Errorf("%q does not say %q", msg, want)
		}
	}
	if output.AsCLIError(err).Code != output.AsCLIError(refusal).Code {
		t.Error("the refusal's code changed")
	}
}

// A bare consolidate names the tokens it leaves alone — each the only one from
// its Cash Hub — instead of skipping them without a word: in --json by id, in
// text by count and amount.
func TestAloneOnTheirHub_IsReported(t *testing.T) {
	amt := uint64(20_000)
	held := []ledger.Entry{
		{ID: "a", Status: ledger.StatusHeld, AmountMillis: &amt, Token: tokenOnHub(t, strings.Repeat("a", 64), "hub1")},
		{ID: "b", Status: ledger.StatusHeld, AmountMillis: &amt, Token: tokenOnHub(t, strings.Repeat("b", 64), "hub1")},
		{ID: "c", Status: ledger.StatusHeld, AmountMillis: &amt, Token: tokenOnHub(t, strings.Repeat("c", 64), "hub2")},
	}
	alone := aloneOnTheirHub(held)
	if len(alone) != 1 || alone[0].ID != "c" {
		t.Fatalf("alone = %+v, want only c", alone)
	}
	doc := withAlone(map[string]any{"consolidated": []any{}}, alone)
	rows, _ := doc["skipped"].([]map[string]any)
	if len(rows) != 1 || rows[0]["id"] != "c" {
		t.Errorf("--json skipped = %v, want c", doc["skipped"])
	}
	text, _ := captureStreams(t, func() { printAlone(alone) })
	if !strings.Contains(text, "Left as is: 1 token(s)") || strings.Contains(text, `"c"`) {
		t.Errorf("text = %q, want a count and amount, no id", text)
	}
}
