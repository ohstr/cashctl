package config

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ohstr/cashctl/internal/store"
)

// UnverifiedConfirmation is a connection string whose signature did not
// verify and which the user agreed to use anyway, once, after its online check
// passed. It is keyed by the sha256 of the lowercased string, so the string
// itself (which carries a secret) is not stored here, and a string that
// differs in any byte asks again.
type UnverifiedConfirmation struct {
	HRP, HubIdentity, Reason string
	// Via is how the user confirmed: "prompt" or "flag" (--allow-unverified).
	Via string
}

// connSHA256 is the key of raw in unverified_confirmations.
func connSHA256(raw string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(raw))))
	return hex.EncodeToString(sum[:])
}

// UnverifiedConfirmed reports whether raw was confirmed before.
func UnverifiedConfirmed(raw string) (bool, error) {
	db, err := store.Open()
	if err != nil {
		return false, err
	}
	defer func() { _ = db.Close() }()
	var one int
	err = db.QueryRow(`SELECT 1 FROM unverified_confirmations WHERE conn_sha256 = ?`, connSHA256(raw)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cashctl.db: reading unverified confirmations: %w", err)
	}
	return true, nil
}

// ConfirmUnverified records that the user confirmed raw.
func ConfirmUnverified(raw string, c UnverifiedConfirmation) error {
	return store.WithBusyRetry(func() error {
		db, err := store.Open()
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		_, err = db.Exec(`INSERT OR REPLACE INTO unverified_confirmations (conn_sha256, hrp, hub_identity, reason, confirmed_at, via)
			VALUES (?, ?, ?, ?, ?, ?)`, connSHA256(raw), c.HRP, c.HubIdentity, c.Reason, time.Now().Unix(), c.Via)
		if err != nil {
			return fmt.Errorf("cashctl.db: saving an unverified confirmation: %w", err)
		}
		return nil
	})
}
