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
	defer func() { _ = db.Close() }()

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
	defer func() { _ = db.Close() }()

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

// ErrAlreadyConfigured is returned by SaveNcliVaultRef/GenerateAndSaveLocal
// when another process claimed the identity first — see claim.
var ErrAlreadyConfigured = errors.New("an identity was already configured by another cashctl process")

// SaveNcliVaultRef records a reference to an existing ncli vault entry —
// never a copied privkey. ErrAlreadyConfigured if an identity already exists.
func SaveNcliVaultRef(npub, label string) error {
	return claim(&Stored{Source: SourceNcliVault, Npub: npub, Label: label})
}

// GenerateAndSaveLocal generates a brand-new keypair via ncli's own
// client.GenerateIdentity (the same generator ncli's own `ncli id` uses —
// just persisted under cashctl's own identity table instead of ncli's
// vault) and stores it directly. Returns the new identity's npub, or
// ErrAlreadyConfigured if another process claimed the identity first (the
// key just generated is then discarded, never used or shown).
func GenerateAndSaveLocal() (npub string, err error) {
	id, err := ncli.GenerateIdentity()
	if err != nil {
		return "", fmt.Errorf("failed to generate identity: %w", err)
	}
	if err := claim(&Stored{Source: SourceLocal, PrivHex: id.PrivKeyHex}); err != nil {
		return "", err
	}
	return id.Npub, nil
}

// claim stores s as cashctl's identity ONLY if none exists yet, atomically
// (a single INSERT ... WHERE NOT EXISTS under SQLite's write lock),
// returning ErrAlreadyConfigured when it lost. It used to replace the
// table's row unconditionally (DELETE + INSERT), so N processes running a
// first-time `init` at once each generated their own key, each "succeeded"
// and printed its own npub, and only the last write survived — every
// earlier caller had been told about an identity that no longer existed,
// and funds sent to it would have been unspendable. A plaintext privkey
// (SourceLocal) means this table needs the same 0600 handling as a seed
// file, enforced by store.Open on the database file as a whole.
func claim(s *Stored) error {
	return store.WithBusyRetry(func() error {
		db, err := store.Open()
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()

		res, err := db.Exec(`INSERT INTO identity (source, npub, label, priv_hex)
			SELECT ?, ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM identity)`,
			s.Source, s.Npub, s.Label, s.PrivHex)
		if err != nil {
			return fmt.Errorf("cashctl.db: saving identity: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return ErrAlreadyConfigured
		}
		return nil
	})
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
