// Package cashkitbackup reads a cashkit version-3 wallet backup (the format
// cashkit documents in its docs/design.md §10): one passphrase-encrypted JSON
// document of every wallet collection, authenticated as a whole by a MAC.
//
// cashkit and cashctl are sibling Cash wallets (a cashkit Holding is a cashctl
// ledger.Entry), so a cashkit user can move to cashctl by restoring a backup
// here. This package is the trust boundary of that move: it refuses a file that
// is not an intact, untampered v3 backup, and only then yields the plaintext
// secrets an import needs. It performs no cashctl state change itself.
//
// The cross-client contract (keep in lockstep with cashkit docs/design.md §10):
//   - key   = scrypt(NFKC(passphrase), salt, N=2^logN, r, p, dkLen=32), params from kdf.
//   - seal  = base64( 24-byte XChaCha20-Poly1305 nonce ‖ ciphertext ), AAD "cashkit/backup/v1".
//     A sealed secret decrypts to the field's PLAINTEXT bytes.
//   - check = sealed "cashkit backup key check": a right passphrase opens it.
//   - mac   = sealed SHA-256 of the collections' canonical bytes (the row codec's
//     compact JSON, keys in written order, bigint as {"$bigint":"<decimal>"}).
//     Any change to a plaintext field — not only a damaged secret — is caught.
package cashkitbackup

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/scrypt"
	"golang.org/x/text/unicode/norm"
)

// Format and Version are the only backup this reader accepts.
const (
	Format  = "cashkit-backup"
	Version = 3
)

var aad = []byte("cashkit/backup/v1")

const checkText = "cashkit backup key check"

// Sentinel errors, so cashctl's command layer can map each to its exit code
// (wrong passphrase → auth, tampered/foreign → invalid_input, …).
var (
	// ErrNotBackup: the input is not a cashkit backup (not JSON, wrong format tag, unusable kdf).
	ErrNotBackup = errors.New("this is not a cashkit backup")
	// ErrVersion: a cashkit backup of a version this reader does not read (only v3).
	ErrVersion = errors.New("unsupported cashkit backup version")
	// ErrWrongPassphrase: the passphrase did not open the backup's key check.
	ErrWrongPassphrase = errors.New("wrong passphrase for this backup")
	// ErrTampered: the backup opened but failed its whole-file integrity check.
	ErrTampered = errors.New("this backup failed its integrity check: it was changed after it was written")
)

type kdfParams struct {
	Name string `json:"name"`
	LogN int    `json:"logN"`
	R    int    `json:"r"`
	P    int    `json:"p"`
	Salt string `json:"salt"`
}

type rawFile struct {
	Format      string          `json:"format"`
	Version     int             `json:"version"`
	WalletID    string          `json:"walletId"`
	CreatedAt   int64           `json:"createdAt"`
	KDF         kdfParams       `json:"kdf"`
	Check       string          `json:"check"`
	Mac         string          `json:"mac"`
	Collections json.RawMessage `json:"collections"`
}

// Backup is a verified cashkit v3 backup: its integrity has been checked and
// its passphrase confirmed. Read its rows with Holdings; recover a sealed
// field's plaintext with Open.
type Backup struct {
	WalletID  string
	CreatedAt int64

	key         []byte
	collections []byte // canonical (compacted) bytes the MAC authenticated
}

// Open parses data, derives the key from passphrase, and verifies the backup
// end to end: format and version, the passphrase (check), then the whole-file
// MAC. It returns an error (one of the sentinels above) unless the file is an
// intact v3 cashkit backup and the passphrase is correct.
func Open(data []byte, passphrase string) (*Backup, error) {
	var f rawFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%w: it is not JSON", ErrNotBackup)
	}
	if f.Format != Format {
		return nil, ErrNotBackup
	}
	if f.Version > Version {
		return nil, fmt.Errorf("%w: it was written by a newer cashkit (version %d); upgrade to import it", ErrVersion, f.Version)
	}
	if f.Version < Version {
		return nil, fmt.Errorf("%w: it was written by an older cashkit (version %d), whose wallet format this does not read", ErrVersion, f.Version)
	}
	if f.KDF.Name != "scrypt" || f.KDF.Salt == "" {
		return nil, fmt.Errorf("%w: it has no usable key parameters", ErrNotBackup)
	}
	salt, err := base64.StdEncoding.DecodeString(f.KDF.Salt)
	if err != nil {
		return nil, fmt.Errorf("%w: its key salt is not valid base64", ErrNotBackup)
	}
	key, err := deriveKey(passphrase, salt, f.KDF.LogN, f.KDF.R, f.KDF.P)
	if err != nil {
		return nil, err
	}

	// Passphrase check: a value sealed under the key. Opening it tells a
	// correct passphrase (and an intact header) from a wrong one.
	chk, err := unseal(key, f.Check)
	if err != nil || !bytes.Equal(chk, []byte(checkText)) {
		return nil, ErrWrongPassphrase
	}

	// Whole-file integrity: the MAC is the sealed SHA-256 of the collections'
	// canonical bytes. Recompute over the file's own collections (compacted —
	// strip only insignificant whitespace, never reorder keys) and compare.
	if f.Mac == "" {
		return nil, fmt.Errorf("%w: it carries no integrity tag, so it cannot be trusted", ErrNotBackup)
	}
	macPlain, err := unseal(key, f.Mac)
	if err != nil {
		return nil, ErrWrongPassphrase
	}
	var canon bytes.Buffer
	if err := json.Compact(&canon, f.Collections); err != nil {
		return nil, fmt.Errorf("%w: its collections are not JSON", ErrNotBackup)
	}
	sum := sha256.Sum256(canon.Bytes())
	if subtle.ConstantTimeCompare(macPlain, sum[:]) != 1 {
		return nil, ErrTampered
	}

	return &Backup{WalletID: f.WalletID, CreatedAt: f.CreatedAt, key: key, collections: canon.Bytes()}, nil
}

func deriveKey(passphrase string, salt []byte, logN, r, p int) ([]byte, error) {
	if logN < 1 || logN > 22 {
		return nil, fmt.Errorf("%w: its key cost (logN %d) is out of range", ErrNotBackup, logN)
	}
	if r <= 0 || p <= 0 {
		return nil, fmt.Errorf("%w: its key parameters are invalid", ErrNotBackup)
	}
	// NFKC so a passphrase typed on a different keyboard/OS derives the same key.
	key, err := scrypt.Key([]byte(norm.NFKC.String(passphrase)), salt, 1<<uint(logN), r, p, 32)
	if err != nil {
		return nil, fmt.Errorf("%w: its key cannot be derived (%v)", ErrNotBackup, err)
	}
	return key, nil
}

// unseal opens a base64( 24-byte nonce ‖ ciphertext ) value sealed under key.
func unseal(key []byte, sealedB64 string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(sealedB64)
	if err != nil {
		return nil, err
	}
	if len(raw) < chacha20poly1305.NonceSizeX+chacha20poly1305.Overhead {
		return nil, errors.New("sealed value too short")
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	nonce := raw[:chacha20poly1305.NonceSizeX]
	ct := raw[chacha20poly1305.NonceSizeX:]
	return aead.Open(nil, nonce, ct, aad)
}

// bigint decodes a cashkit amount: a JSON number or {"$bigint":"<decimal>"}.
type bigint struct {
	Set   bool
	Value *big.Int
}

func (b *bigint) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if string(data) == "null" {
		return nil
	}
	if len(data) > 0 && data[0] == '{' {
		var w struct {
			Bigint string `json:"$bigint"`
		}
		if err := json.Unmarshal(data, &w); err != nil {
			return err
		}
		z, ok := new(big.Int).SetString(w.Bigint, 10)
		if !ok {
			return fmt.Errorf("invalid $bigint %q", w.Bigint)
		}
		b.Value, b.Set = z, true
		return nil
	}
	z, ok := new(big.Int).SetString(string(data), 10)
	if !ok {
		return fmt.Errorf("invalid number %s", data)
	}
	b.Value, b.Set = z, true
	return nil
}

// Uint64 reports the amount as a uint64; ok is false when unset or out of range.
func (b bigint) Uint64() (v uint64, ok bool) {
	if !b.Set || b.Value == nil || b.Value.Sign() < 0 || !b.Value.IsUint64() {
		return 0, false
	}
	return b.Value.Uint64(), true
}

// Sealed is a backup's enveloped secret: {"sealed": "<base64 nonce‖ciphertext>"}.
type Sealed struct {
	Sealed string `json:"sealed"`
}

// ConnectionKeyBinding mirrors cashkit's connection-key slice identity.
type ConnectionKeyBinding struct {
	Platform      string `json:"platform"`
	ExternalID    string `json:"externalId"`
	AttestationID string `json:"attestationId"`
	IaPubkey      string `json:"iaPubkey"`
}

// Holding is one bill in the backup (cashkit's Holding; secrets still sealed).
type Holding struct {
	ID            string                `json:"id"`
	Token         string                `json:"token"`
	WalletPubkey  string                `json:"walletPubkey"`
	MinterPubkey  string                `json:"minterPubkey"`
	MinterXonly   string                `json:"minterXonly"`
	HubGroup      string                `json:"hubGroup"`
	RelayUrls     []string              `json:"relayUrls"`
	Amount        bigint                `json:"amount"`
	ExpiresAt     *int64                `json:"expiresAt"`
	Verified      bool                  `json:"verified"`
	Status        string                `json:"status"`
	Mode          string                `json:"mode"`
	Protection    string                `json:"protection"`
	CashSecret    *Sealed               `json:"cashSecret"`
	ThrowawayKey  *Sealed               `json:"throwawayKey"`
	ConnectionKey *ConnectionKeyBinding `json:"connectionKey"`
}

// Holdings returns every holding in the backup, secrets still sealed.
func (b *Backup) Holdings() ([]Holding, error) {
	var c struct {
		Holdings []Holding `json:"holdings"`
	}
	if err := json.Unmarshal(b.collections, &c); err != nil {
		return nil, fmt.Errorf("backup: its holdings are malformed: %w", err)
	}
	return c.Holdings, nil
}

// OpenString recovers a sealed field's plaintext as a string (a cashkit secret
// — a cash secret, throwaway key, … — is stored as its text form). It returns
// an error if the field is damaged (it was authenticated by the MAC, so this
// should not happen on a verified backup, but the key could still be wrong for
// a field sealed differently).
func (b *Backup) OpenString(s Sealed) (string, error) {
	plain, err := unseal(b.key, s.Sealed)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
