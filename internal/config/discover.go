package config

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ohstr/cashctl/internal/store"
)

// The editable discovery relays and the local curator set of `cash discover`
// (NIP-CASH §Fetching Listings, §Curators). None is shipped.

func listColumn(query string) ([]string, error) {
	db, err := store.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("cashctl.db: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func execDiscover(query string, args ...any) error {
	return store.WithBusyRetry(func() error {
		db, err := store.Open()
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		_, err = db.Exec(query, args...)
		return err
	})
}

// DiscoverRelays lists the discovery relays.
func DiscoverRelays() ([]string, error) {
	return listColumn(`SELECT url FROM discover_relays ORDER BY added_at, url`)
}

// AddDiscoverRelay adds a discovery relay.
func AddDiscoverRelay(url string) error {
	return execDiscover(`INSERT OR IGNORE INTO discover_relays (url, added_at) VALUES (?, ?)`, url, time.Now().Unix())
}

// RemoveDiscoverRelay removes a discovery relay.
func RemoveDiscoverRelay(url string) error {
	return execDiscover(`DELETE FROM discover_relays WHERE url = ?`, url)
}

// Curators lists the local curator set (hex pubkeys).
func Curators() ([]string, error) {
	return listColumn(`SELECT pubkey FROM discover_curators ORDER BY added_at, pubkey`)
}

// AddCurator adds a curator.
func AddCurator(pubkey string) error {
	return execDiscover(`INSERT OR IGNORE INTO discover_curators (pubkey, added_at) VALUES (?, ?)`, pubkey, time.Now().Unix())
}

// RemoveCurator removes a curator.
func RemoveCurator(pubkey string) error {
	return execDiscover(`DELETE FROM discover_curators WHERE pubkey = ?`, pubkey)
}

// DiscoverState reads a stored discover snapshot, nil when none.
func DiscoverState(key string) ([]byte, error) {
	db, err := store.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	var v []byte
	err = db.QueryRow(`SELECT value FROM discover_state WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return v, err
}

// PutDiscoverState stores a discover snapshot.
func PutDiscoverState(key string, value []byte) error {
	return execDiscover(`INSERT OR REPLACE INTO discover_state (key, value, updated_at) VALUES (?, ?, ?)`, key, value, time.Now().Unix())
}
