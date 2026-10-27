package ledger

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ohstr/cashctl/internal/store"
)

// Order states beyond NIP-CO's own (open, paid, minted, expired, closed).
const (
	// OrderPlacing is an order written ahead whose reply has not been read.
	OrderPlacing = "placing"
	// OrderEndedUnpaid is an order past pay_by that never showed progress.
	OrderEndedUnpaid = "ended_unpaid"
)

// Order is one cash_order (NIP-CO) placed from here: its notify, payment and
// recipient, and the order as the Hub last stated it. The buyer key or cash
// secret lives with its call. Notify is never printed.
type Order struct {
	OrderID        string   `json:"order"`
	CallID         string   `json:"call"`
	Notify         string   `json:"-"`
	Payment        string   `json:"payment"`
	AmountMillis   uint64   `json:"amount_millis"`
	RecipientKind  string   `json:"recipient_kind"`
	Recipient      string   `json:"recipient"`
	HubIdentity    string   `json:"hub_identity"`
	Chain          string   `json:"chain"`
	Network        string   `json:"network"`
	ConnVerified   bool     `json:"hub_verified"`
	State          string   `json:"state"`
	SawProgress    bool     `json:"-"`
	TermsChanged   bool     `json:"terms_changed,omitempty"`
	PayBy          int64    `json:"pay_by,omitempty"`
	OrderJSON      string   `json:"-"`
	AcceptedRelays []string `json:"-"`
	CreatedAt      int64    `json:"created_at"`
	CheckedAt      int64    `json:"checked_at,omitempty"`
	FinishedAt     int64    `json:"finished_at,omitempty"`
}

// execer is a *sql.DB or a *sql.Tx.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func putOrder(db execer, o Order) error {
	if o.CreatedAt == 0 {
		o.CreatedAt = time.Now().Unix()
	}
	relays, _ := json.Marshal(o.AcceptedRelays)
	_, err := db.Exec(`INSERT OR REPLACE INTO orders (order_id, call_id, notify, payment, amount_millis, recipient_kind,
		recipient, hub_identity, chain, network, conn_verified, state, saw_progress, terms_changed, pay_by, order_json,
		accepted_relays, created_at, checked_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.OrderID, o.CallID, o.Notify, o.Payment, o.AmountMillis, o.RecipientKind, o.Recipient, o.HubIdentity, o.Chain,
		o.Network, o.ConnVerified, o.State, o.SawProgress, o.TermsChanged, nullInt(o.PayBy), orderNullString(o.OrderJSON),
		string(relays), o.CreatedAt, nullInt(o.CheckedAt), nullInt(o.FinishedAt))
	if err != nil {
		return fmt.Errorf("cashctl.db: saving order %s: %w", o.OrderID, err)
	}
	return nil
}

// PutOrder writes o (insert or replace) on its own.
func PutOrder(o Order) error {
	return store.WithBusyRetry(func() error {
		db, err := store.Open()
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		return putOrder(db, o)
	})
}

// StageOrder has the next Save write o in the same transaction as the calls
// and entries, so an order and the call that places it are written together.
func (l *Ledger) StageOrder(o Order) {
	l.stagedOrders = append(l.stagedOrders, o)
}

// DropStagedOrders forgets orders staged and not yet saved.
func (l *Ledger) DropStagedOrders() { l.stagedOrders = nil }

// DeleteOrder removes an order row.
func DeleteOrder(id string) error {
	return store.WithBusyRetry(func() error {
		db, err := store.Open()
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		_, err = db.Exec(`DELETE FROM orders WHERE order_id = ?`, id)
		return err
	})
}

func nullInt(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: v != 0} }

const orderColumns = `order_id, call_id, notify, payment, amount_millis, recipient_kind, recipient, hub_identity, chain,
	network, conn_verified, state, saw_progress, terms_changed, pay_by, order_json, accepted_relays, created_at, checked_at, finished_at`

func scanOrder(row interface{ Scan(...any) error }) (Order, error) {
	var o Order
	var network, orderJSON, relays sql.NullString
	var payBy, checked, finished sql.NullInt64
	err := row.Scan(&o.OrderID, &o.CallID, &o.Notify, &o.Payment, &o.AmountMillis, &o.RecipientKind, &o.Recipient,
		&o.HubIdentity, &o.Chain, &network, &o.ConnVerified, &o.State, &o.SawProgress, &o.TermsChanged, &payBy,
		&orderJSON, &relays, &o.CreatedAt, &checked, &finished)
	o.Network, o.OrderJSON = network.String, orderJSON.String
	o.PayBy, o.CheckedAt, o.FinishedAt = payBy.Int64, checked.Int64, finished.Int64
	if relays.String != "" {
		_ = json.Unmarshal([]byte(relays.String), &o.AcceptedRelays)
	}
	return o, err
}

// FindOrder reads an order by its id.
func FindOrder(id string) (Order, bool, error) {
	return findOrderBy(`order_id = ?`, id)
}

// OrderForCall reads the order a call belongs to.
func OrderForCall(callID string) (Order, bool, error) {
	return findOrderBy(`call_id = ?`, callID)
}

func findOrderBy(where, arg string) (Order, bool, error) {
	db, err := store.Open()
	if err != nil {
		return Order{}, false, err
	}
	defer func() { _ = db.Close() }()
	o, err := scanOrder(db.QueryRow(`SELECT `+orderColumns+` FROM orders WHERE `+where, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return Order{}, false, nil
	}
	if err != nil {
		return Order{}, false, fmt.Errorf("cashctl.db: reading an order: %w", err)
	}
	return o, true, nil
}

// Orders lists every order, oldest first.
func Orders() ([]Order, error) {
	db, err := store.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT ` + orderColumns + ` FROM orders ORDER BY created_at, rowid`)
	if err != nil {
		return nil, fmt.Errorf("cashctl.db: reading orders: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Order{}
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// Finished reports an order cashctl stops reading on its own.
func (o Order) Finished() bool {
	return o.FinishedAt != 0
}

func orderNullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
