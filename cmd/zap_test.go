package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nipcash"
	"github.com/ohstr/nmilat/utils"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/identity"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
	"github.com/ohstr/cashctl/internal/testmint"
)

// zapFixture is a local identity, a minter and a Cash Zap to that identity.
type zapFixture struct {
	me, minterPriv, minterX string
	ev                      *nip01.Event
	bill                    string
	raw, target             string
}

func newZapFixture(t *testing.T) *zapFixture {
	t.Helper()
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	if _, err := identity.GenerateAndSaveLocal(); err != nil {
		t.Fatal(err)
	}
	priv, err := identity.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	me, _ := utils.GetPublicKey(priv)
	minterPriv, minterX := testmint.Key(t)
	senderPriv, _ := testmint.Key(t)
	note, err := nip01.NewSignedEvent(1, "a note", priv)
	if err != nil {
		t.Fatal(err)
	}
	ev, bill := testmint.CashZap(t, minterPriv, senderPriv, note, 21000)
	raw, _ := json.Marshal(ev)
	nb, _ := json.Marshal(note)
	target := filepath.Join(t.TempDir(), "note.json")
	_ = os.WriteFile(target, nb, 0o600)
	return &zapFixture{me: me, minterPriv: minterPriv, minterX: minterX, ev: ev, bill: bill, raw: string(raw), target: target}
}

// fakeZapHub answers cash_status mine for the zap bill.
type fakeZapHub struct {
	res   *nipcash.CashStatusResult
	err   error
	dials int
}

func (f *fakeZapHub) CashStatus(context.Context, nipcash.Credential, string) (*nipcash.CashStatusResult, error) {
	return f.res, f.err
}
func (f *fakeZapHub) Close() {}

func installZapHub(t *testing.T, h *fakeZapHub) {
	t.Helper()
	prev := dialZapFn
	dialZapFn = func(context.Context, string, nipcash.ZapContext) (zapStatusClient, error) {
		h.dials++
		return h, nil
	}
	t.Cleanup(func() { dialZapFn = prev })
}

func (f *zapFixture) mine(amount uint64, claimed bool) *nipcash.CashStatusResult {
	return &nipcash.CashStatusResult{Recipients: []nipcash.RecipientStatus{{IdentityType: "pubkey", IdentityValue: f.me, AmountMillis: amount, Claimed: claimed}}}
}

func zapCmd(c *cobra.Command, flags map[string]string) *cobra.Command {
	c.Flags().Bool("json", true, "")
	c.Flags().Bool("yes", false, "")
	c.Flags().String("connection", "", "")
	for k, v := range flags {
		_ = c.Flags().Set(k, v)
	}
	return c
}

func runDecodeZap(t *testing.T, raw string, flags map[string]string) map[string]any {
	t.Helper()
	c := zapCmd(newDecodeCmd(), flags)
	c.SetArgs([]string{raw})
	var err error
	stdout, _ := captureStreams(t, func() { err = c.Execute() })
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("%q: %v", stdout, err)
	}
	return doc
}

// decode runs rules 1-8 in order, counts only a zap from a trusted minter, and
// never says received.
func TestDecodeZap_RulesAndCountable(t *testing.T) {
	f := newZapFixture(t)
	doc := runDecodeZap(t, f.raw, map[string]string{"target": f.target})
	rules, _ := doc["rules"].([]any)
	if len(rules) != 8 || rules[0].(map[string]any)["n"] != float64(1) {
		t.Fatalf("rules %v", rules)
	}
	if doc["countable"] != false || rules[7].(map[string]any)["ok"] != false {
		t.Errorf("an untrusted minter counted: %v", doc)
	}
	if _, ok := doc["received"]; ok {
		t.Error("decode said received")
	}
	if err := config.TrustMinter(config.TrustedMinter{MinterXOnly: f.minterX, Chain: "flokicoin"}); err != nil {
		t.Fatal(err)
	}
	if doc := runDecodeZap(t, f.raw, nil); doc["countable"] != false {
		t.Errorf("counted without its target event: %v", doc)
	}
	if doc := runDecodeZap(t, f.raw, map[string]string{"target": f.target}); doc["countable"] != true {
		t.Errorf("a trusted minter's zap not countable: %v", doc)
	}

	// Signature first: a changed event fails rule 1 and nothing else runs.
	var ev nip01.Event
	_ = json.Unmarshal([]byte(f.raw), &ev)
	ev.Content = "changed"
	b, _ := json.Marshal(ev)
	path := filepath.Join(t.TempDir(), "zap.json")
	_ = os.WriteFile(path, b, 0o600)
	doc = runDecodeZap(t, path, nil)
	r1 := doc["rules"].([]any)[0].(map[string]any)
	if r1["ok"] != false || doc["countable"] != false {
		t.Errorf("tampered zap: %v", doc)
	}
}

func runReceiveZap(t *testing.T, args []string, flags map[string]string) (string, error) {
	t.Helper()
	c := zapCmd(newCashReceiveCmd(), flags)
	var err error
	stdout, _ := captureStreams(t, func() { err = runCashReceive(c, args) })
	return stdout, err
}

// receive: an untrusted minter needs approval before any contact; with it,
// rule 9 (the Hub's own row for the local identity) decides, and the bill is
// kept with its zap event so it can be dialled later.
func TestReceiveZap_ApprovalThenRule9(t *testing.T) {
	f := newZapFixture(t)
	h := &fakeZapHub{res: f.mine(21000, false)}
	installZapHub(t, h)
	_, err := runReceiveZap(t, []string{f.raw}, nil)
	if ce := output.AsCLIError(err); ce == nil || output.ExitCode(err) != 2 || ce.Reason != reasonMinterUntrusted {
		t.Fatalf("untrusted: %v", err)
	}
	if h.dials != 0 {
		t.Fatal("the minter was contacted before approval")
	}
	stdout, err := runReceiveZap(t, []string{f.raw}, map[string]string{"trust-minter": "true"})
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if !strings.Contains(stdout, `"received": true`) && !strings.Contains(stdout, `"received":true`) {
		t.Errorf("stdout %s", stdout)
	}
	l, _ := ledger.Load()
	held := l.Held()
	if len(held) != 1 || held[0].ZapEvent == "" || held[0].AmountMillis == nil || *held[0].AmountMillis != 21000 {
		t.Fatalf("held %+v", held)
	}
	if zc, err := heldZapContext(held[0].Token); err != nil || zc.Recipient != f.me {
		t.Errorf("the held zap bill can't be dialled: %v", err)
	}
}

// Rule 9 failures: claimed, wrong amount, gone, no answer.
func TestReceiveZap_Rule9Failures(t *testing.T) {
	for name, tc := range map[string]struct {
		res  func(f *zapFixture) *nipcash.CashStatusResult
		err  error
		exit int
	}{
		"claimed": {res: func(f *zapFixture) *nipcash.CashStatusResult { return f.mine(21000, true) }, exit: 3},
		"shared":  {res: func(f *zapFixture) *nipcash.CashStatusResult { return f.mine(1000, false) }, exit: 3},
		"gone": {res: func(*zapFixture) *nipcash.CashStatusResult {
			return &nipcash.CashStatusResult{Tombstone: &nipcash.Tombstone{}}
		}, exit: 10},
		"no answer": {err: context.DeadlineExceeded, exit: 6},
	} {
		t.Run(name, func(t *testing.T) {
			f := newZapFixture(t)
			h := &fakeZapHub{err: tc.err}
			if tc.res != nil {
				h.res = tc.res(f)
			}
			installZapHub(t, h)
			_, err := runReceiveZap(t, []string{f.raw}, map[string]string{"trust-minter": "true"})
			if output.ExitCode(err) != tc.exit {
				t.Errorf("err %v, exit %d want %d", err, output.ExitCode(err), tc.exit)
			}
			if l, _ := ledger.Load(); len(l.Held()) != 0 {
				t.Error("a zap not proven received was kept")
			}
		})
	}
}

// A zap bill alone is refused with the way to give its event; --zap-event
// takes it; a zap for someone else is refused.
func TestReceiveZap_BillNeedsItsEvent(t *testing.T) {
	f := newZapFixture(t)
	installZapHub(t, &fakeZapHub{res: f.mine(21000, false)})
	_, err := runReceiveZap(t, []string{f.bill}, nil)
	if output.ExitCode(err) != 3 || !strings.Contains(err.Error(), "--zap-event") {
		t.Fatalf("bill alone: %v", err)
	}
	path := filepath.Join(t.TempDir(), "zap.json")
	_ = os.WriteFile(path, []byte(f.raw), 0o600)
	if _, err := runReceiveZap(t, []string{f.bill}, map[string]string{"zap-event": path, "trust-minter": "true"}); err != nil {
		t.Fatalf("with --zap-event: %v", err)
	}

	other, _ := testmint.Key(t)
	sender, _ := testmint.Key(t)
	otherNote, _ := nip01.NewSignedEvent(1, "theirs", other)
	ev, _ := testmint.CashZap(t, f.minterPriv, sender, otherNote, 1000)
	raw, _ := json.Marshal(ev)
	if _, err := runReceiveZap(t, []string{string(raw)}, map[string]string{"trust-minter": "true"}); output.ExitCode(err) != 3 {
		t.Errorf("someone else's zap: %v", err)
	}
}

// --find-zap looks only on --relay.
func TestReceiveZap_FindZapNeedsARelay(t *testing.T) {
	f := newZapFixture(t)
	installZapHub(t, &fakeZapHub{res: f.mine(21000, false)})
	if _, err := runReceiveZap(t, []string{f.bill}, map[string]string{"find-zap": "true"}); output.ExitCode(err) != 2 {
		t.Fatalf("no relay: %v", err)
	}
	prev := findZapEvents
	var asked []string
	findZapEvents = func(_ context.Context, relays []string, wallet string) ([]*nip01.Event, error) {
		asked = relays
		if wallet != tagValue(f.ev, "w") {
			return nil, errors.New("wrong #w")
		}
		return []*nip01.Event{f.ev}, nil
	}
	t.Cleanup(func() { findZapEvents = prev })
	if _, err := runReceiveZap(t, []string{f.bill}, map[string]string{"find-zap": "true", "relay": "wss://mine.example", "trust-minter": "true"}); err != nil {
		t.Fatalf("find-zap: %v", err)
	}
	if len(asked) != 1 || asked[0] != "wss://mine.example" {
		t.Errorf("asked %v", asked)
	}
}

func tagValue(ev *nip01.Event, name string) string {
	for _, tg := range ev.Tags {
		if len(tg) > 1 && tg[0] == name {
			return tg[1]
		}
	}
	return ""
}

// Z-3: --zap-event must name the bill given; Z-4: decode --target verifies the
// target's signature before rule 7 trusts it.
func TestZap_EventMustMatchBillAndTargetVerified(t *testing.T) {
	f := newZapFixture(t)
	installZapHub(t, &fakeZapHub{res: f.mine(21000, false)})
	other := newZapFixtureSameDir(t, f)
	path := filepath.Join(t.TempDir(), "zap.json")
	_ = os.WriteFile(path, []byte(other.raw), 0o600)
	if _, err := runReceiveZap(t, []string{f.bill}, map[string]string{"zap-event": path, "trust-minter": "true"}); output.ExitCode(err) != 3 {
		t.Fatalf("--zap-event of another bill: %v", err)
	}
	if l, _ := ledger.Load(); len(l.Held()) != 0 {
		t.Fatal("a bill was kept from a mismatched event")
	}

	var note nip01.Event
	b, _ := os.ReadFile(f.target)
	_ = json.Unmarshal(b, &note)
	note.Content = "forged"
	forged, _ := json.Marshal(note)
	fp := filepath.Join(t.TempDir(), "forged.json")
	_ = os.WriteFile(fp, forged, 0o600)
	c := zapCmd(newDecodeCmd(), map[string]string{"target": fp})
	c.SetArgs([]string{f.raw})
	var err error
	captureStreams(t, func() { err = c.Execute() })
	if output.ExitCode(err) != 3 {
		t.Errorf("forged target: %v", err)
	}
}

// newZapFixtureSameDir is a second zap to the same identity, from f's minter.
func newZapFixtureSameDir(t *testing.T, f *zapFixture) *zapFixture {
	t.Helper()
	priv, _ := identity.Resolve(nil)
	sender, _ := testmint.Key(t)
	note, _ := nip01.NewSignedEvent(1, "another note", priv)
	ev, bill := testmint.CashZap(t, f.minterPriv, sender, note, 21000)
	raw, _ := json.Marshal(ev)
	return &zapFixture{me: f.me, minterPriv: f.minterPriv, minterX: f.minterX, ev: ev, bill: bill, raw: string(raw)}
}

// NIP-CZ §Receiving: one bill is held once. Receiving the same Cash Zap again
// is a conflict and adds nothing; another zap is still received.
func TestReceiveZap_TheSameZapTwiceHoldsOneBill(t *testing.T) {
	f := newZapFixture(t)
	installZapHub(t, &fakeZapHub{res: f.mine(21000, false)})
	trust := map[string]string{"trust-minter": "true"}
	if _, err := runReceiveZap(t, []string{f.raw}, trust); err != nil {
		t.Fatalf("first receive: %v", err)
	}
	if _, err := runReceiveZap(t, []string{f.raw}, trust); output.ExitCode(err) != 5 {
		t.Fatalf("second receive: exit %d (%v), want 5 (conflict)", output.ExitCode(err), err)
	}
	l, _ := ledger.Load()
	if held := l.Held(); len(held) != 1 {
		t.Fatalf("held %d bills from one zap", len(held))
	}
	other := newZapFixtureSameDir(t, f)
	if _, err := runReceiveZap(t, []string{other.raw}, trust); err != nil {
		t.Fatalf("another zap: %v", err)
	}
	l, _ = ledger.Load()
	if held := l.Held(); len(held) != 2 {
		t.Errorf("held %d bills after a second, different zap; want 2", len(held))
	}
}
