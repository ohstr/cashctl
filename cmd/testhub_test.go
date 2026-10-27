package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	relayclient "github.com/ohstr/nmilat/relay/client"

	"github.com/ohstr/cashctl/internal/crashpoint"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/store"
)

// fakeBillHub is a token-aware stand-in for the Hubs behind every held bill:
// each bill is keyed by its token, and answers only to the credential that
// currently spends it. Unlike a stub that ignores which bill it is asked
// about, a probe or move on bill A can never answer for bill B, so a carve
// (the bill moving to a new token) is observable.
type fakeBillHub struct {
	t  *testing.T
	mu sync.Mutex

	bills map[string]*fakeBill
	keys  []fakeKey

	// carve makes a move to a cash secret leave the old token and continue
	// under a new one, as a Hub does for a bill that has changed hands.
	carve bool
	// dropReplies makes the next n moves commit and then lose their reply.
	dropReplies int
	// probesFail makes every probe go unanswered.
	probesFail bool
	// afterCommit runs once a move has committed, before its reply.
	afterCommit func()

	moves   int
	probes  []fakeProbe
	carveNo int
}

type fakeBill struct {
	owner  nipcash.Credential
	amount uint64
	gone   bool
}

type fakeKey struct {
	target nipcash.Target
	cred   nipcash.Credential
}

type fakeProbe struct {
	token string
	cred  nipcash.Credential
}

// newFakeBillHub installs the fake behind every bill-creating call (through a
// fake caller that writes each call ahead like nmilat) and both Hub probes for
// the rest of the test.
func newFakeBillHub(t *testing.T) *fakeBillHub {
	t.Helper()
	h := &fakeBillHub{t: t, bills: map[string]*fakeBill{}}
	hooks := installFakeCaller(t)
	hooks.transfer = func(e *ledger.Entry, p nipcash.CashTransferParams) (*nipcash.CashTransferResult, error) {
		return h.move(e, p.Credential, p.To)
	}
	origLive, origAmount := probeCredentialFn, probeAmountFn
	t.Cleanup(func() { probeCredentialFn, probeAmountFn = origLive, origAmount })
	probeCredentialFn = func(e *ledger.Entry, cred nipcash.Credential) (bool, error) {
		_, ok, err := h.probe(e, cred)
		return ok, err
	}
	probeAmountFn = func(e *ledger.Entry, cred nipcash.Credential) (uint64, bool, error) {
		return h.probe(e, cred)
	}
	return h
}

// bill registers token as a live bill of amount, spent by owner.
func (h *fakeBillHub) bill(token string, owner nipcash.Credential, amount uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bills[token] = &fakeBill{owner: owner, amount: amount}
}

// key lets a move to priv's pubkey hand the bill to priv's signing credential.
func (h *fakeBillHub) key(priv, pub string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.keys = append(h.keys, fakeKey{target: nipcash.Pubkey(pub), cred: nipcash.BySigning(priv)})
}

// owner is the credential that spends token now, or nil if no live bill has it.
func (h *fakeBillHub) owner(token string) nipcash.Credential {
	h.mu.Lock()
	defer h.mu.Unlock()
	if b := h.bills[token]; b != nil && !b.gone {
		return b.owner
	}
	return nil
}

// liveTokens is every token still live on the fake.
func (h *fakeBillHub) liveTokens() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for tok, b := range h.bills {
		if !b.gone {
			out = append(out, tok)
		}
	}
	return out
}

func (h *fakeBillHub) moveCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.moves
}

func (h *fakeBillHub) credFor(target nipcash.Target) nipcash.Credential {
	if ct, ok := target.(*nipcash.CashTarget); ok {
		return nipcash.BySecret(ct.Secret())
	}
	for _, k := range h.keys {
		if k.target == target {
			return k.cred
		}
	}
	h.t.Fatalf("fakeBillHub: move to a target it has no key for: %#v", target)
	return nil
}

// refusal is the Hub's answer to a move it cannot make: no such bill, or a
// credential that does not spend it (UNAUTHORIZED for a wrong cash secret,
// NIP-CASH §Errors).
func refusal(cred nipcash.Credential, known bool) error {
	if known {
		return &relayclient.WalletError{Method: "cash_transfer", Code: "UNAUTHORIZED", Message: "credential does not spend this bill"}
	}
	return &relayclient.WalletError{Method: "cash_transfer", Code: "NOT_FOUND", Message: "no such bill"}
}

// move is a whole-amount cash_transfer of e's bill to target, spent with cred.
func (h *fakeBillHub) move(e *ledger.Entry, cred nipcash.Credential, target nipcash.Target) (*nipcash.CashTransferResult, error) {
	h.mu.Lock()
	h.moves++
	b := h.bills[e.Token]
	if b == nil || b.gone || b.owner != cred {
		h.mu.Unlock()
		return nil, refusal(cred, b != nil && !b.gone)
	}
	newOwner := h.credFor(target)
	res := &nipcash.CashTransferResult{AmountMillis: b.amount}
	if _, isCash := target.(*nipcash.CashTarget); isCash && h.carve {
		h.carveNo++
		tok := fmt.Sprintf("%s-carved%d", e.Token, h.carveNo)
		h.bills[tok] = &fakeBill{owner: newOwner, amount: b.amount}
		b.gone = true
		res.NewWalletToken, res.NewWalletPubkey = tok, "pk-"+tok
	} else {
		b.owner = newOwner
	}
	drop := h.dropReplies > 0
	if drop {
		h.dropReplies--
	}
	hook := h.afterCommit
	h.afterCommit = nil
	h.mu.Unlock()
	if hook != nil {
		hook()
	}
	if drop {
		return nil, context.DeadlineExceeded
	}
	return res, nil
}

// probe answers whether cred spends e's bill, and for how much.
func (h *fakeBillHub) probe(e *ledger.Entry, cred nipcash.Credential) (uint64, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.probes = append(h.probes, fakeProbe{token: e.Token, cred: cred})
	if h.probesFail {
		return 0, false, context.DeadlineExceeded
	}
	b := h.bills[e.Token]
	if b == nil || b.gone || b.owner != cred {
		return 0, false, nil
	}
	return b.amount, true, nil
}

// failSaves makes every later ledger write fail, until the returned restore
// runs (also run at cleanup). A trigger rather than file permissions, which a
// test running as root ignores.
func failSaves(t *testing.T) (restore func()) {
	t.Helper()
	exec := func(stmts ...string) {
		db, err := store.Open()
		if err != nil {
			t.Fatalf("failSaves: %v", err)
		}
		defer func() { _ = db.Close() }()
		for _, s := range stmts {
			if _, err := db.Exec(s); err != nil {
				t.Fatalf("failSaves: %s: %v", s, err)
			}
		}
	}
	var create, drop []string
	for _, table := range []string{"entries", "pending_calls"} {
		for _, op := range []string{"UPDATE", "INSERT"} {
			name := "fail_" + strings.ToLower(op) + "_" + table
			create = append(create, `CREATE TRIGGER `+name+` BEFORE `+op+` ON `+table+` BEGIN SELECT RAISE(ABORT, 'injected save failure'); END`)
			drop = append(drop, `DROP TRIGGER IF EXISTS `+name)
		}
	}
	exec(create...)
	done := false
	restore = func() {
		if done {
			return
		}
		done = true
		exec(drop...)
	}
	t.Cleanup(restore)
	return restore
}

// fakeCallerHooks answer the bill-creating calls a test's fake caller receives.
// transfer gets the held entry being spent (found by the dialled token in the
// ledger the call is written to); consolidate gets the dialled token. Either
// returning context.DeadlineExceeded is a lost reply; a *relayclient.WalletError
// is the Hub's coded answer (BAD_REQUEST final, any other kept open); a
// *notSentError is a call never published.
type fakeCallerHooks struct {
	transfer    func(e *ledger.Entry, p nipcash.CashTransferParams) (*nipcash.CashTransferResult, error)
	consolidate func(dialToken string, p nipcash.CashConsolidateParams) (*nipcash.CashConsolidateResult, error)
	// published counts calls that reached the "Hub".
	published int
	// dialErr fails the dial of these tokens: nothing is sent.
	dialErr map[string]error
}

// activeFakeHooks is the fake caller installed for the running test, so two
// helpers installing one share it.
var activeFakeHooks *fakeCallerHooks

// installFakeCaller puts a fake behind dialCallerFn for the rest of the test.
func installFakeCaller(t *testing.T) *fakeCallerHooks {
	t.Helper()
	if activeFakeHooks != nil {
		return activeFakeHooks
	}
	hooks := &fakeCallerHooks{}
	activeFakeHooks = hooks
	orig := dialCallerFn
	t.Cleanup(func() { dialCallerFn, activeFakeHooks = orig, nil })
	dialCallerFn = func(_ context.Context, token string) (billCaller, error) {
		if err := hooks.dialErr[token]; err != nil {
			return nil, err
		}
		return &fakeCaller{t: t, token: token, hooks: hooks}, nil
	}
	return hooks
}

type fakeCaller struct {
	t     *testing.T
	token string
	hooks *fakeCallerHooks
}

func (c *fakeCaller) Close() {}

// writeAhead does what nmilat does before publishing: the call, with its
// deadline and generated secrets, goes to the journal first.
func (c *fakeCaller) writeAhead(ctx context.Context, j nipcashclient.Journal, device string, pc nipcash.PendingCall) (nipcash.PendingCall, error) {
	pc.Deadline, pc.HubIdentity, pc.MaxRequestAge, pc.Device = time.Now().Unix()+60, strings.Repeat("ab", 32), 120, device
	if err := j.WriteAhead(ctx, []nipcash.PendingCall{pc}); err != nil {
		return pc, fmt.Errorf("%w: write ahead: %v", nipcashclient.ErrNotSent, err)
	}
	return pc, nil
}

// answer maps a hook's result the way nmilat maps a Hub's answer to a call
// carrying a request_id.
func answer[T any](c *fakeCaller, pc nipcash.PendingCall, res T, err error) (T, error) {
	var zero T
	var notSent *notSentError
	var walletErr *relayclient.WalletError
	var gone *nipcash.BillGone
	switch {
	case errors.As(err, &gone):
		// A tombstone answer: definitive, the named bill is destroyed.
		c.hooks.published++
		return zero, err
	case err == nil:
		c.hooks.published++
		// nmilat's post-publish hook (WithAfterPublish).
		crashpoint.Hit("calls.after_send")
		return res, nil
	case errors.As(err, &notSent):
		return zero, fmt.Errorf("%w: %v", nipcashclient.ErrNotSent, err)
	case errors.As(err, &walletErr) && walletErr.Code == "BAD_REQUEST":
		c.hooks.published++
		return zero, &nipcashclient.FinalError{Method: pc.Method, Code: walletErr.Code, Message: walletErr.Message}
	case errors.As(err, &walletErr):
		c.hooks.published++
		pc.LastCode = walletErr.Code
		return zero, &nipcashclient.IndeterminateError{Call: pc, Code: walletErr.Code, Message: walletErr.Message, Cause: err}
	default:
		c.hooks.published++
		return zero, &nipcashclient.IndeterminateError{Call: pc, Cause: err}
	}
}

func (c *fakeCaller) CashTransfer(ctx context.Context, p nipcash.CashTransferParams, j nipcashclient.Journal, device string) (*nipcash.CashTransferResult, error) {
	tok, err := nipcash.Decode(c.token)
	wallet := strings.Repeat("cd", 32)
	if err == nil {
		wallet = tok.WalletPubkey
	}
	if p.RequestID == "" {
		p.RequestID, _ = nipcash.NewRequestID()
	}
	pc, err := p.Pending(wallet)
	if err != nil {
		return nil, err
	}
	if pc, err = c.writeAhead(ctx, j, device, pc); err != nil {
		return nil, err
	}
	var e *ledger.Entry
	if lj, ok := j.(*ledgerJournal); ok {
		e, _ = lj.l.FindByToken(c.token)
	}
	if e == nil {
		e = &ledger.Entry{Token: c.token}
	}
	if c.hooks.transfer == nil {
		c.t.Fatalf("fake caller: unexpected cash_transfer on %s", c.token)
	}
	res, err := c.hooks.transfer(e, p)
	return answer(c, pc, res, err)
}

func (c *fakeCaller) CashConsolidate(ctx context.Context, p nipcash.CashConsolidateParams, j nipcashclient.Journal, device string) (*nipcash.CashConsolidateResult, error) {
	if p.RequestID == "" {
		p.RequestID, _ = nipcash.NewRequestID()
	}
	pc, err := p.Pending()
	if err != nil {
		return nil, err
	}
	if pc, err = c.writeAhead(ctx, j, device, pc); err != nil {
		return nil, err
	}
	if c.hooks.consolidate == nil {
		c.t.Fatalf("fake caller: unexpected cash_consolidate on %s", c.token)
	}
	res, err := c.hooks.consolidate(c.token, p)
	return answer(c, pc, res, err)
}

// T-07: a probe on bill A never answers for bill B, and a move of A leaves B alone.
func TestFakeBillHub_AnswersOnlyForTheBillAskedAbout(t *testing.T) {
	h := newFakeBillHub(t)
	h.bill("tok-a", nipcash.BySecret("sa"), 1000)
	h.bill("tok-b", nipcash.BySecret("sb"), 2000)
	a, b := &ledger.Entry{Token: "tok-a"}, &ledger.Entry{Token: "tok-b"}

	if ok, err := probeCredentialFn(b, nipcash.BySecret("sa")); err != nil || ok {
		t.Errorf("bill B answered to bill A's secret: ok=%v err=%v", ok, err)
	}
	if amt, ok, err := probeAmountFn(a, nipcash.BySecret("sa")); err != nil || !ok || amt != 1000 {
		t.Errorf("bill A to its own secret: %d %v %v; want 1000 true nil", amt, ok, err)
	}
	if _, err := h.move(b, nipcash.BySecret("sa"), mustCashTarget(t)); !isWrongSecretDecline(err) {
		t.Errorf("moving bill B with bill A's secret: %v; want UNAUTHORIZED", err)
	}
	if h.owner("tok-b") != nipcash.BySecret("sb") {
		t.Error("a refused move changed bill B")
	}

	h.carve = true
	next := mustCashTarget(t)
	res, err := h.move(a, nipcash.BySecret("sa"), next)
	if err != nil || res.NewWalletToken == "" {
		t.Fatalf("carving bill A: %+v %v", res, err)
	}
	if ok, _ := probeCredentialFn(a, nipcash.BySecret(next.Secret())); ok {
		t.Error("the old token answered after its bill was carved away")
	}
	if ok, _ := probeCredentialFn(&ledger.Entry{Token: res.NewWalletToken}, nipcash.BySecret(next.Secret())); !ok {
		t.Error("the carved bill does not answer to its new secret")
	}
}

// mustCashTarget is a fresh cash target, failing the test if none can be made.
func mustCashTarget(t testing.TB) *nipcash.CashTarget {
	t.Helper()
	ct, err := nipcash.NewCashTarget()
	if err != nil {
		t.Fatal(err)
	}
	return ct
}

// hexWallet is a stable 64-hex wallet key named after s, for fixtures that need
// a wallet key nmilat accepts.
func hexWallet(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Unit tests never wait for a deadline; the ones about the wait opt in.
func init() { placementWaitEnabled = false }
