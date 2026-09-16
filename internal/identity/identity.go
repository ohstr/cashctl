// Package identity manages cashctl's local identity — compatible with, but
// not a copy of, ncli's own vault (see cashctl-plan.md's "Local wallet layer
// (identity + ledger)"). cashctl.db's identity table (a single row, at
// most) stores either a reference to an existing ncli vault entry, or
// cashctl's own independently generated keypair — never both, never a
// plaintext copy of a vault entry's key.
package identity

import (
	"database/sql"
	"errors"
	"fmt"

	ncli "github.com/ohstr/ncli/client"

	"github.com/ohstr/cashctl/internal/store"
)

// Source identifies where Resolve should get the signing key from.
type Source string

const (
	// SourceNcliVault: re-unlock the real ncli vault live on every use —
	// the identity table holds only Npub/Label, never a copied privkey.
	SourceNcliVault Source = "ncli-vault"
	// SourceLocal: cashctl's own independently generated keypair, stored
	// directly (plaintext) in the identity table — same 0600 handling as
	// a seed file (cashctl.db as a whole, see internal/store).
	SourceLocal Source = "cashctl-local"
)

// Stored is the in-memory shape of cashctl.db's identity table.
type Stored struct {
	Source Source
	// Npub/Label: set only for SourceNcliVault — which vault entry to
	// re-resolve at use time.
	Npub  string
	Label string
	// PrivHex: set only for SourceLocal.
	PrivHex string
}

// Exists reports whether an identity has been configured yet.
func Exists() (bool, error) {
	db, err := store.Open()
	if err != nil {
		return false, err
	}
	defer db.Close()

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM identity`).Scan(&n); err != nil {
		return false, fmt.Errorf("cashctl.db: checking identity: %w", err)
	}
	return n > 0, nil
}

// ErrNotConfigured is returned by Load when no identity has been set up
// yet — callers should either check Exists first, or surface this as a
// "run `cashctl init`" usage error.
var ErrNotConfigured = errors.New("no identity configured yet — run `cashctl init` first")

// Load reads the stored identity reference.
func Load() (*Stored, error) {
	db, err := store.Open()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	var s Stored
	err = db.QueryRow(`SELECT source, npub, label, priv_hex FROM identity LIMIT 1`).
		Scan(&s.Source, &s.Npub, &s.Label, &s.PrivHex)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotConfigured
	}
	if err != nil {
		return nil, fmt.Errorf("cashctl.db: reading identity: %w", err)
	}
	return &s, nil
}

// SaveNcliVaultRef records a reference to an existing ncli vault entry —
// never a copied privkey.
func SaveNcliVaultRef(npub, label string) error {
	return save(&Stored{Source: SourceNcliVault, Npub: npub, Label: label})
}

// GenerateAndSaveLocal generates a brand-new keypair via ncli's own
// client.GenerateIdentity (the same generator ncli's own `ncli id` uses —
// just persisted under cashctl's own identity table instead of ncli's
// vault) and stores it directly. Returns the new identity's npub.
func GenerateAndSaveLocal() (npub string, err error) {
	id, err := ncli.GenerateIdentity()
	if err != nil {
		return "", fmt.Errorf("failed to generate identity: %w", err)
	}
	if err := save(&Stored{Source: SourceLocal, PrivHex: id.PrivKeyHex}); err != nil {
		return "", err
	}
	return id.Npub, nil
}

// save replaces cashctl.db's identity table (at most one row) with s, in
// one transaction — a plaintext privkey (SourceLocal) means this table
// needs the same 0600 handling as a seed file, enforced by store.Open on
// the database file as a whole.
func save(s *Stored) error {
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM identity`); err != nil {
		return fmt.Errorf("cashctl.db: clearing identity: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO identity (source, npub, label, priv_hex) VALUES (?, ?, ?, ?)`,
		s.Source, s.Npub, s.Label, s.PrivHex); err != nil {
		return fmt.Errorf("cashctl.db: saving identity: %w", err)
	}
	return tx.Commit()
}

// PasswordPrompt resolves the ncli vault password when Resolve needs to
// unlock it — sourced from NCLI_VAULT_PASSWORD for non-interactive/agentic
// use, or an interactive prompt otherwise, exactly as ncli's own
// keyresolve.ResolveVaultPassword does. Left to the caller (cmd/) rather
// than baked into this package, so this package needs no terminal I/O of
// its own and stays trivially unit-testable.
type PasswordPrompt func() (string, error)

// Resolve returns the raw private key hex for the stored identity — the
// default `--as pubkey:<privkey>` credential everywhere in cashctl unless a
// command overrides it. For SourceNcliVault, this re-unlocks the real
// vault live via promptPassword; promptPassword is never called for
// SourceLocal.
func Resolve(promptPassword PasswordPrompt) (string, error) {
	s, err := Load()
	if err != nil {
		return "", err
	}
	switch s.Source {
	case SourceLocal:
		return s.PrivHex, nil
	case SourceNcliVault:
		entry, found, err := ncli.FindVaultEntry(s.Npub)
		if err != nil {
			return "", fmt.Errorf("failed to look up ncli vault entry: %w", err)
		}
		if !found {
			return "", fmt.Errorf("identity references ncli vault entry %q, but it's no longer in the vault", s.Label)
		}
		password, err := promptPassword()
		if err != nil {
			return "", err
		}
		vaultPrivKeyHex, err := ncli.UnlockVaultIdentity(password)
		if err != nil {
			return "", fmt.Errorf("failed to unlock ncli vault: %w", err)
		}
		return ncli.DecryptVaultEntry(vaultPrivKeyHex, *entry)
	default:
		return "", fmt.Errorf("stored identity has an unknown source %q", s.Source)
	}
}
