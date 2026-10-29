package ledger

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ohstr/cashctl/internal/store"
)

// Withdrawal states beyond NIP-CASH's payout states (queued, broadcast,
// confirmed, final, cancelled).
const (
	// WithdrawSending is a withdrawal written ahead whose answer has not been read.
	WithdrawSending = "sending"
	// WithdrawNotApplied is a withdrawal the Hub refused: the bill is still yours.
	WithdrawNotApplied = "not_applied"
	// WithdrawBillGone is a withdrawal the Hub answered with the bill's
	// tombstone: nothing was withdrawn, and the bill was already spent.
	WithdrawBillGone = "bill_gone"
)

// Withdrawal is one cash_withdraw placed from here, with the quote the user
// confirmed and the payout as the Hub last stated it. Notify, the quote event
// and the receipt are never printed outside their own commands.
type Withdrawal struct {
	WithdrawalID, CallID, SourceEntryID, WalletPubkey, HubIdentity string
	Address, QuoteJSON, QuoteID                                    string
	AmountMillis, NetworkFeeMillis, HubFeeMillis                   uint64
	FeeMillis, PayoutMillis                                        uint64
	SentBy, HoldsUntil, SendBefore                                 int64
	Notify                                                         string
	AcceptedRelays                                                 []string
	State, PayoutJSON, RemintEntryID                               string
	QuoteExpiredSeen                                               bool
	// ReceiptInvalidSeen counts the answers in a row (the reply, then lookups
	// done) whose receipt did not verify; ReceiptInvalidLimit of them make the
	// call stuck, looked up only hourly.
	ReceiptInvalidSeen                 int
	ConfirmedAt, CheckedAt, FinishedAt int64
	// Recovered is a withdrawal found through a cash_status
	// withdraw_request_id with no local record: never re-placed, its address
	// not confirmed on this device.
	Recovered bool
	// ReceivedAt is set, never stored, by the save that records a final
	// payout: the address it was received at, printed that once.
	ReceivedAt string
}

// ReceiptInvalidLimit is how many answers in a row without a valid receipt
// leave a withdrawal stuck (plan WP-20 step 6).
const ReceiptInvalidLimit = 2

// DeviceRecovered is the device of a recovered withdrawal's call: never this
// device, so it is looked up and never re-placed.
const DeviceRecovered = "recovered"

func nz(v int64) sql.NullInt64   { return sql.NullInt64{Int64: v, Valid: v != 0} }
func ns(v string) sql.NullString { return sql.NullString{String: v, Valid: v != ""} }

// PutWithdrawal writes w (insert or replace).
func PutWithdrawal(w Withdrawal) error {
	return store.WithBusyRetry(func() error {
		db, err := store.Open()
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		return putWithdrawal(db, w)
	})
}

// StageWithdrawal has the next Save write w in the same transaction as the
// calls and entries, so a withdrawal and its call are written together.
func (l *Ledger) StageWithdrawal(w Withdrawal) {
	l.stagedWithdrawals = append(l.stagedWithdrawals, w)
}

func putWithdrawal(db execer, w Withdrawal) error {
	relays, _ := json.Marshal(w.AcceptedRelays)
	_, err := db.Exec(`INSERT OR REPLACE INTO withdrawals (withdrawal_id, call_id, source_entry_id, wallet_pubkey,
		hub_identity, address, quote_json, quote_id, amount_millis, network_fee_millis, hub_fee_millis, fee_millis,
		payout_millis, sent_by, holds_until, send_before, notify, accepted_relays, state, payout_json, remint_entry_id,
		quote_expired_seen, confirmed_at, checked_at, finished_at, recovered, receipt_invalid_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		w.WithdrawalID, w.CallID, w.SourceEntryID, w.WalletPubkey, w.HubIdentity, ns(w.Address), ns(w.QuoteJSON),
		ns(w.QuoteID), w.AmountMillis, w.NetworkFeeMillis, w.HubFeeMillis, w.FeeMillis, w.PayoutMillis, w.SentBy,
		nz(w.HoldsUntil), nz(w.SendBefore), w.Notify, string(relays), w.State, ns(w.PayoutJSON), ns(w.RemintEntryID),
		w.QuoteExpiredSeen, nz(w.ConfirmedAt), nz(w.CheckedAt), nz(w.FinishedAt), w.Recovered, w.ReceiptInvalidSeen)
	if err != nil {
		return fmt.Errorf("cashctl.db: saving withdrawal %s: %w", w.WithdrawalID, err)
	}
	return nil
}

const withdrawalColumns = `withdrawal_id, call_id, source_entry_id, wallet_pubkey, hub_identity, address, quote_json,
	quote_id, amount_millis, network_fee_millis, hub_fee_millis, fee_millis, payout_millis, sent_by, holds_until,
	send_before, notify, accepted_relays, state, payout_json, remint_entry_id, quote_expired_seen, confirmed_at,
	checked_at, finished_at, recovered, receipt_invalid_seen`

func scanWithdrawal(row interface{ Scan(...any) error }) (Withdrawal, error) {
	var w Withdrawal
	var addr, quote, quoteID, relays, payout, remint sql.NullString
	var netFee, hubFee, holds, before, confirmed, checked, finished sql.NullInt64
	err := row.Scan(&w.WithdrawalID, &w.CallID, &w.SourceEntryID, &w.WalletPubkey, &w.HubIdentity, &addr, &quote,
		&quoteID, &w.AmountMillis, &netFee, &hubFee, &w.FeeMillis, &w.PayoutMillis, &w.SentBy, &holds, &before,
		&w.Notify, &relays, &w.State, &payout, &remint, &w.QuoteExpiredSeen, &confirmed, &checked, &finished, &w.Recovered, &w.ReceiptInvalidSeen)
	w.Address, w.QuoteJSON, w.QuoteID, w.PayoutJSON, w.RemintEntryID = addr.String, quote.String, quoteID.String, payout.String, remint.String
	w.NetworkFeeMillis, w.HubFeeMillis = uint64(netFee.Int64), uint64(hubFee.Int64) //nolint:gosec // stored from uint64
	w.HoldsUntil, w.SendBefore, w.ConfirmedAt, w.CheckedAt, w.FinishedAt = holds.Int64, before.Int64, confirmed.Int64, checked.Int64, finished.Int64
	if relays.String != "" {
		_ = json.Unmarshal([]byte(relays.String), &w.AcceptedRelays)
	}
	return w, err
}

func findWithdrawalBy(where, arg string) (Withdrawal, bool, error) {
	db, err := store.Open()
	if err != nil {
		return Withdrawal{}, false, err
	}
	defer func() { _ = db.Close() }()
	w, err := scanWithdrawal(db.QueryRow(`SELECT `+withdrawalColumns+` FROM withdrawals WHERE `+where, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return Withdrawal{}, false, nil
	}
	if err != nil {
		return Withdrawal{}, false, fmt.Errorf("cashctl.db: reading a withdrawal: %w", err)
	}
	return w, true, nil
}

// FindWithdrawal reads a withdrawal by its id.
func FindWithdrawal(id string) (Withdrawal, bool, error) {
	return findWithdrawalBy(`withdrawal_id = ?`, id)
}

// WithdrawalForCall reads the withdrawal a call belongs to.
func WithdrawalForCall(callID string) (Withdrawal, bool, error) {
	return findWithdrawalBy(`call_id = ?`, callID)
}

// Withdrawals lists every withdrawal, oldest first.
func Withdrawals() ([]Withdrawal, error) {
	db, err := store.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT ` + withdrawalColumns + ` FROM withdrawals ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("cashctl.db: reading withdrawals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Withdrawal
	for rows.Next() {
		w, err := scanWithdrawal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// WithdrawQuote is a verified quote kept for `cash withdraw send --quote`.
type WithdrawQuote struct {
	Handle, EntryID, WalletPubkey, HubIdentity, Address, QuoteJSON, QuoteID string
	SendBefore                                                              int64
	Used                                                                    bool
	CreatedAt                                                               int64
}

// QuoteMaxAge is how long a stored quote handle may be used.
const QuoteMaxAge = 24 * 3600

// PutWithdrawQuote stores q; a newer quote for the same bill supersedes older ones.
func PutWithdrawQuote(q WithdrawQuote) error {
	if q.CreatedAt == 0 {
		q.CreatedAt = time.Now().Unix()
	}
	return store.WithBusyRetry(func() error {
		db, err := store.Open()
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		if _, err := db.Exec(`UPDATE withdraw_quotes SET used = 1 WHERE entry_id = ?`, q.EntryID); err != nil {
			return err
		}
		_, err = db.Exec(`INSERT OR REPLACE INTO withdraw_quotes (quote_handle, entry_id, wallet_pubkey, hub_identity, address,
			quote_json, quote_id, send_before, used, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
			q.Handle, q.EntryID, q.WalletPubkey, q.HubIdentity, q.Address, q.QuoteJSON, q.QuoteID, nz(q.SendBefore), q.CreatedAt)
		return err
	})
}

// FindWithdrawQuote reads a stored quote by handle.
func FindWithdrawQuote(handle string) (WithdrawQuote, bool, error) {
	db, err := store.Open()
	if err != nil {
		return WithdrawQuote{}, false, err
	}
	defer func() { _ = db.Close() }()
	var q WithdrawQuote
	var before sql.NullInt64
	err = db.QueryRow(`SELECT quote_handle, entry_id, wallet_pubkey, hub_identity, address, quote_json, quote_id, send_before,
		used, created_at FROM withdraw_quotes WHERE quote_handle = ?`, handle).Scan(&q.Handle, &q.EntryID, &q.WalletPubkey,
		&q.HubIdentity, &q.Address, &q.QuoteJSON, &q.QuoteID, &before, &q.Used, &q.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WithdrawQuote{}, false, nil
	}
	q.SendBefore = before.Int64
	return q, err == nil, err
}

// UseWithdrawQuote marks a quote used.
func UseWithdrawQuote(handle string) error {
	return store.WithBusyRetry(func() error {
		db, err := store.Open()
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		_, err = db.Exec(`UPDATE withdraw_quotes SET used = 1 WHERE quote_handle = ?`, handle)
		return err
	})
}
