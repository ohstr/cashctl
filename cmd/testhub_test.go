package cmd

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/ohstr/nmilat/nipcash"
	relayclient "github.com/ohstr/nmilat/relay/client"

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

// newFakeBillHub installs the fake behind moveCashFn and both Hub probes for
// the rest of the test.
func newFakeBillHub(t *testing.T) *fakeBillHub {
	t.Helper()
	h := &fakeBillHub{t: t, bills: map[string]*fakeBill{}}
	origMove, origLive, origAmount := moveCashFn, probeCredentialFn, probeAmountFn
	t.Cleanup(func() { moveCashFn, probeCredentialFn, probeAmountFn = origMove, origLive, origAmount })
	moveCashFn = h.move
	probeCredentialFn = func(e *ledger.Entry, cred nipcash.Credential, _ string) (bool, error) {
		_, ok, err := h.probe(e, cred)
		return ok, err
	}
	probeAmountFn = func(e *ledger.Entry, cred nipcash.Credential, _ string) (uint64, bool, error) {
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

func notFound() error {
	return &relayclient.WalletError{Method: "cash_transfer", Code: "NOT_FOUND", Message: "no such bill"}
}

// move is a whole-amount cash_transfer of e's bill to target, spent with cred.
func (h *fakeBillHub) move(e *ledger.Entry, cred nipcash.Credential, target nipcash.Target) (*nipcash.CashTransferResult, error) {
	h.mu.Lock()
	h.moves++
	b := h.bills[e.Token]
	if b == nil || b.gone || b.owner != cred {
		h.mu.Unlock()
		return nil, notFound()
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
	exec(`CREATE TRIGGER fail_update BEFORE UPDATE ON entries BEGIN SELECT RAISE(ABORT, 'injected save failure'); END`,
		`CREATE TRIGGER fail_insert BEFORE INSERT ON entries BEGIN SELECT RAISE(ABORT, 'injected save failure'); END`)
	done := false
	restore = func() {
		if done {
			return
		}
		done = true
		exec(`DROP TRIGGER IF EXISTS fail_update`, `DROP TRIGGER IF EXISTS fail_insert`)
	}
	t.Cleanup(restore)
	return restore
}

// T-07: a probe on bill A never answers for bill B, and a move of A leaves B alone.
func TestFakeBillHub_AnswersOnlyForTheBillAskedAbout(t *testing.T) {
	h := newFakeBillHub(t)
	h.bill("tok-a", nipcash.BySecret("sa"), 1000)
	h.bill("tok-b", nipcash.BySecret("sb"), 2000)
	a, b := &ledger.Entry{Token: "tok-a"}, &ledger.Entry{Token: "tok-b"}

	if ok, err := probeCredentialFn(b, nipcash.BySecret("sa"), ""); err != nil || ok {
		t.Errorf("bill B answered to bill A's secret: ok=%v err=%v", ok, err)
	}
	if amt, ok, err := probeAmountFn(a, nipcash.BySecret("sa"), ""); err != nil || !ok || amt != 1000 {
		t.Errorf("bill A to its own secret: %d %v %v; want 1000 true nil", amt, ok, err)
	}
	if _, err := moveCashFn(b, nipcash.BySecret("sa"), nipcash.NewCashTarget()); !isWrongSecretDecline(err) {
		t.Errorf("moving bill B with bill A's secret: %v; want NOT_FOUND", err)
	}
	if h.owner("tok-b") != nipcash.BySecret("sb") {
		t.Error("a refused move changed bill B")
	}

	h.carve = true
	next := nipcash.NewCashTarget()
	res, err := moveCashFn(a, nipcash.BySecret("sa"), next)
	if err != nil || res.NewWalletToken == "" {
		t.Fatalf("carving bill A: %+v %v", res, err)
	}
	if ok, _ := probeCredentialFn(a, nipcash.BySecret(next.Secret()), ""); ok {
		t.Error("the old token answered after its bill was carved away")
	}
	if ok, _ := probeCredentialFn(&ledger.Entry{Token: res.NewWalletToken}, nipcash.BySecret(next.Secret()), ""); !ok {
		t.Error("the carved bill does not answer to its new secret")
	}
}
