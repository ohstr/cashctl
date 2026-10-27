package config

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ohstr/cashctl/internal/store"
)

// Join states. A join is written before it is sent (NIP-CW §Pairing
// Connection: the member generates the wallet's connection key, and a lost
// reply must not lose it).
const (
	JoinWritten    = "written"     // written ahead; the join may have been sent
	JoinPending    = "pending"     // a lookup said pending, or no answer yet
	JoinDone       = "done"        // the wallet exists and is registered here
	JoinNotApplied = "not_applied" // no wallet was made; the key is forgotten
	JoinShown      = "shown"       // kept for the member to see (expired, refused lookup, ...)
)

// JoinPurposeRotation marks a joins row that is a member's own rotate request
// (NIP-CW §Recovering a Lost Connection) rather than a join: the new
// connection key is written ahead before the signed request is shown, and the
// row is looked up with the join lookup naming that key. Its purpose, the
// wallet it moves and the signed request are kept in the row's terms column.
const JoinPurposeRotation = "rotation"

// rotationTerms is a rotation row's terms column.
type rotationTerms struct {
	Purpose      string          `json:"purpose"`
	WalletPubkey string          `json:"wallet_pubkey"`
	Event        json.RawMessage `json:"event,omitempty"`
}

// Join is one circle join, from its write-ahead to its outcome.
// ConnectionPubkey is what the Hub knows the wallet by.
type Join struct {
	ConnectionPubkey string `json:"connection_pubkey"`
	// ConnectionSecret is the wallet's client secret: json:"-" like
	// Connection.Value, so listing joins never prints it. Cleared once the
	// wallet is registered.
	ConnectionSecret string `json:"-"`
	// HubPubkey is the circlehub1's TLV 0: a circle is keyed by it, whichever
	// string joined it.
	HubPubkey    string `json:"hub_pubkey"`
	MemberPubkey string `json:"member_pubkey"`
	// HubConnection is the circlehub1 the join went out on; lookups use it.
	HubConnection string `json:"-"`
	HubIdentity   string `json:"hub_identity"`
	Chain         string `json:"chain"`
	HubLabel      string `json:"hub_label,omitempty"`
	ConnVerified  bool   `json:"hub_verified"`
	Deadline      int64  `json:"deadline"`
	DeviceID      string `json:"-"`
	Terms         string `json:"-"`
	State         string `json:"state"`
	Detail        string `json:"detail,omitempty"`
	RetryAfter    int64  `json:"retry_after,omitempty"`
	WalletName    string `json:"wallet,omitempty"`
	// LookupAnswered is any lookup of this join answered (not omitted).
	LookupAnswered bool `json:"-"`
	// AltLookupTried is a lookup already signed with another string of the
	// same circle (R-CW-21), which is tried once.
	AltLookupTried bool  `json:"-"`
	CreatedAt      int64 `json:"created_at"`
	// Purpose is "" for a join and JoinPurposeRotation for a rotate request,
	// whose WalletPubkey is the wallet it moves to ConnectionPubkey and whose
	// Event is the member request as signed (WalletName is that wallet's
	// name here). Its Deadline is the request's created_at plus 24 hours,
	// the last moment a Hub accepts it.
	Purpose      string          `json:"purpose,omitempty"`
	WalletPubkey string          `json:"wallet_pubkey,omitempty"`
	Event        json.RawMessage `json:"-"`
}

// IsRotation reports whether j is a rotate request rather than a join.
func (j Join) IsRotation() bool { return j.Purpose == JoinPurposeRotation }

// Open reports whether the join's outcome is still unknown.
func (j Join) Open() bool { return j.State == JoinWritten || j.State == JoinPending }

// ErrJoinReadBack is a write-ahead whose row did not read back as written:
// the join is not sent.
var ErrJoinReadBack = errors.New("cashctl.db: the join's write-ahead did not read back as written")

// WriteJoin records j, durably, before its request is sent, and reads it back.
func WriteJoin(j Join) error {
	now := time.Now().Unix()
	if j.CreatedAt == 0 {
		j.CreatedAt = now
	}
	if j.State == "" {
		j.State = JoinWritten
	}
	var walletName sql.NullString
	if j.IsRotation() {
		terms, err := json.Marshal(rotationTerms{Purpose: j.Purpose, WalletPubkey: j.WalletPubkey, Event: j.Event})
		if err != nil {
			return err
		}
		j.Terms = string(terms)
		walletName = sql.NullString{String: j.WalletName, Valid: j.WalletName != ""}
	}
	err := store.WithBusyRetry(func() error {
		db, err := store.Open()
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		_, err = db.Exec(`INSERT INTO joins (connection_pubkey, connection_secret, hub_pubkey, member_pubkey, hub_connection,
			hub_identity, chain, hub_label, conn_verified, deadline, device_id, terms, state, wallet_name, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			j.ConnectionPubkey, j.ConnectionSecret, j.HubPubkey, j.MemberPubkey, j.HubConnection,
			j.HubIdentity, j.Chain, j.HubLabel, j.ConnVerified, j.Deadline, j.DeviceID, j.Terms, j.State, walletName, j.CreatedAt, now)
		if err != nil {
			return fmt.Errorf("cashctl.db: saving the join's connection key: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	got, ok, err := FindJoin(j.ConnectionPubkey)
	if err != nil {
		return err
	}
	if !ok || got.ConnectionSecret != j.ConnectionSecret || got.Deadline != j.Deadline ||
		got.HubPubkey != j.HubPubkey || got.MemberPubkey != j.MemberPubkey {
		return ErrJoinReadBack
	}
	return nil
}

// SetJoinState records a join's outcome. A not_applied join forgets its
// connection secret: no wallet holds it.
func SetJoinState(connectionPubkey, state, detail string, retryAfter int64) error {
	return store.WithBusyRetry(func() error {
		db, err := store.Open()
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		q := `UPDATE joins SET state = ?, detail = ?, retry_after = ?, updated_at = ? WHERE connection_pubkey = ?`
		if state == JoinNotApplied {
			q = `UPDATE joins SET state = ?, detail = ?, retry_after = ?, updated_at = ?, connection_secret = '' WHERE connection_pubkey = ?`
		}
		var ra sql.NullInt64
		if retryAfter > 0 {
			ra = sql.NullInt64{Int64: retryAfter, Valid: true}
		}
		if _, err := db.Exec(q, state, detail, ra, time.Now().Unix(), connectionPubkey); err != nil {
			return fmt.Errorf("cashctl.db: updating a join: %w", err)
		}
		return nil
	})
}

const joinColumns = `connection_pubkey, connection_secret, hub_pubkey, member_pubkey, hub_connection, hub_identity, chain,
	hub_label, conn_verified, deadline, device_id, terms, state, detail, retry_after, wallet_name,
	lookup_answered, alt_lookup_tried, created_at`

func scanJoin(row interface{ Scan(...any) error }) (Join, error) {
	var j Join
	var terms, detail, wallet sql.NullString
	var retry sql.NullInt64
	err := row.Scan(&j.ConnectionPubkey, &j.ConnectionSecret, &j.HubPubkey, &j.MemberPubkey, &j.HubConnection,
		&j.HubIdentity, &j.Chain, &j.HubLabel, &j.ConnVerified, &j.Deadline, &j.DeviceID, &terms, &j.State,
		&detail, &retry, &wallet, &j.LookupAnswered, &j.AltLookupTried, &j.CreatedAt)
	j.Terms, j.Detail, j.WalletName, j.RetryAfter = terms.String, detail.String, wallet.String, retry.Int64
	var rt rotationTerms
	if err == nil && json.Unmarshal([]byte(j.Terms), &rt) == nil && rt.Purpose == JoinPurposeRotation {
		j.Purpose, j.WalletPubkey, j.Event = rt.Purpose, rt.WalletPubkey, rt.Event
	}
	return j, err
}

// FindJoin reads the join with this connection pubkey.
func FindJoin(connectionPubkey string) (Join, bool, error) {
	db, err := store.Open()
	if err != nil {
		return Join{}, false, err
	}
	defer func() { _ = db.Close() }()
	j, err := scanJoin(db.QueryRow(`SELECT `+joinColumns+` FROM joins WHERE connection_pubkey = ?`, connectionPubkey))
	if errors.Is(err, sql.ErrNoRows) {
		return Join{}, false, nil
	}
	if err != nil {
		return Join{}, false, fmt.Errorf("cashctl.db: reading a join: %w", err)
	}
	return j, true, nil
}

// Joins lists every join, oldest first.
func Joins() ([]Join, error) {
	db, err := store.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT ` + joinColumns + ` FROM joins ORDER BY created_at, rowid`)
	if err != nil {
		return nil, fmt.Errorf("cashctl.db: reading joins: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Join{}
	for rows.Next() {
		j, err := scanJoin(rows)
		if err != nil {
			return nil, fmt.Errorf("cashctl.db: reading joins: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// PendingJoins lists the joins a member still needs to see: open ones, and
// ones kept for display (an expired join, a refused lookup). Done and
// not_applied joins are left out.
func PendingJoins() ([]Join, error) {
	all, err := Joins()
	if err != nil {
		return nil, err
	}
	out := []Join{}
	for _, j := range all {
		if j.State != JoinDone && j.State != JoinNotApplied {
			out = append(out, j)
		}
	}
	return out, nil
}

// MarkJoinLookup records that a lookup of the join was answered, and/or that
// the once-only lookup over another string of its circle was tried. Flags
// only ever turn on.
func MarkJoinLookup(connectionPubkey string, answered, altTried bool) error {
	return store.WithBusyRetry(func() error {
		db, err := store.Open()
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		_, err = db.Exec(`UPDATE joins SET lookup_answered = MAX(lookup_answered, ?), alt_lookup_tried = MAX(alt_lookup_tried, ?)
			WHERE connection_pubkey = ?`, answered, altTried, connectionPubkey)
		return err
	})
}

// OtherHubConnections are the circlehub1 strings this wallet holds for the
// circle with TLV 0 hubPubkey other than except: a circle is keyed by its TLV
// 0, whichever string joined it (R-CW-21).
func OtherHubConnections(hubPubkey, except string) ([]string, error) {
	all, err := Joins()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, j := range all {
		if j.HubPubkey == hubPubkey && j.HubConnection != "" && j.HubConnection != except && !slices.Contains(out, j.HubConnection) {
			out = append(out, j.HubConnection)
		}
	}
	return out, nil
}

// OpenRotation is the open rotate request of the wallet with walletPubkey, if
// there is one.
func OpenRotation(walletPubkey string) (Join, bool, error) {
	all, err := Joins()
	if err != nil {
		return Join{}, false, err
	}
	for _, j := range all {
		if j.IsRotation() && j.Open() && j.WalletPubkey == walletPubkey {
			return j, true, nil
		}
	}
	return Join{}, false, nil
}
