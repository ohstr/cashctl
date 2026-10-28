package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/ohstr/cashctl/internal/store"
)

// TrustedMinter is a minter (x-only key) the user trusts for one chain: a Cash
// Zap it signed is checked with its Hub without asking (NIP-CZ rule 8).
type TrustedMinter struct {
	MinterXOnly string `json:"minter"`
	Chain       string `json:"chain"`
}

// TrustedMinters lists trusted_minters.
func TrustedMinters() ([]TrustedMinter, error) {
	db, err := store.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT minter_xonly, chain FROM trusted_minters ORDER BY added_at`)
	if err != nil {
		return nil, fmt.Errorf("cashctl.db: reading trusted minters: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []TrustedMinter
	for rows.Next() {
		var m TrustedMinter
		if err := rows.Scan(&m.MinterXOnly, &m.Chain); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// TrustMinter adds a trusted minter.
func TrustMinter(m TrustedMinter) error {
	return store.WithBusyRetry(func() error {
		db, err := store.Open()
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		_, err = db.Exec(`INSERT OR IGNORE INTO trusted_minters (minter_xonly, chain, added_at) VALUES (?, ?, ?)`,
			strings.ToLower(m.MinterXOnly), m.Chain, time.Now().Unix())
		return err
	})
}

// UntrustMinter removes a trusted minter for chain.
func UntrustMinter(m TrustedMinter) error {
	return store.WithBusyRetry(func() error {
		db, err := store.Open()
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		_, err = db.Exec(`DELETE FROM trusted_minters WHERE minter_xonly = ? AND chain = ?`, strings.ToLower(m.MinterXOnly), m.Chain)
		return err
	})
}
