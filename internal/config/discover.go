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

func listColumn(query string, args ...any) ([]string, error) {
	db, err := store.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(query, args...)
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

// Event origins in discover_events: what request first returned an event. A
// trusted request (follows, curators, the nodes and operators they name) wins
// over the global one, which is pruned first.
const (
	OriginTrusted = "trusted"
	OriginGlobal  = "global"
)

// PutDiscoverEvent caches a public event, checked by the caller; an event
// already cached keeps its row, except that trusted replaces global.
func PutDiscoverEvent(id string, kind int, pubkey string, createdAt int64, raw, origin, relay string) error {
	return execDiscover(`INSERT INTO discover_events (id, kind, pubkey, created_at, raw, origin, fetched_at, relay)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''))
		ON CONFLICT(id) DO UPDATE SET origin = CASE WHEN excluded.origin = 'trusted' THEN 'trusted' ELSE discover_events.origin END,
			relay = COALESCE(discover_events.relay, excluded.relay)`,
		id, kind, pubkey, createdAt, raw, origin, time.Now().Unix(), relay)
}

// DiscoverEventRelay is the relay that first served the cached event id, ""
// when unknown.
func DiscoverEventRelay(id string) string {
	rows, err := listColumn(`SELECT COALESCE(relay, '') FROM discover_events WHERE id = ?`, id)
	if err != nil || len(rows) == 0 {
		return ""
	}
	return rows[0]
}

// DiscoverEvents lists the cached events' raw JSON, oldest first.
func DiscoverEvents() ([]string, error) {
	return listColumn(`SELECT raw FROM discover_events ORDER BY created_at, id`)
}

// PruneDiscoverEvents keeps at most max global (unreviewed) events, the newest.
// Trusted events (the user's own, follows', curators' and what they name) are
// never pruned: a flood of listings must not push them out, and their since
// has already moved past them (D-1).
func PruneDiscoverEvents(max int) error {
	return execDiscover(`DELETE FROM discover_events WHERE origin != 'trusted' AND id NOT IN (
		SELECT id FROM discover_events WHERE origin != 'trusted' ORDER BY created_at DESC LIMIT ?)`, max)
}

// OutboxItem is a recommendation or report waiting to be published (kept
// unsigned until it is), or one published.
type OutboxItem struct {
	ID           string `json:"outbox"`
	Kind         int    `json:"kind"`
	Node         string `json:"node"`
	Offer        string `json:"offer,omitempty"`
	Draft        string `json:"-"`
	PublishAfter int64  `json:"publish_after"`
	State        string `json:"state"`
	EventID      string `json:"event_id,omitempty"`
	PublishedTo  string `json:"published_to,omitempty"`
	CreatedAt    int64  `json:"created_at"`
}

// Outbox states.
const (
	OutboxWaiting   = "waiting"
	OutboxPublished = "published"
	OutboxCancelled = "cancelled"
)

// PutOutbox writes an outbox row (insert or replace).
func PutOutbox(o OutboxItem) error {
	if o.CreatedAt == 0 {
		o.CreatedAt = time.Now().Unix()
	}
	return execDiscover(`INSERT OR REPLACE INTO discover_outbox (outbox_id, kind, node, offer, draft, publish_after, state,
		event_id, published_to, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.ID, o.Kind, o.Node, o.Offer, o.Draft, o.PublishAfter, o.State, o.EventID, o.PublishedTo, o.CreatedAt)
}

// Outbox lists the outbox, oldest first.
func Outbox() ([]OutboxItem, error) {
	db, err := store.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT outbox_id, kind, node, COALESCE(offer, ''), draft, publish_after, state,
		COALESCE(event_id, ''), COALESCE(published_to, ''), created_at FROM discover_outbox ORDER BY created_at, outbox_id`)
	if err != nil {
		return nil, fmt.Errorf("cashctl.db: reading the discover outbox: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []OutboxItem
	for rows.Next() {
		var o OutboxItem
		if err := rows.Scan(&o.ID, &o.Kind, &o.Node, &o.Offer, &o.Draft, &o.PublishAfter, &o.State, &o.EventID, &o.PublishedTo, &o.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
