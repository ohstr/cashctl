package ledger

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nipcash"

	"github.com/ohstr/cashctl/internal/store"
)

func testCall(t *testing.T, sources ...string) Call {
	t.Helper()
	rid, err := nipcash.NewRequestID()
	if err != nil {
		t.Fatal(err)
	}
	ct, err := nipcash.NewCashTarget()
	if err != nil {
		t.Fatal(err)
	}
	return Call{
		RequestID: rid, Method: "cash_transfer", Purpose: "protect", Deadline: time.Now().Unix() + 60, SentH: 120,
		HubIdentity: strings.Repeat("ab", 32), Target: strings.Repeat("cd", 32), TargetCredential: "lokicash1token",
		DeviceID: strings.Repeat("ee", 32), AmountMillis: 1000, Chain: "flokicoin", AuthorizerKind: AuthCashSecret,
		AuthorizerSecret: strings.Repeat("11", 32), SourceEntryIDs: sources, ParamsJSON: `{"current_amount":1000}`,
		Secrets: []CallSecret{{Role: RoleNewIdentity, Secret: ct.Secret()}},
	}
}

// A call and its secrets round-trip through Save and Load; the commitment is
// computed from the secret; the request id and every secret stay out of JSON.
func TestCalls_RoundTripAndStayOutOfJSON(t *testing.T) {
	withTempConfigDir(t)
	l, _ := Load()
	c, err := l.AddCall(testCall(t, "tok-a"))
	if err != nil {
		t.Fatal(err)
	}
	if c.CallID != CallIDFor(c.RequestID) || len(c.CallID) != 12 || c.State != CallWritten {
		t.Fatalf("call %+v: want a 12-hex handle and state written", c)
	}
	want, _ := nipcash.CashTargetFromSecret(c.Secrets[0].Secret)
	if c.Secrets[0].Commitment != want.Commitment() {
		t.Error("commitment not computed from the secret")
	}
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	again, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := again.FindCall(c.CallID)
	if !ok || !callsEqual(*got, *c) {
		t.Fatalf("call after reload:\n%+v\nwant\n%+v", got, c)
	}
	b, _ := json.Marshal(got)
	for _, secret := range []string{c.RequestID, c.Secrets[0].Secret, c.AuthorizerSecret, c.TargetCredential} {
		if strings.Contains(string(b), secret) {
			t.Errorf("a call's JSON carries a secret or its request id: %s", b)
		}
	}
	if _, ok := again.OpenCallFor("tok-a"); !ok {
		t.Error("OpenCallFor does not find the open call on its source")
	}
	if _, ok := again.OpenCallFor("tok-b"); ok {
		t.Error("OpenCallFor found a call on a bill it does not spend")
	}
}

// T-11: the read-back passes for what was written and fails for a row that
// differs in any column, or whose secret no longer matches its commitment.
func TestVerifyWritten_CatchesAnyDifference(t *testing.T) {
	withTempConfigDir(t)
	l, _ := Load()
	c, _ := l.AddCall(testCall(t, "tok-a"))
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	if err := VerifyWritten(*c); err != nil {
		t.Fatalf("VerifyWritten of what was written: %v", err)
	}
	for name, stmt := range map[string]string{
		"deadline":   `UPDATE pending_calls SET deadline = deadline + 1`,
		"commitment": `UPDATE call_secrets SET commitment = '00'`,
	} {
		t.Run(name, func(t *testing.T) {
			db, err := store.Open()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(stmt); err != nil {
				t.Fatal(err)
			}
			_ = db.Close()
			if err := VerifyWritten(*c); !errors.Is(err, ErrReadBack) {
				t.Errorf("VerifyWritten after the %s changed: %v, want ErrReadBack", name, err)
			}
		})
	}
}

// Two processes updating one call: the second is refused, never overwrites.
func TestCalls_ConcurrentUpdateIsRefused(t *testing.T) {
	withTempConfigDir(t)
	l, _ := Load()
	c, _ := l.AddCall(testCall(t))
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	a, _ := Load()
	b, _ := Load()
	_ = a.SetCallState(c.CallID, CallLookup, "no answer")
	_ = b.SetCallState(c.CallID, CallNotApplied, "BAD_REQUEST")
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	if err := b.Save(); !errors.Is(err, ErrConcurrentUpdate) {
		t.Fatalf("second update of one call: %v, want ErrConcurrentUpdate", err)
	}
	final, _ := Load()
	if got, _ := final.FindCall(c.CallID); got.State != CallLookup {
		t.Errorf("state %q, want the first writer's lookup", got.State)
	}
}

// A call never sent is removed with its secrets.
func TestRemoveCall_DeletesTheRowAndItsSecrets(t *testing.T) {
	withTempConfigDir(t)
	l, _ := Load()
	c, _ := l.AddCall(testCall(t))
	_ = l.Save()
	l.RemoveCall(c.CallID)
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	db, _ := store.Open()
	defer func() { _ = db.Close() }()
	var calls, secrets int
	_ = db.QueryRow(`SELECT COUNT(*) FROM pending_calls`).Scan(&calls)
	_ = db.QueryRow(`SELECT COUNT(*) FROM call_secrets`).Scan(&secrets)
	if calls+secrets != 0 {
		t.Errorf("%d calls and %d secrets left, want none", calls, secrets)
	}
}

// The derived id's primary key is the lock: only one insert wins.
func TestInsertDerived_IsALock(t *testing.T) {
	withTempConfigDir(t)
	id, parent := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	if err := InsertDerived(id, parent, nil, time.Now().Unix()+3600); err != nil {
		t.Fatal(err)
	}
	if err := InsertDerived(id, parent, nil, time.Now().Unix()+3600); !errors.Is(err, ErrDerivedRecorded) {
		t.Fatalf("second insert: %v, want ErrDerivedRecorded", err)
	}
	if ok, err := DerivedRecorded(id); !ok || err != nil {
		t.Errorf("DerivedRecorded = %v (%v), want true", ok, err)
	}
}

// A bill-creating call and the entries it touches commit together or not at
// all.
func TestSave_CallsAndEntriesCommitTogether(t *testing.T) {
	withTempConfigDir(t)
	l, _ := Load()
	_, _ = l.Add(Entry{Token: "lokicash1one", WalletPubkey: "w", Secret: "s"})
	_, _ = l.AddCall(testCall(t))
	db, _ := store.Open()
	if _, err := db.Exec(`CREATE TRIGGER fail BEFORE INSERT ON call_secrets BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if err := l.Save(); err == nil {
		t.Fatal("Save succeeded through the injected failure")
	}
	fresh, _ := Load()
	if len(fresh.Entries) != 0 || len(fresh.Calls) != 0 {
		t.Errorf("a failed save left %d entries and %d calls, want none", len(fresh.Entries), len(fresh.Calls))
	}
}

// Final calls go once old enough; open, expired and stuck ones stay.
func TestPruneCalls(t *testing.T) {
	now := time.Now().Unix()
	old := now - 40*24*3600
	l := &Ledger{Calls: []Call{
		{CallID: "done-old", State: CallDone, Deadline: old, CreatedAt: old},
		{CallID: "done-new", State: CallDone, Deadline: now - 60, CreatedAt: now - 60},
		{CallID: "expired-old", State: CallExpired, Deadline: old, CreatedAt: old},
		{CallID: "stuck-old", State: CallStuck, Deadline: old, CreatedAt: old},
		{CallID: "open-old", State: CallLookup, Deadline: old, CreatedAt: old},
	}}
	if n := l.PruneCalls(now, 300); n != 1 {
		t.Errorf("pruned %d, want 1", n)
	}
	if _, ok := l.FindCall("done-old"); ok {
		t.Error("an old final call was kept")
	}
	for _, id := range []string{"done-new", "expired-old", "stuck-old", "open-old"} {
		if _, ok := l.FindCall(id); !ok {
			t.Errorf("%s was pruned", id)
		}
	}
}
