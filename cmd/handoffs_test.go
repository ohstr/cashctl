package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nipcash"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/ledger"

	"github.com/ohstr/cashctl/internal/testmint"
)

func handoffToken(t *testing.T) string {
	t.Helper()
	tok, err := testmint.Encode(t, nipcash.Token{HRP: "lokicash", WalletPubkey: strings.Repeat("ab", 32), Secret: strings.Repeat("cd", 32), RelayURLs: []string{"wss://relay.example"}})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// I-27: a cash note and a gift are recorded with the bill's own pubkey.
func TestRecordHandoff_KindsAndPubkey(t *testing.T) {
	tok := handoffToken(t)
	l := &ledger.Ledger{}
	recordHandoff(l, tok+"#"+strings.Repeat("ee", 32), tok, 5000, "cash")
	recordHandoff(l, "", tok, 7000, "npub1bob")
	recordHandoff(l, "", "", 1, "nobody")
	if len(l.Handoffs) != 2 {
		t.Fatalf("handoffs = %d, want 2 (nothing to hand over records nothing)", len(l.Handoffs))
	}
	note, gift := l.Handoffs[0], l.Handoffs[1]
	if note.Kind != ledger.HandoffCashNote || note.Status != ledger.HandoffUnclaimed || note.WalletPubkey != strings.Repeat("ab", 32) {
		t.Errorf("cash note recorded as %+v", note)
	}
	if gift.Kind != ledger.HandoffGift || gift.Status != ledger.HandoffSent || gift.Handoff != tok {
		t.Errorf("gift recorded as %+v", gift)
	}
}

// An unclaimed note's secret is hidden unless asked for; a claimed one is shown.
func TestWalletHistory_MasksUnclaimedSecretsUnlessAsked(t *testing.T) {
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	orig := checkHandoffFn
	checkHandoffFn = func(string) string { return "" } // the Hub gave no answer: nothing changes
	t.Cleanup(func() { checkHandoffFn = orig })

	tok := handoffToken(t)
	secret := strings.Repeat("ee", 32)
	l, _ := ledger.Load()
	recordHandoff(l, tok+"#"+secret, tok, 5000, "cash")
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	run := func(show bool) map[string]any {
		cmd := newWalletHistoryCmd()
		cmd.Flags().Bool("json", true, "")
		args := []string{}
		if show {
			args = append(args, "--show-secrets")
		}
		cmd.SetArgs(args)
		var doc map[string]any
		printed := withCapturedOutput(func() {
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
		})
		if err := json.Unmarshal([]byte(printed), &doc); err != nil {
			t.Fatalf("history --json is not JSON: %v\n%s", err, printed)
		}
		return doc
	}
	masked, _ := json.Marshal(run(false))
	if strings.Contains(string(masked), secret) {
		t.Errorf("an unclaimed note's secret was printed without --show-secrets: %s", masked)
	}
	shown, _ := json.Marshal(run(true))
	if !strings.Contains(string(shown), secret) {
		t.Errorf("--show-secrets did not show the note in full: %s", shown)
	}
}

// A refresh that learns the note was taken records that, and stops masking it.
func TestRefreshHandoffs_RecordsAClaim(t *testing.T) {
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	orig := checkHandoffFn
	checkHandoffFn = func(string) string { return ledger.HandoffClaimed }
	t.Cleanup(func() { checkHandoffFn = orig })
	tok := handoffToken(t)
	l, _ := ledger.Load()
	recordHandoff(l, tok+"#"+strings.Repeat("ee", 32), tok, 5000, "cash")
	_ = l.Save()
	refreshHandoffs(l)
	fresh, _ := ledger.Load()
	if fresh.Handoffs[0].Status != ledger.HandoffClaimed || fresh.Handoffs[0].CheckedAt == "" {
		t.Errorf("claim not saved: %+v", fresh.Handoffs[0])
	}
	if got := unclaimedHandoffs(fresh); len(got) != 0 {
		t.Errorf("a claimed note is still listed as unclaimed: %v", got)
	}
}
