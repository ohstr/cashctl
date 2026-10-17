package config

import (
	"fmt"

	"github.com/ohstr/cashctl/internal/store"
)

// PendingJoin is a circle join's connection key, saved before the request is
// sent (NIP-CW §Pairing Connection: the member generates the key and the Hub
// never sees the secret). It is removed once the join is answered. One still
// here means the Hub never answered, so whether that wallet exists is unknown;
// ConnectionPubkey is what the Hub knows it by.
type PendingJoin struct {
	ConnectionPubkey string `json:"connection_pubkey"`
	// ConnectionSecret is the wallet's client secret: json:"-" like
	// Connection.Value, so listing pending joins never prints it.
	ConnectionSecret string `json:"-"`
	HubPubkey        string `json:"hub_pubkey"`
	HubLabel         string `json:"hub_label,omitempty"`
	At               string `json:"at"`
}

// SavePendingJoin records p, durably, before its request is sent.
func SavePendingJoin(p PendingJoin) error {
	return store.WithBusyRetry(func() error {
		db, err := store.Open()
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		_, err = db.Exec(`INSERT INTO pending_joins (connection_pubkey, connection_secret, hub_pubkey, hub_label, at)
			VALUES (?, ?, ?, ?, ?)`, p.ConnectionPubkey, p.ConnectionSecret, p.HubPubkey, p.HubLabel, p.At)
		if err != nil {
			return fmt.Errorf("cashctl.db: saving the join's connection key: %w", err)
		}
		return nil
	})
}

// DeletePendingJoin forgets the pending join with this connection pubkey.
func DeletePendingJoin(connectionPubkey string) error {
	return store.WithBusyRetry(func() error {
		db, err := store.Open()
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		if _, err := db.Exec(`DELETE FROM pending_joins WHERE connection_pubkey = ?`, connectionPubkey); err != nil {
			return fmt.Errorf("cashctl.db: removing a pending join: %w", err)
		}
		return nil
	})
}

// PendingJoins lists the joins that never got an answer, oldest first.
func PendingJoins() ([]PendingJoin, error) {
	db, err := store.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT connection_pubkey, connection_secret, hub_pubkey, hub_label, at FROM pending_joins ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("cashctl.db: reading pending joins: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []PendingJoin{}
	for rows.Next() {
		var p PendingJoin
		if err := rows.Scan(&p.ConnectionPubkey, &p.ConnectionSecret, &p.HubPubkey, &p.HubLabel, &p.At); err != nil {
			return nil, fmt.Errorf("cashctl.db: reading pending joins: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
