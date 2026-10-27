package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nipCZ"
	"github.com/ohstr/nmilat/nipIC"
	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/ohstr/nmilat/utils"

	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
	"github.com/ohstr/cashctl/internal/testmint"
)

// keyZapFixture is a Cash Zap to a discord connection key the user controls
// (an attestation by a local IA), and the --as that claims it.
type keyZapFixture struct {
	*zapFixture
	keyEv               *nip01.Event
	keyRaw, keyBill, as string
}

func newKeyZapFixture(t *testing.T) *keyZapFixture {
	t.Helper()
	f := newZapFixture(t)
	userPriv, userPub := testmint.Key(t)
	iaPriv, iaPub := testmint.Key(t)
	ck, err := nipIC.ConnectionKeyFromAttested("discord", "482910")
	if err != nil {
		t.Fatal(err)
	}
	att, err := nipIC.NewAttestation(nipIC.AttestationParams{PrivateKey: iaPriv, ConnectionKey: ck, UserPubkey: userPub,
		Platform: "discord", AuthType: "test", ExpirationDays: 30,
		Evidence: nipIC.Evidence{Platform: "discord", UserID: "482910", Username: "u", VerifiedAt: time.Now().Unix()}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(att)
	file := filepath.Join(t.TempDir(), "att.json")
	if err := os.WriteFile(file, b, 0o600); err != nil {
		t.Fatal(err)
	}
	sender, _ := testmint.Key(t)
	ev, bill := testmint.CashZapToKey(t, f.minterPriv, sender, string(ck), "discord", iaPub, 21000)
	raw, _ := json.Marshal(ev)
	return &keyZapFixture{zapFixture: f, keyEv: ev, keyRaw: string(raw), keyBill: bill,
		as: "connection-key:" + userPriv + ",discord,482910," + file}
}

// A zap to a connection key needs its --as; with it, rule 9 is a full
// in-place transfer to the local pubkey, written ahead, and the bill is then
// held, bound to the local key, with its zap event.
func TestReceiveZap_ConnectionKeyClaim(t *testing.T) {
	f := newKeyZapFixture(t)
	if _, err := runReceiveZap(t, []string{f.keyRaw}, map[string]string{"trust-minter": "true"}); output.ExitCode(err) != 2 {
		t.Fatalf("without --as: %v", err)
	}
	hooks := installFakeCaller(t)
	var sent nipcash.CashTransferParams
	hooks.transfer = func(_ *ledger.Entry, p nipcash.CashTransferParams) (*nipcash.CashTransferResult, error) {
		sent = p
		return &nipcash.CashTransferResult{AmountMillis: 21000}, nil
	}
	if _, err := runReceiveZap(t, []string{f.keyRaw}, map[string]string{"trust-minter": "true", "as": f.as}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if sent.SplitAmount != nil || sent.CurrentAmount != 21000 {
		t.Errorf("not a full in-place transfer: %+v", sent)
	}
	l, _ := ledger.Load()
	held := l.Held()
	if len(held) != 1 || held[0].ZapEvent == "" || held[0].ConnectionKeyPlatform != "" || held[0].IdentityRequired == nil || !*held[0].IdentityRequired {
		t.Fatalf("held %+v", held)
	}
	for _, c := range l.Calls {
		if c.Purpose != purposeZapClaim || c.State != ledger.CallDone {
			t.Errorf("call %s/%s", c.Purpose, c.State)
		}
	}
}

// A claim the Hub answers with a split (not in place) is not received and the
// call is kept; a refusal leaves nothing held; a lost reply stays open (exit 8)
// and is applied by the settle engine.
func TestReceiveZap_ConnectionKeyClaimOutcomes(t *testing.T) {
	f := newKeyZapFixture(t)
	hooks := installFakeCaller(t)
	hooks.transfer = func(*ledger.Entry, nipcash.CashTransferParams) (*nipcash.CashTransferResult, error) {
		return &nipcash.CashTransferResult{AmountMillis: 21000, NewWalletPubkey: strings.Repeat("ab", 32), NewWalletToken: "lokicash1x"}, nil
	}
	if _, err := runReceiveZap(t, []string{f.keyRaw}, map[string]string{"trust-minter": "true", "as": f.as}); output.ExitCode(err) != 8 {
		t.Fatalf("split answer: %v", err)
	}
	if l, _ := ledger.Load(); len(l.Held()) != 0 {
		t.Error("a claim that wasn't in place was kept as held")
	}

	f = newKeyZapFixture(t)
	hooks.transfer = func(*ledger.Entry, nipcash.CashTransferParams) (*nipcash.CashTransferResult, error) {
		return nil, &relayclient.WalletError{Code: "BAD_REQUEST", Message: "no"}
	}
	if _, err := runReceiveZap(t, []string{f.keyRaw}, map[string]string{"trust-minter": "true", "as": f.as}); output.ExitCode(err) != 3 {
		t.Fatalf("refused: %v", err)
	}
	if l, _ := ledger.Load(); len(l.Held()) != 0 {
		t.Error("a refused claim was kept")
	}

	f = newKeyZapFixture(t)
	hooks.transfer = func(*ledger.Entry, nipcash.CashTransferParams) (*nipcash.CashTransferResult, error) {
		return nil, errors.New("no reply")
	}
	_, err := runReceiveZap(t, []string{f.keyRaw}, map[string]string{"trust-minter": "true", "as": f.as})
	if output.ExitCode(err) != 8 {
		t.Fatalf("lost reply: %v", err)
	}
	l, _ := ledger.Load()
	var open *ledger.Call
	for i := range l.Calls {
		if l.Calls[i].Open() {
			open = &l.Calls[i]
		}
	}
	if open == nil || open.Purpose != purposeZapClaim {
		t.Fatalf("no open claim: %+v", l.Calls)
	}
	open.Deadline = time.Now().Unix() - 1
	_ = l.Save()
	result, _ := json.Marshal(map[string]any{"amount_millis": 21000, "identity_type": "pubkey", "identity_value": f.me})
	withLookup(t, func(*ledger.Call) (nipcashclient.LookupOutcome, error) {
		return nipcashclient.LookupOutcome{Status: nipcash.LookupDone, Result: result}, nil
	})
	t.Setenv(asCredentialEnv, f.as)
	l = settleNow(t)
	if held := l.Held(); len(held) != 1 || held[0].ConnectionKeyPlatform != "" {
		t.Errorf("after settle: held %+v", held)
	}
}

// Rule 8 reads the recipient's own 10522 (from --relay): a minter it lists is
// trusted without --trust-minter.
func TestReceiveZap_RecipientInfoListsTheMinter(t *testing.T) {
	f := newZapFixture(t)
	installZapHub(t, &fakeZapHub{res: f.mine(21000, false)})
	prev := zapInfoFn
	var asked []string
	zapInfoFn = func(_ context.Context, relays []string, pub string) (*nipCZ.CashZapInfo, []string) {
		asked = relays
		return &nipCZ.CashZapInfo{Pubkey: pub, Mints: map[string][]string{"flokicoin": {f.minterX}}}, nil
	}
	t.Cleanup(func() { zapInfoFn = prev })
	if _, err := runReceiveZap(t, []string{f.raw}, nil); output.ExitCode(err) != 2 {
		t.Fatalf("without a relay the minter is untrusted: %v", err)
	}
	if _, err := runReceiveZap(t, []string{f.raw}, map[string]string{"relay": "wss://mine.example"}); err != nil {
		t.Fatalf("listed minter: %v", err)
	}
	if len(asked) != 1 || asked[0] != "wss://mine.example" {
		t.Errorf("asked %v", asked)
	}
}

// --find-zap also looks where the local identity reads its zaps (its 10522
// relays), never on the bill's hints.
func TestReceiveZap_FindZapUsesIncomingRelays(t *testing.T) {
	f := newZapFixture(t)
	installZapHub(t, &fakeZapHub{res: f.mine(21000, false)})
	prevInfo, prevFind := zapInfoFn, findZapEvents
	zapInfoFn = func(context.Context, []string, string) (*nipCZ.CashZapInfo, []string) {
		return &nipCZ.CashZapInfo{Relays: []string{"wss://inbox.example"}}, nil
	}
	var asked []string
	findZapEvents = func(_ context.Context, relays []string, _ string) ([]*nip01.Event, error) {
		asked = relays
		return []*nip01.Event{f.ev}, nil
	}
	t.Cleanup(func() { zapInfoFn, findZapEvents = prevInfo, prevFind })
	if _, err := runReceiveZap(t, []string{f.bill}, map[string]string{"find-zap": "true", "relay": "wss://mine.example", "trust-minter": "true"}); err != nil {
		t.Fatalf("find-zap: %v", err)
	}
	joined := strings.Join(asked, ",")
	if !strings.Contains(joined, "wss://inbox.example") || !strings.Contains(joined, "wss://mine.example") || strings.Contains(joined, "hub.example") {
		t.Errorf("asked %v", asked)
	}
}

// cash minters add|rm|list keeps trust per chain.
func TestCashMinters(t *testing.T) {
	f := newZapFixture(t)
	add := zapCmd(newCashMintersCmd(), nil)
	add.SetArgs([]string{"add", "02" + f.minterX, "--chain", "flokicoin"})
	var err error
	captureStreams(t, func() { err = add.Execute() })
	if err != nil {
		t.Fatal(err)
	}
	list, _ := config.TrustedMinters()
	if len(list) != 1 || list[0].MinterXOnly != f.minterX || list[0].Chain != "flokicoin" {
		t.Fatalf("minters %+v", list)
	}
	noChain := zapCmd(newCashMintersCmd(), nil)
	noChain.SetArgs([]string{"add", f.minterX})
	captureStreams(t, func() { err = noChain.Execute() })
	if output.ExitCode(err) != 2 {
		t.Errorf("without --chain: %v", err)
	}
	rm := zapCmd(newCashMintersCmd(), nil)
	rm.SetArgs([]string{"rm", f.minterX, "--chain", "flokicoin"})
	captureStreams(t, func() { err = rm.Execute() })
	if list, _ := config.TrustedMinters(); err != nil || len(list) != 0 {
		t.Errorf("after rm: %v %+v", err, list)
	}
	_ = utils.GetPublicKey
}
