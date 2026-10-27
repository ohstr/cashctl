package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
	"github.com/ohstr/cashctl/internal/testmint"
)

// fakeCashHub stands in for nipcash/client's HubClient: it writes the call
// ahead as MintCash does, then answers as told, minting bills signed by its
// identity.
type fakeCashHub struct {
	t                 *testing.T
	hubPriv, hubXOnly string
	infoErr           error
	network           string
	// answer is the outcome after the write-ahead: nil mints a bill.
	answer  func(pc nipcash.PendingCall) error
	signer  string // the key bills are signed with (the hub's by default)
	lookup  func(pc nipcash.PendingCall, p nipcash.MintCashParams) (nipcashclient.LookupOutcome, error)
	mints   int
	lastReq nipcash.MintCashParams
}

func (f *fakeCashHub) Info(context.Context) (*nipcashclient.HubInfo, error) {
	if f.infoErr != nil {
		return nil, f.infoErr
	}
	return &nipcashclient.HubInfo{IdentityXOnly: f.hubXOnly, Chain: "flokicoin", Network: f.network, Verified: true,
		MaxRequestAge: 60, LookupOffered: true}, nil
}
func (f *fakeCashHub) Close() {}

// bill mints a bill for p's first recipient, signed by f.signer.
func (f *fakeCashHub) bill() (string, string) {
	wallet := testmint.Hex32(f.t)
	signer := f.signer
	if signer == "" {
		signer = f.hubPriv
	}
	tok := testmint.Bill(f.t, nipcash.Token{HRP: "lokicash", WalletPubkey: wallet, Secret: testmint.Hex32(f.t),
		RelayURLs: []string{"wss://hub.example"}, IdentityRequired: ptrTo(false)}, signer)
	return tok, wallet
}

func (f *fakeCashHub) MintCash(ctx context.Context, p nipcash.MintCashParams, j nipcashclient.Journal, device string) (*nipcash.MintCashResult, error) {
	if p.RequestID == "" {
		id, err := nipcash.NewRequestID()
		if err != nil {
			f.t.Fatal(err)
		}
		p.RequestID = id
	}
	pc, err := p.Pending(strings.Repeat("ab", 32))
	if err != nil {
		return nil, err
	}
	pc.Deadline, pc.HubIdentity, pc.MaxRequestAge, pc.Device, pc.Chain = time.Now().Unix()+120, f.hubXOnly, 60, device, "flokicoin"
	if err := j.WriteAhead(ctx, []nipcash.PendingCall{pc}); err != nil {
		return nil, errors.Join(nipcashclient.ErrNotSent, err)
	}
	f.mints++
	f.lastReq = p
	if f.answer != nil {
		if err := f.answer(pc); err != nil {
			return nil, err
		}
	}
	tok, wallet := f.bill()
	return &nipcash.MintCashResult{WalletPubkey: wallet, CashToken: tok}, nil
}

func (f *fakeCashHub) LookupMint(_ context.Context, pc nipcash.PendingCall) (nipcashclient.LookupOutcome, error) {
	if f.lookup == nil {
		return nipcashclient.LookupOutcome{NotServed: true}, nil
	}
	return f.lookup(pc, f.lastReq)
}

// doneResult is a lookup's done result for the mint p asked for.
func (f *fakeCashHub) doneResult(p nipcash.MintCashParams) json.RawMessage {
	req, err := p.Request()
	if err != nil {
		f.t.Fatal(err)
	}
	tok, wallet := f.bill()
	raw, _ := json.Marshal(map[string]any{"wallet_pubkey": wallet, "cash_token": tok, "expires_at": nil, "recipients": req.Recipients})
	return raw
}

func setUpCashHub(t *testing.T) (*fakeCashHub, string) {
	t.Helper()
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	priv, xonly := testmint.Key(t)
	f := &fakeCashHub{t: t, hubPriv: priv, hubXOnly: xonly, network: "mainnet"}
	prev := dialCashHubFn
	dialCashHubFn = func(context.Context, string) (cashHub, error) { return f, nil }
	t.Cleanup(func() { dialCashHubFn = prev })
	return f, testmint.CashHub(t, priv, "")
}

func newTestMintCmd(flags map[string]string) *cobra.Command {
	c := newCashMintCmd()
	c.Flags().Bool("json", true, "")
	c.Flags().Bool("yes", false, "")
	c.Flags().String("connection", "", "")
	for k, v := range flags {
		_ = c.Flags().Set(k, v)
	}
	return c
}

func runMint(t *testing.T, hub string, flags map[string]string) (string, error) {
	t.Helper()
	if flags == nil {
		flags = map[string]string{}
	}
	flags["hub"] = hub
	c := newTestMintCmd(flags)
	var err error
	stdout, _ := captureStreams(t, func() { err = runCashMint(c, []string{"100"}) })
	return stdout, err
}

func mintCalls(t *testing.T) []ledger.Call {
	t.Helper()
	l, err := ledger.Load()
	if err != nil {
		t.Fatal(err)
	}
	var out []ledger.Call
	for _, c := range l.Calls {
		if c.Method == nipcash.MethodMintCash {
			out = append(out, c)
		}
	}
	return out
}

// A mint to cash is written ahead, checked (the Hub's mint signature), closed
// done, and handed out as a cash note; --json carries the network fields.
func TestCashMint_ToCashHandsOutANote(t *testing.T) {
	_, hub := setUpCashHub(t)
	stdout, err := runMint(t, hub, nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	var doc map[string]any
	if jErr := json.Unmarshal([]byte(stdout), &doc); jErr != nil {
		t.Fatalf("stdout %q: %v", stdout, jErr)
	}
	for _, k := range []string{"cash_token", "handoff", "wallet_pubkey", "amount_millis", "call", "network", "chain", "test_network", "hub_verified"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("--json lacks %q", k)
		}
	}
	if h, _ := doc["handoff"].(string); !strings.Contains(h, "#") {
		t.Errorf("handoff %q is not a cash note", h)
	}
	calls := mintCalls(t)
	if len(calls) != 1 || calls[0].State != ledger.CallDone || len(calls[0].Secrets) != 0 || calls[0].Chain != "flokicoin" ||
		calls[0].AuthorizerKind != ledger.AuthCashHubClient {
		t.Fatalf("calls %+v, want one done mint without secrets", calls)
	}
	l, _ := ledger.Load()
	if len(l.Handoffs) != 1 || l.Handoffs[0].Kind != ledger.HandoffCashNote {
		t.Errorf("handoffs %+v", l.Handoffs)
	}
}

// The online check failing is connection_mismatch; the URI form and a
// circlehub1 are connection_invalid; nothing is written.
func TestCashMint_RefusesBeforeWriting(t *testing.T) {
	f, hub := setUpCashHub(t)
	f.infoErr = nipcash.ErrHubIdentityMismatch
	_, err := runMint(t, hub, nil)
	if ce := output.AsCLIError(err); ce == nil || output.ExitCode(err) != 7 || ce.Reason != reasonConnectionMismatch {
		t.Errorf("mismatch: %v", err)
	}
	priv, _ := testmint.Key(t)
	for _, bad := range []string{"nostr+walletconnect://" + strings.Repeat("ab", 32) + "?relay=wss%3A%2F%2Fr.example&secret=" + strings.Repeat("cd", 32), testmint.CircleHub(t, priv, "")} {
		_, err := runMint(t, bad, nil)
		if ce := output.AsCLIError(err); ce == nil || output.ExitCode(err) != 3 || ce.Reason != reasonConnectionInvalid {
			t.Errorf("%.20s: %v", bad, err)
		}
	}
	if f.mints != 0 || len(mintCalls(t)) != 0 {
		t.Error("something was written or sent")
	}
}

// -c never applies to a mint.
func TestCashMint_RejectsConnectionFlag(t *testing.T) {
	_, hub := setUpCashHub(t)
	_, err := runMint(t, hub, map[string]string{"connection": "work"})
	if output.ExitCode(err) != 2 {
		t.Errorf("err %v, want usage", err)
	}
}

// BAD_REQUEST closes the call not applied; a lost reply keeps it open (exit 8),
// an unknown lookup asks for --replace, which mints again under the derived id.
func TestCashMint_OutcomesAndReplace(t *testing.T) {
	f, hub := setUpCashHub(t)
	f.answer = func(nipcash.PendingCall) error {
		return &nipcashclient.FinalError{Method: nipcash.MethodMintCash, Code: "BAD_REQUEST", Message: "bad"}
	}
	_, err := runMint(t, hub, nil)
	if output.ExitCode(err) != 3 {
		t.Fatalf("BAD_REQUEST: %v", err)
	}
	if calls := mintCalls(t); len(calls) != 1 || calls[0].State != ledger.CallNotApplied {
		t.Fatalf("after BAD_REQUEST: %+v", calls)
	}

	f.answer = func(pc nipcash.PendingCall) error {
		return &nipcashclient.IndeterminateError{Call: pc, Cause: errors.New("no reply")}
	}
	_, err = runMint(t, hub, nil)
	ce := output.AsCLIError(err)
	if output.ExitCode(err) != 8 || ce.Call["method"] != nipcash.MethodMintCash {
		t.Fatalf("lost reply: %v", err)
	}
	l, _ := ledger.Load()
	var open *ledger.Call
	for i := range l.Calls {
		if l.Calls[i].Open() {
			open = &l.Calls[i]
		}
	}
	if open == nil || len(open.Secrets) != 1 {
		t.Fatalf("open call %+v, want its cash secret kept", open)
	}
	open.Deadline = time.Now().Unix() - 1
	_ = l.Save()

	f.lookup = func(nipcash.PendingCall, nipcash.MintCashParams) (nipcashclient.LookupOutcome, error) {
		return nipcashclient.LookupOutcome{Status: nipcash.LookupUnknown}, nil
	}
	l = settleNow(t)
	c, _ := l.FindCall(open.CallID)
	if c.State != ledger.CallStuck || c.Detail != "confirm_replace" {
		t.Fatalf("after unknown: %s/%s, want stuck/confirm_replace (a mint is never re-placed on its own)", c.State, c.Detail)
	}

	f.answer = nil
	cmd := newCashSettleCmd()
	cmd.Flags().Bool("json", true, "")
	cmd.Flags().Bool("yes", true, "")
	_ = cmd.Flags().Set("replace", open.CallID)
	captureStreams(t, func() { err = runCashSettle(cmd, nil) })
	if err != nil {
		t.Fatalf("settle --replace: %v", err)
	}
	l, _ = ledger.Load()
	parent, _ := l.FindCall(open.CallID)
	if parent.State != ledger.CallReplaced {
		t.Errorf("parent %s/%s, want replaced", parent.State, parent.Detail)
	}
	derived, _ := nipcash.DerivedRequestID(open.RequestID)
	child, ok := l.FindCall(ledger.CallIDFor(derived))
	if !ok || child.State != ledger.CallDone || child.Replaces != open.RequestID {
		t.Errorf("derived call %+v ok=%v, want done, replacing the parent", child, ok)
	}
}

// A lookup that finds the mint done records it from the kept secret.
func TestCashMint_LookupDoneApplies(t *testing.T) {
	f, hub := setUpCashHub(t)
	f.answer = func(pc nipcash.PendingCall) error {
		return &nipcashclient.IndeterminateError{Call: pc, Cause: errors.New("no reply")}
	}
	_, _ = runMint(t, hub, nil)
	l, _ := ledger.Load()
	c := &l.Calls[0]
	c.Deadline = time.Now().Unix() - 1
	_ = l.Save()
	f.lookup = func(_ nipcash.PendingCall, p nipcash.MintCashParams) (nipcashclient.LookupOutcome, error) {
		return nipcashclient.LookupOutcome{Status: nipcash.LookupDone, Result: f.doneResult(p)}, nil
	}
	l = settleNow(t)
	c, _ = l.FindCall(c.CallID)
	if c.State != ledger.CallDone || len(l.Handoffs) != 1 {
		t.Errorf("call %s/%s handoffs %d, want done with the note handed out", c.State, c.Detail, len(l.Handoffs))
	}
}

// A bill not signed by the Hub's identity is never accepted: the call stays
// for a lookup.
func TestCashMint_WrongSignerKeepsTheCall(t *testing.T) {
	f, hub := setUpCashHub(t)
	f.signer, _ = testmint.Key(t)
	_, err := runMint(t, hub, nil)
	if output.ExitCode(err) != 8 {
		t.Fatalf("err %v, want indeterminate", err)
	}
	if calls := mintCalls(t); len(calls) != 1 || calls[0].State != ledger.CallStuck || len(calls[0].Secrets) != 1 {
		t.Errorf("calls %+v, want stuck with its secret", calls)
	}
}

// An unverified string is refused under --json without --allow-unverified,
// after the online check and before anything is written.
func TestCashMint_UnverifiedStringNeedsConfirmation(t *testing.T) {
	f, _ := setUpCashHub(t)
	h, err := nipcash.DecodeCashHubConnection(testmint.CashHub(t, f.hubPriv, ""))
	if err != nil {
		t.Fatal(err)
	}
	h.Label, h.HasLabel = "tampered", true
	bad, err := nipcash.EncodeCashHubConnection(h)
	if err != nil {
		t.Fatal(err)
	}
	_, err = runMint(t, bad, nil)
	if ce := output.AsCLIError(err); ce == nil || output.ExitCode(err) != 2 || ce.Reason != reasonConnectionUnverified {
		t.Fatalf("err %v, want connection_unverified", err)
	}
	if f.mints != 0 {
		t.Error("a mint was sent")
	}
	if _, err := runMint(t, bad, map[string]string{"allow-unverified": "true"}); err != nil {
		t.Fatalf("with --allow-unverified: %v", err)
	}
}
