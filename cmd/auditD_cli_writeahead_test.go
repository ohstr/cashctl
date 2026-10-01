package cmd

// Audit D, CLI / local-state hardening role: the rest of D-CLI-1.
//
// auditD_cli_killwindow_test.go covers the consolidate site, which is the one
// the finding was demonstrated against. D-CLI-1 has three sites, though — every
// path that can reach a cash-mode destination — and the other two live in
// cash_transfer.go. These cover transferWithAutoConsolidate end to end through
// its existing attemptTransferFromSourcesFn seam, plus the contract of
// parkDestinationCashSecret itself, which runCashTransfer's direct path has no
// seam to exercise (it dials nipcashclient.Connect itself).
//
// The property under test is always the same one, and it is deliberately
// asserted at the instant it matters rather than afterwards: while the request
// is in flight with the Hub, a process that died right then must still be able
// to find the destination secret in local state.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nipcash"

	"github.com/ohstr/cashctl/internal/ledger"
)

// auditDSeedTwoHeldCash puts two held cash-mode entries in a fresh ledger and
// returns it plus their IDs, in the order Add assigned them.
func auditDSeedTwoHeldCash(t *testing.T) (*ledger.Ledger, []string) {
	t.Helper()
	l, err := ledger.Load()
	if err != nil {
		t.Fatalf("ledger.Load() error = %v", err)
	}
	for _, tok := range []string{"lokicash1wa-a", "lokicash1wa-b"} {
		amt := uint64(1000)
		if _, addErr := l.Add(ledger.Entry{
			Token: tok, WalletPubkey: tok + "-pub", Secret: "s-" + tok,
			AmountMillis: &amt, IdentityRequired: ptrTo(false), CashSecret: "old-" + tok,
			CashProtection: ledger.CashShared, Verified: true,
		}); addErr != nil {
			t.Fatalf("l.Add(%s) error = %v", tok, addErr)
		}
	}
	if err := l.Save(); err != nil {
		t.Fatalf("l.Save() error = %v", err)
	}
	return l, []string{l.Entries[0].ID, l.Entries[1].ID}
}

// TestAuditD_CLI_TransferSitesParkBeforeTheirWireCall covers the two sites in
// cash_transfer.go that neither the consolidate regression test nor the helper
// unit tests above can reach.
//
// Both are unreachable end to end without a relay, and not for a shallow
// reason: runCashTransfer dials nipcashclient.Connect itself, and
// transferWithAutoConsolidate's "Preparing sources..." step calls
// sourceFromEntry, which dials once per source before the seam the suite does
// have (attemptTransferFromSourcesFn) is ever reached. That is also why the
// pre-existing TestTransferWithAutoConsolidate_Declined stops at the confirm.
//
// So this is a source-ordering test, with the same honest limit as the
// signal-handler test next door: it proves the write-ahead is placed before the
// call that can commit the money, not that the write itself works — the
// consolidate regression test and the helper tests above are what prove that,
// against the identical helper. What it specifically catches is the refactor
// that moves or drops one of these two calls while the other two sites keep
// their tests green.
func TestAuditD_CLI_TransferSitesParkBeforeTheirWireCall(t *testing.T) {
	src, err := os.ReadFile("cash_transfer.go")
	if err != nil {
		t.Fatalf("reading cash_transfer.go: %v", err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "cash_transfer.go", src, 0)
	if err != nil {
		t.Fatalf("parsing cash_transfer.go: %v", err)
	}

	// wireCall is the call that can commit money at each site: once it has
	// been placed, a kill destroys an unparked cash secret for good.
	for _, tc := range []struct{ fn, wireCall string }{
		{"runCashTransfer", "client.CashTransfer("},
		{"transferWithAutoConsolidate", "attemptTransferFromSourcesWithRetry("},
	} {
		var body string
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Name.Name != tc.fn || fd.Body == nil {
				continue
			}
			body = string(src[fset.Position(fd.Body.Pos()).Offset:fset.Position(fd.Body.End()).Offset])
		}
		if body == "" {
			t.Fatalf("%s not found in cash_transfer.go — this test is pinning a function that no "+
				"longer exists, so fix the test rather than assuming the site is safe", tc.fn)
		}
		park := strings.Index(body, "parkDestinationCashSecret(")
		wire := strings.Index(body, tc.wireCall)
		if park < 0 {
			t.Errorf("%s never calls parkDestinationCashSecret: its cash-mode destination secret "+
				"exists only in this process's heap while %s is in flight, so a kill there leaves "+
				"a funded, permanently unspendable bill (D-CLI-1)", tc.fn, tc.wireCall)
			continue
		}
		if wire < 0 {
			t.Errorf("%s no longer contains %s — the wire call this ordering is about has moved, "+
				"so re-derive where the park belongs", tc.fn, tc.wireCall)
			continue
		}
		if park > wire {
			t.Errorf("%s calls parkDestinationCashSecret AFTER %s — persisting the secret once the "+
				"Hub may already have committed is not write-ahead at all", tc.fn, tc.wireCall)
		}
	}
}

// TestAuditD_CLI_DirectTransferParksAfterItsConfirm: the direct path must park
// after the spend confirmation, not before. A declined confirm places no call,
// so a park written ahead of it would leave `wallet show` warning about a bill
// the user explicitly refused to create — a false alarm about money at risk,
// which the report above is careful not to raise.
func TestAuditD_CLI_DirectTransferParksAfterItsConfirm(t *testing.T) {
	src, err := os.ReadFile("cash_transfer.go")
	if err != nil {
		t.Fatalf("reading cash_transfer.go: %v", err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "cash_transfer.go", src, 0)
	if err != nil {
		t.Fatalf("parsing cash_transfer.go: %v", err)
	}
	var body string
	for _, decl := range file.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Name.Name == "runCashTransfer" && fd.Body != nil {
			body = string(src[fset.Position(fd.Body.Pos()).Offset:fset.Position(fd.Body.End()).Offset])
		}
	}
	if body == "" {
		t.Fatal("runCashTransfer not found in cash_transfer.go")
	}
	confirm := strings.Index(body, "if !Confirm(cmd, false, message)")
	park := strings.Index(body, "parkDestinationCashSecret(")
	if confirm < 0 {
		t.Skip("runCashTransfer's confirm no longer has the shape this test keys off; re-derive it")
	}
	if park < 0 {
		t.Fatal("runCashTransfer never calls parkDestinationCashSecret")
	}
	if park < confirm {
		t.Error("runCashTransfer parks the destination secret BEFORE its spend confirmation, so " +
			"declining leaves a park for a bill that was never created")
	}
}

// TestAuditD_CLI_ParkIsNotSavedByReleaseAlone pins the half of the contract
// that protects the worst case: the Hub committed, the reply arrived, and the
// local Save that was meant to record it FAILED. release() must not have
// persisted anything on its own, so the park is still on disk for the next
// process to find — the one moment it is genuinely the only copy again.
func TestAuditD_CLI_ParkIsNotSavedByReleaseAlone(t *testing.T) {
	auditDTempLedger(t)
	l, ids := auditDSeedTwoHeldCash(t)

	target := nipcash.NewCashTarget()
	parked, err := parkDestinationCashSecret(l, ids, target)
	if err != nil {
		t.Fatalf("parkDestinationCashSecret() error = %v", err)
	}
	if !auditDLedgerBytesContain(t, target.Secret()) {
		t.Fatal("parkDestinationCashSecret() did not reach the disk at all")
	}

	// The caller's own Save is what persists the clear. Standing in for one
	// that failed: release, then never Save.
	parked.release()
	if got := mustFind(t, l, ids[0]).PendingDestinationCashSecret; got != "" {
		t.Fatalf("release() left the in-memory park set to %q, want cleared", got)
	}
	fresh, err := ledger.Load()
	if err != nil {
		t.Fatalf("ledger.Load() error = %v", err)
	}
	var stillParked bool
	for _, e := range fresh.Entries {
		if e.PendingDestinationCashSecret == target.Secret() {
			stillParked = true
		}
	}
	if !stillParked {
		t.Fatal("release() persisted the clear by itself — so a send whose final Save fails " +
			"loses the destination secret, which is exactly the case the write-ahead exists for")
	}
}

// TestAuditD_CLI_ParkRefusesWhenItCannotRecord is the deliberate choice to fail
// the command rather than place a call whose interruption destroys money with
// the safety net absent. Unreachable through today's commands; the point is
// that it cannot degrade quietly if that ever changes.
func TestAuditD_CLI_ParkRefusesWhenItCannotRecord(t *testing.T) {
	auditDTempLedger(t)
	l, err := ledger.Load()
	if err != nil {
		t.Fatalf("ledger.Load() error = %v", err)
	}

	if _, err := parkDestinationCashSecret(l, []string{"tok-nope"}, nipcash.NewCashTarget()); err == nil {
		t.Fatal("parkDestinationCashSecret(no such row) = nil error, want a refusal — proceeding " +
			"would place the call with nothing recording the secret")
	}
	if _, err := parkDestinationCashSecret(l, nil, nipcash.NewCashTarget()); err == nil {
		t.Fatal("parkDestinationCashSecret(no source ids) = nil error, want a refusal")
	}
}

// TestAuditD_CLI_ParkIsANoOpForANonCashTarget: a pubkey/connection-key
// destination is reachable from the Hub's own reply, so there is no local-only
// secret at risk and nothing should be written. Without this the helper could
// "pass" its other tests by parking indiscriminately, leaving every ordinary
// transfer with a stale warning on it.
func TestAuditD_CLI_ParkIsANoOpForANonCashTarget(t *testing.T) {
	auditDTempLedger(t)
	l, ids := auditDSeedTwoHeldCash(t)

	parked, err := parkDestinationCashSecret(l, ids, nipcash.Pubkey(strings.Repeat("a1", 32)))
	if err != nil {
		t.Fatalf("parkDestinationCashSecret(pubkey target) error = %v, want nil", err)
	}
	if got := mustFind(t, l, ids[0]).PendingDestinationCashSecret; got != "" {
		t.Fatalf("a pubkey target parked %q, want nothing parked", got)
	}
	// And the zero value's release is harmless rather than panicking, which is
	// what lets every caller skip branching on the target kind a second time.
	parked.release()
}

// TestAuditD_CLI_ReleaseLeavesAConcurrentParkAlone: two processes can be in
// this window at once (a `transfer` and a `wallet protect`, say). The one that
// finishes first must not clear a park it did not write, which would silently
// discard the other's only copy.
func TestAuditD_CLI_ReleaseLeavesAConcurrentParkAlone(t *testing.T) {
	auditDTempLedger(t)
	l, ids := auditDSeedTwoHeldCash(t)

	mine := nipcash.NewCashTarget()
	parked, err := parkDestinationCashSecret(l, ids, mine)
	if err != nil {
		t.Fatalf("parkDestinationCashSecret() error = %v", err)
	}
	theirs := nipcash.NewCashTarget()
	mustFind(t, l, ids[0]).PendingDestinationCashSecret = theirs.Secret()

	parked.release()

	if got := mustFind(t, l, ids[0]).PendingDestinationCashSecret; got != theirs.Secret() {
		t.Fatalf("release() cleared a park it did not write: got %q, want the other call's %q", got, theirs.Secret())
	}
}

// TestAuditD_CLI_UnreconciledParkIsReportedToTheUser is why the column is worth
// more than a passing test: a secret persisted where no human can see it
// protects nothing. unreconciledDestinationParks is what `wallet show` prints.
func TestAuditD_CLI_UnreconciledParkIsReportedToTheUser(t *testing.T) {
	auditDTempLedger(t)
	l, ids := auditDSeedTwoHeldCash(t)

	if lines := unreconciledDestinationParks(l); len(lines) != 0 {
		t.Fatalf("a clean ledger reported %d unfinished sends, want 0 — a false warning about "+
			"money at risk is its own defect", len(lines))
	}

	target := nipcash.NewCashTarget()
	if _, err := parkDestinationCashSecret(l, ids, target); err != nil {
		t.Fatalf("parkDestinationCashSecret() error = %v", err)
	}
	// As a fresh process would see it after the kill, not as this one left it.
	fresh, err := ledger.Load()
	if err != nil {
		t.Fatalf("ledger.Load() error = %v", err)
	}
	lines := unreconciledDestinationParks(fresh)
	if len(lines) != 1 {
		t.Fatalf("unreconciledDestinationParks() returned %d lines, want 1: %v", len(lines), lines)
	}
	// The secret itself must be in the text. Naming the entry without it would
	// tell a user their money is stuck and give them no way to reach it.
	if !strings.Contains(lines[0], target.Secret()) {
		t.Fatalf("the report does not contain the secret, so it is unactionable: %q", lines[0])
	}
	if !strings.Contains(lines[0], ids[0]) {
		t.Fatalf("the report does not name the entry it is about: %q", lines[0])
	}
}

// TestAuditD_CLI_ParkSurvivesALedgerReloadRoundTrip pins the persistence layer
// itself: a field added to Entry is only as good as its column, and the audit
// trail has a precedent for exactly this going wrong silently (lokihub's
// db_migrate copying 6 of 27 tables and reporting success).
func TestAuditD_CLI_ParkSurvivesALedgerReloadRoundTrip(t *testing.T) {
	auditDTempLedger(t)
	l, ids := auditDSeedTwoHeldCash(t)

	want := nipcash.NewCashTarget().Secret()
	mustFind(t, l, ids[1]).PendingDestinationCashSecret = want
	if err := l.Save(); err != nil {
		t.Fatalf("l.Save() error = %v", err)
	}
	fresh, err := ledger.Load()
	if err != nil {
		t.Fatalf("ledger.Load() error = %v", err)
	}
	got := mustFind(t, fresh, ids[1]).PendingDestinationCashSecret
	if got != want {
		t.Fatalf("PendingDestinationCashSecret did not survive a Save/Load round trip: got %q, want %q", got, want)
	}
	// And it is scoped to its own row rather than smeared across every entry.
	if other := mustFind(t, fresh, ids[0]).PendingDestinationCashSecret; other != "" {
		t.Fatalf("entry %s also came back parked (%q), want only %s parked", ids[0], other, ids[1])
	}
}

func mustFind(t *testing.T, l *ledger.Ledger, id string) *ledger.Entry {
	t.Helper()
	e, ok := l.Find(id)
	if !ok {
		t.Fatalf("l.Find(%q) not found", id)
	}
	return e
}
