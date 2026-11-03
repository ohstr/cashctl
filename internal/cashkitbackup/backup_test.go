package cashkitbackup_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ohstr/cashctl/internal/cashkitbackup"
)

// The fixture is a REAL cashkit v3 backup, produced by cashkit's own
// exportBackup over its test harness (see testdata/backup-v3.expected.json for
// the values the writer put in). It is the cross-client check: if Open accepts
// it and the recomputed MAC matches, cashctl's crypto + codec port is byte-for-
// byte faithful to cashkit.
type expected struct {
	Passphrase     string `json:"passphrase"`
	WalletID       string `json:"walletId"`
	IdentityPubkey string `json:"identityPubkey"`
	Cash           struct {
		Token        string `json:"token"`
		Amount       string `json:"amount"`
		CashSecret   string `json:"cashSecret"`
		WalletPubkey string `json:"walletPubkey"`
	} `json:"cash"`
	Pubkey struct {
		Token        string `json:"token"`
		Amount       string `json:"amount"`
		WalletPubkey string `json:"walletPubkey"`
	} `json:"pubkey"`
}

func load(t *testing.T) ([]byte, expected) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "backup-v3.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	expRaw, err := os.ReadFile(filepath.Join("testdata", "backup-v3.expected.json"))
	if err != nil {
		t.Fatalf("read expected: %v", err)
	}
	var exp expected
	if err := json.Unmarshal(expRaw, &exp); err != nil {
		t.Fatalf("parse expected: %v", err)
	}
	return data, exp
}

func TestOpenRealFixture(t *testing.T) {
	data, exp := load(t)

	b, err := cashkitbackup.Open(data, exp.Passphrase)
	if err != nil {
		t.Fatalf("Open a valid backup: %v", err)
	}
	if b.WalletID != exp.WalletID {
		t.Errorf("walletId: got %q want %q", b.WalletID, exp.WalletID)
	}

	holdings, err := b.Holdings()
	if err != nil {
		t.Fatalf("Holdings: %v", err)
	}
	if len(holdings) != 2 {
		t.Fatalf("holdings: got %d want 2", len(holdings))
	}

	var cash, pub *cashkitbackup.Holding
	for i := range holdings {
		switch holdings[i].Mode {
		case "cash":
			cash = &holdings[i]
		case "pubkey":
			pub = &holdings[i]
		}
	}
	if cash == nil || pub == nil {
		t.Fatalf("expected one cash and one pubkey holding; got %+v", holdings)
	}

	if amt, ok := cash.Amount.Uint64(); !ok || amt != 3000 {
		t.Errorf("cash amount: got %v ok=%v want 3000", amt, ok)
	}
	if cash.WalletPubkey != exp.Cash.WalletPubkey {
		t.Errorf("cash walletPubkey: got %q want %q", cash.WalletPubkey, exp.Cash.WalletPubkey)
	}
	if cash.CashSecret == nil {
		t.Fatal("cash holding has no sealed cashSecret")
	}
	secret, err := b.OpenString(*cash.CashSecret)
	if err != nil {
		t.Fatalf("open cashSecret: %v", err)
	}
	if secret != exp.Cash.CashSecret {
		t.Errorf("opened cashSecret: got %q want %q", secret, exp.Cash.CashSecret)
	}

	if amt, ok := pub.Amount.Uint64(); !ok || amt != 5000 {
		t.Errorf("pubkey amount: got %v ok=%v want 5000", amt, ok)
	}
	if pub.CashSecret != nil {
		t.Error("pubkey holding should carry no cashSecret")
	}
}

func TestWrongPassphrase(t *testing.T) {
	data, _ := load(t)
	if _, err := cashkitbackup.Open(data, "not the passphrase"); !errors.Is(err, cashkitbackup.ErrWrongPassphrase) {
		t.Fatalf("want ErrWrongPassphrase, got %v", err)
	}
}

func TestTamperedPlaintextRejected(t *testing.T) {
	data, exp := load(t)
	// Inflate a holding's plaintext amount, leaving every sealed secret untouched.
	tampered := bytes.Replace(data, []byte(`"$bigint":"3000"`), []byte(`"$bigint":"3001"`), 1)
	if bytes.Equal(tampered, data) {
		t.Fatal("fixture did not contain the expected amount token to tamper")
	}
	if _, err := cashkitbackup.Open(tampered, exp.Passphrase); !errors.Is(err, cashkitbackup.ErrTampered) {
		t.Fatalf("want ErrTampered, got %v", err)
	}
}

func TestVersionRefused(t *testing.T) {
	data, exp := load(t)
	older := bytes.Replace(data, []byte(`"version":3`), []byte(`"version":2`), 1)
	if bytes.Equal(older, data) {
		t.Fatal("fixture did not contain the version token")
	}
	if _, err := cashkitbackup.Open(older, exp.Passphrase); !errors.Is(err, cashkitbackup.ErrVersion) {
		t.Fatalf("want ErrVersion, got %v", err)
	}
}

func TestNotABackup(t *testing.T) {
	for _, in := range []string{"not json", `{"format":"other"}`, `{}`} {
		if _, err := cashkitbackup.Open([]byte(in), "x"); !errors.Is(err, cashkitbackup.ErrNotBackup) {
			t.Errorf("input %q: want ErrNotBackup, got %v", in, err)
		}
	}
}
