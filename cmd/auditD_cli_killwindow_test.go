package cmd

// Audit D, CLI / local-state hardening role, surface 3 item 5: kill-window fund
// safety.
//
// The brief named cmd/receive_secure.go as the place that deliberately
// implements write-ahead persistence of a locally-generated cash secret
// (PendingCashSecret saved BEFORE the wire call) and asked whether every money
// path has that property or only those two. It does not.
//
// A cash-mode target's secret is generated entirely client-side — NIP-CASH
// §Cash-Mode Slices: only a one-way commitment ever crosses the wire, the Hub
// never learns or returns the preimage. So for `transfer --to cash` and
// `consolidate --to cash`, between the moment the request leaves cashctl and the
// moment ledger.Save() lands, the secret that controls the destination bill
// exists in exactly one place: this process's heap. If the process dies in that
// window after the Hub has committed, the bill is real, funded, and permanently
// unspendable — there is no protocol-level way to recover the preimage, and no
// ledger row for anything to reconcile against.
//
// protectRekeyOnly/protectCashReceipt solve exactly this, and their own doc
// comments say so in as many words ("there is no reason to let placing the call
// be the difference between 'the new secret exists on disk' and 'the new secret
// exists only in this process's own memory, gone the instant it dies'").
// protectCashReceipt even solves the harder sub-case these two paths have — a
// secret for a wallet that does not exist yet — by parking it on the SOURCE
// entry's PendingCashSecret. The mechanism is built, documented and used; it is
// simply not wired into the two paths below.
//
// FIXED 2026-10-01. parkDestinationCashSecret (cmd/cash_writeahead.go) wires
// that discipline into all three sites that reach a cash-mode destination —
// runCashTransfer, transferWithAutoConsolidate and doCashConsolidate — against
// a dedicated Entry.PendingDestinationCashSecret rather than
// PendingCashSecret, because that field means "a candidate for THIS row's own
// credential" and is PROMOTED onto CashSecret as one by spendCashEntry and
// retryWithPendingSecrets; a different bill's secret is not that.
//
// So the assertion below is inverted from the one this file shipped with: it
// now requires the secret to be on disk at the instant the request is in
// flight, which is the property the fix buys. The probe either side of it is
// kept exactly as it was — it is what made the original finding credible, and
// it is equally what would catch the fix regressing.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nipcash"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/store"
)

func auditDTempLedger(t *testing.T) {
	t.Helper()
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
}

// auditDLedgerBytesContain reports whether the raw cashctl.db file on disk
// contains needle anywhere in it. Deliberately the whole file rather than a
// targeted SELECT: the question is "could a process that restarts from scratch
// find this value anywhere in local state", and a byte scan cannot be fooled by
// looking in the wrong column.
func auditDLedgerBytesContain(t *testing.T, needle string) bool {
	t.Helper()
	path, err := store.Path()
	if err != nil {
		t.Fatalf("store.Path() error = %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false
		}
		t.Fatalf("reading %s: %v", filepath.Base(path), err)
	}
	return strings.Contains(string(b), needle)
}

// TestAuditD_CLI_ConsolidateToCash_SecretIsWrittenAheadOfWireCall is the
// regression test for D-CLI-1.
//
// attemptCashConsolidateFn is the package-var seam the existing suite already
// uses to stand in for the network. Inside it, the request is — by construction
// — at the Hub. That is the exact instant a SIGINT, a closed terminal or a power
// loss destroys the money, so that is where this test looks at the disk.
func TestAuditD_CLI_ConsolidateToCash_SecretIsWrittenAheadOfWireCall(t *testing.T) {
	auditDTempLedger(t)

	l, err := ledger.Load()
	if err != nil {
		t.Fatalf("ledger.Load() error = %v", err)
	}
	for _, tok := range []string{"lokicash1src-a", "lokicash1src-b"} {
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
	ids := []string{l.Entries[0].ID, l.Entries[1].ID}

	// The cash target, and therefore the only copy of the destination secret,
	// is created here — exactly as credential.ParseTarget("cash") creates it
	// for the real command, before any prompt and before any wire call.
	target := nipcash.NewCashTarget()
	secret := target.Secret()
	if secret == "" {
		t.Fatal("nipcash.NewCashTarget() produced an empty secret; this test can prove nothing")
	}

	var onDiskAtWireTime bool
	var pendingAtWireTime []string
	withFakeAttemptCashConsolidate(t, func(dialToken string, sources []nipcash.Source, tgt nipcash.Target) (*nipcash.CashConsolidateResult, error) {
		// We are now "at the Hub": the request has been placed, the Hub is
		// about to commit it, and the reply has not come back.
		onDiskAtWireTime = auditDLedgerBytesContain(t, secret)

		// Second, independent probe: a fresh process's view of local state.
		fresh, loadErr := ledger.Load()
		if loadErr != nil {
			t.Fatalf("in-flight ledger.Load() error = %v", loadErr)
		}
		for _, e := range fresh.Entries {
			if e.PendingCashSecret != "" {
				pendingAtWireTime = append(pendingAtWireTime, e.ID+" PendingCashSecret="+e.PendingCashSecret)
			}
			if e.PendingDestinationCashSecret != "" {
				pendingAtWireTime = append(pendingAtWireTime, e.ID+" PendingDestinationCashSecret="+e.PendingDestinationCashSecret)
			}
			if e.CashSecret == secret {
				pendingAtWireTime = append(pendingAtWireTime, e.ID+" holds the destination secret as CashSecret")
			}
		}
		return &nipcash.CashConsolidateResult{
			AmountMillis: 2000, NewWalletToken: "lokicash1merged", NewWalletPubkey: "merged-pub",
		}, nil
	})

	newEntry, _, err := doCashConsolidate(&cobra.Command{}, l, []string{"lokicash1src-a"}, nil, ids, target, true)
	if err != nil {
		t.Fatalf("doCashConsolidate() error = %v", err)
	}

	// Sanity: the secret really is the one thing that makes the result
	// spendable, and the result really does carry it afterwards. Without this,
	// the assertion below could pass for the uninteresting reason that the
	// path never handles the secret at all.
	if newEntry == nil || newEntry.CashSecret != secret {
		t.Fatalf("newEntry.CashSecret = %q, want the target's own secret %q — "+
			"this path is supposed to be the only holder of it", newEntryCashSecret(newEntry), secret)
	}

	if !onDiskAtWireTime {
		t.Fatal("D-CLI-1 HAS REGRESSED: at the instant the cash_consolidate request was in " +
			"flight with the Hub, the destination bill's cash secret was nowhere in " +
			"cashctl.db. A kill in that window (Ctrl-C, closed terminal, OOM, power loss) " +
			"destroys the only copy of it, and the resulting bill is funded and " +
			"permanently unspendable by anyone.")
	}
	// Not just "some bytes matching the secret are in the file" — the specific
	// field, on a specific row, as a fresh process would actually read it.
	// Without this the byte scan above could be satisfied by the secret landing
	// anywhere at all, including somewhere no code ever looks.
	var parkedOn []string
	for _, line := range pendingAtWireTime {
		if strings.Contains(line, secret) {
			parkedOn = append(parkedOn, line)
		}
	}
	if len(parkedOn) == 0 {
		t.Fatalf("the secret is in cashctl.db's bytes but no reloaded entry exposes it as a "+
			"destination park — a fresh process could not find it where the recovery path "+
			"looks. in-flight rows carrying anything: %v", pendingAtWireTime)
	}
	t.Logf("D-CLI-1 fixed: the destination secret is on disk, and readable as a park, while "+
		"the cash_consolidate request is still in flight: %v", parkedOn)

	// And the park is cleared once the outcome IS recorded, so it never
	// accumulates on a successful send — the other half of the contract, and
	// the half a careless fix gets wrong by parking and never releasing.
	if err := l.Save(); err != nil {
		t.Fatalf("post-call l.Save() error = %v", err)
	}
	reloaded, err := ledger.Load()
	if err != nil {
		t.Fatalf("post-call ledger.Load() error = %v", err)
	}
	for _, e := range reloaded.Entries {
		if e.PendingDestinationCashSecret != "" {
			t.Fatalf("entry %s still carries a destination park (%s) after the reply was "+
				"recorded — a successful send must release it, or `wallet show` warns about "+
				"money that is not actually at risk", e.ID, e.PendingDestinationCashSecret)
		}
	}
	// The secret itself must still be on disk, now as the merged entry's own
	// spending credential rather than a park.
	if !auditDLedgerBytesContain(t, secret) {
		t.Fatal("the secret left the database entirely when the park was released — the " +
			"release cleared the wrong thing")
	}
	t.Log("…and the park is released once the outcome is recorded, with the secret now held " +
		"as the merged entry's own CashSecret.")
}

func newEntryCashSecret(e *ledger.Entry) string {
	if e == nil {
		return "<nil entry>"
	}
	return e.CashSecret
}

// TestAuditD_CLI_NoSignalHandlerNarrowsTheKillWindow records the aggravating
// factor for D-CLI-1: cashctl installs no SIGINT/SIGTERM handler anywhere, so
// Ctrl-C during the "Consolidating…"/"Transferring…" spinner is an immediate,
// uninterruptible process death rather than something that could flush the
// ledger first. That turns a rare crash into an ordinary user action: the
// spinner is on screen for the whole duration of the wire call, which is
// exactly the moment an impatient user reaches for Ctrl-C.
//
// A source-structure test, with the same honest limit as auditC's AST test: it
// proves no handler is registered, not that registering one would be
// sufficient. Write-ahead persistence is the real fix; a handler alone would
// still lose a SIGKILL or a power loss.
func TestAuditD_CLI_NoSignalHandlerNarrowsTheKillWindow(t *testing.T) {
	roots := []string{".", "../internal", ".."}
	var hits []string
	seen := map[string]bool{}
	for _, root := range roots {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			abs, _ := filepath.Abs(path)
			if seen[abs] {
				return nil
			}
			seen[abs] = true
			b, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil
			}
			src := string(b)
			for _, marker := range []string{"signal.Notify", "signal.NotifyContext", "os.Interrupt", "syscall.SIGTERM"} {
				if strings.Contains(src, marker) {
					hits = append(hits, path+": "+marker)
				}
			}
			return nil
		})
	}
	if len(hits) > 0 {
		t.Fatalf("UNEXPECTED (aggravating factor would be refuted): signal handling found: %v", hits)
	}
	t.Log("cashctl registers no SIGINT/SIGTERM handler in any non-test source file, so " +
		"Ctrl-C during the in-flight window of D-CLI-1 is an immediate kill.")
}
