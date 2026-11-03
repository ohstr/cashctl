package cashkitimport_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ohstr/cashctl/internal/cashkitbackup"
	"github.com/ohstr/cashctl/internal/cashkitimport"
	"github.com/ohstr/cashctl/internal/ledger"
)

type expected struct {
	Passphrase string `json:"passphrase"`
	WalletID   string `json:"walletId"`
	Cash       struct {
		Amount       string `json:"amount"`
		CashSecret   string `json:"cashSecret"`
		WalletPubkey string `json:"walletPubkey"`
	} `json:"cash"`
}

// fixtureDir is cashkitbackup's testdata — the same real cashkit v3 backup.
func fixtureDir() string { return filepath.Join("..", "cashkitbackup", "testdata") }

func load(t *testing.T) (*cashkitbackup.Backup, expected) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureDir(), "backup-v3.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	expRaw, err := os.ReadFile(filepath.Join(fixtureDir(), "backup-v3.expected.json"))
	if err != nil {
		t.Fatalf("read expected: %v", err)
	}
	var exp expected
	if err := json.Unmarshal(expRaw, &exp); err != nil {
		t.Fatalf("parse expected: %v", err)
	}
	b, err := cashkitbackup.Open(data, exp.Passphrase)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	return b, exp
}

func TestBuildPlan(t *testing.T) {
	b, exp := load(t)

	p, err := cashkitimport.BuildPlan(b)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if p.WalletID != exp.WalletID {
		t.Errorf("walletID: got %q want %q", p.WalletID, exp.WalletID)
	}

	// Exactly the one cash holding becomes an entry; the pubkey holding is
	// reported skipped (identity-bound), never silently dropped.
	if len(p.Entries) != 1 {
		t.Fatalf("entries: got %d want 1 (%+v)", len(p.Entries), p.Skipped)
	}
	if p.Skipped[cashkitimport.SkipPubkey] != 1 {
		t.Errorf("skipped pubkey: got %d want 1 (all skips: %+v)", p.Skipped[cashkitimport.SkipPubkey], p.Skipped)
	}
	if got := p.IdentityBoundSkipped(); got != 1 {
		t.Errorf("IdentityBoundSkipped: got %d want 1", got)
	}

	e := p.Entries[0]
	if e.CashSecret != exp.Cash.CashSecret {
		t.Errorf("cashSecret: got %q want %q", e.CashSecret, exp.Cash.CashSecret)
	}
	if e.AmountMillis == nil || *e.AmountMillis != 3000 {
		t.Errorf("amountMillis: got %v want 3000", e.AmountMillis)
	}
	if p.TotalMillis != 3000 {
		t.Errorf("totalMillis: got %d want 3000", p.TotalMillis)
	}
	if e.WalletPubkey != exp.Cash.WalletPubkey {
		t.Errorf("walletPubkey: got %q want %q", e.WalletPubkey, exp.Cash.WalletPubkey)
	}
	if e.Secret == "" {
		t.Error("entry Secret (NWC connection secret from the token) should be set")
	}
	if e.IdentityRequired == nil || *e.IdentityRequired {
		t.Error("cash-mode entry should have IdentityRequired=false")
	}
	// cashkit auto-protects a received cash gift (re-keys it), so this holding
	// is "protected" and the backup carries the re-keyed secret; the entry must
	// carry that across so cashctl does not wrongly warn it is shared.
	if e.CashProtection != ledger.CashProtected {
		t.Errorf("cashProtection: got %q want %q", e.CashProtection, ledger.CashProtected)
	}
	if !e.Verified {
		t.Error("the fixture's cash holding was verified; the entry should be too")
	}
}
