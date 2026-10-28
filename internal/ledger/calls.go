package ledger

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/ohstr/nmilat/nipcash"

	"github.com/ohstr/cashctl/internal/store"
)

// A call is one bill-creating request carrying a request_id, written here
// before it is sent and kept until it is closed (NIP-CASH §Lost Replies;
// NIP-CP pending-entry semantics, kept locally). It holds what a lookup and a
// re-placement need: the id, the exact deadline sent, the target, who
// authorized it and every secret it generated.

// Call states. written, lookup, pending, stuck and expired (of another device)
// are open; done, not_applied, replaced, closed and expired (this device's)
// are final.
const (
	CallWritten    = "written"
	CallLookup     = "lookup"
	CallPending    = "pending"
	CallDone       = "done"
	CallNotApplied = "not_applied"
	CallReplaced   = "replaced"
	CallExpired    = "expired"
	CallStuck      = "stuck"
	CallClosed     = "closed"
	// CallOpenWithdraw is a withdrawal the Hub accepted: its payout is followed
	// until it is final or cancelled.
	CallOpenWithdraw = "open_withdraw"
	// CallOpenOrder is a placed order: its call is answered, and the order is
	// read until it is finished.
	CallOpenOrder = "open_order"
)

// Authorizer kinds: who signed (or, for a cash note, whose secret opened) the
// call, which is who must sign its lookup.
const (
	AuthLocalIdentity = "local_identity"
	AuthThrowawayKey  = "throwaway_key"
	AuthCashSecret    = "cash_secret"
	AuthCashHubClient = "cashhub_client"
	AuthAsCredential  = "as_credential"
	AuthBuyerKey      = "buyer_key"
)

// Secret roles: what each generated secret becomes.
const (
	RoleNewIdentity   = "new_identity"
	RoleRemainder     = "remainder"
	RoleMintRecipient = "mint_recipient"
	// RoleRemint is a cash-mode withdrawal's re-mint secret, used only if the
	// Hub cancels the payout.
	RoleRemint = "remint"
	// RoleInterimKey is the merge key an interim consolidate round targets
	// (its pubkey is the round's new identity): what spends the merged bill.
	RoleInterimKey = "interim_key"
)

// CallSecret is a secret a call generated, with its commitment, which is all
// the Hub sees.
type CallSecret struct {
	Role       string
	Secret     string
	Commitment string
}

// Call is one row of pending_calls with its secrets. Fields that hold a
// secret or the request id carry json:"-": a Call is shown to users by its
// CallID, never by request id (which a stranger could look up with).
type Call struct {
	CallID           string   `json:"call"`
	RequestID        string   `json:"-"`
	Method           string   `json:"method"`
	Purpose          string   `json:"purpose"`
	Deadline         int64    `json:"deadline"`
	SentH            int64    `json:"-"`
	HubIdentity      string   `json:"hub_identity"`
	Target           string   `json:"target"`
	TargetCredential string   `json:"-"`
	DeviceID         string   `json:"-"`
	AmountMillis     uint64   `json:"amount_millis"`
	Chain            string   `json:"chain"`
	AuthorizerKind   string   `json:"-"`
	AuthorizerPubkey string   `json:"-"`
	AuthorizerSecret string   `json:"-"`
	SourceEntryIDs   []string `json:"token_ids"`
	ParamsJSON       string   `json:"-"`
	Replaces         string   `json:"-"`
	Attempt          int      `json:"attempt"`
	State            string   `json:"state"`
	Detail           string   `json:"detail,omitempty"`
	FirstError       string   `json:"first_error,omitempty"`
	FirstDetail      string   `json:"first_detail,omitempty"`
	SawReused        bool     `json:"-"`
	RetryAfter       *int64   `json:"retry_after,omitempty"`
	CreatedAt        int64    `json:"created_at"`
	UpdatedAt        int64    `json:"updated_at"`
	ResultJSON       string   `json:"-"`
	ZapContext       string   `json:"-"`

	Secrets []CallSecret `json:"-"`
}

// Open reports a call whose outcome is not yet final.
func (c Call) Open() bool {
	switch c.State {
	case CallDone, CallNotApplied, CallReplaced, CallClosed:
		return false
	}
	return true
}

// CallIDFor is the local handle shown to users: the first 12 hex characters
// of sha256("call:" ‖ request_id). The request id itself is never printed.
func CallIDFor(requestID string) string {
	sum := sha256.Sum256([]byte("call:" + requestID))
	return hex.EncodeToString(sum[:])[:12]
}

// ErrDuplicateCall is AddCall given a request id already recorded.
var ErrDuplicateCall = errors.New("this request id is already recorded")

// AddCall records a call written ahead, state written unless set. The secrets'
// commitments are computed here from the secrets, never taken from the
// caller.
func (l *Ledger) AddCall(c Call) (*Call, error) {
	if !nipcash.ValidRequestID(c.RequestID) {
		return nil, fmt.Errorf("ledger: not a valid request id")
	}
	c.CallID = CallIDFor(c.RequestID)
	for _, existing := range l.Calls {
		if existing.CallID == c.CallID || existing.RequestID == c.RequestID {
			return nil, ErrDuplicateCall
		}
	}
	for i := range c.Secrets {
		commitment, err := commitmentOf(c.Secrets[i].Secret)
		if err != nil {
			return nil, err
		}
		c.Secrets[i].Commitment = commitment
	}
	now := time.Now().Unix()
	c.CreatedAt, c.UpdatedAt = now, now
	if c.State == "" {
		c.State = CallWritten
	}
	l.Calls = append(l.Calls, c)
	return &l.Calls[len(l.Calls)-1], nil
}

// FindCall looks a call up by its CallID.
func (l *Ledger) FindCall(callID string) (*Call, bool) {
	for i := range l.Calls {
		if l.Calls[i].CallID == callID {
			return &l.Calls[i], true
		}
	}
	return nil, false
}

// OpenCallFor is an open call that spends entryID, if any: no new call may be
// placed on a source while one is open (the B4 invariant).
func (l *Ledger) OpenCallFor(entryID string) (*Call, bool) {
	for i := range l.Calls {
		if l.Calls[i].Open() && slices.Contains(l.Calls[i].SourceEntryIDs, entryID) {
			return &l.Calls[i], true
		}
	}
	return nil, false
}

// SetCallState moves a call to state with detail, stamping updated_at.
func (l *Ledger) SetCallState(callID, state, detail string) error {
	c, ok := l.FindCall(callID)
	if !ok {
		return fmt.Errorf("no call %q", callID)
	}
	c.State, c.Detail, c.UpdatedAt = state, detail, time.Now().Unix()
	return nil
}

// RemoveCall drops a call that was never sent, with its secrets, at the next
// Save. Only a call nothing reached the Hub with may be removed.
func (l *Ledger) RemoveCall(callID string) bool {
	for i := range l.Calls {
		if l.Calls[i].CallID == callID {
			l.Calls = append(l.Calls[:i], l.Calls[i+1:]...)
			return true
		}
	}
	return false
}

func commitmentOf(secret string) (string, error) {
	t, err := nipcash.CashTargetFromSecret(secret)
	if err != nil {
		return "", fmt.Errorf("ledger: not a cash secret: %w", err)
	}
	return t.Commitment(), nil
}

// ReadCall reads a call back on a fresh connection.
func ReadCall(callID string) (Call, error) {
	db, err := store.Open()
	if err != nil {
		return Call{}, err
	}
	defer func() { _ = db.Close() }()
	calls, _, err := loadCalls(db, `WHERE call_id = ?`, callID)
	if err != nil {
		return Call{}, err
	}
	if len(calls) != 1 {
		return Call{}, fmt.Errorf("ledger: call %s was not written", callID)
	}
	return calls[0], nil
}

// ErrReadBack is a call whose stored row differs from what was meant to be
// written: it must not be sent.
var ErrReadBack = errors.New("ledger: the call read back differs from what was written")

// VerifyWritten reads want back on a fresh connection and compares every
// column but updated_at, and recomputes each secret's commitment from the
// stored secret. Only a call that passes may be sent.
func VerifyWritten(want Call) error {
	got, err := ReadCall(want.CallID)
	if err != nil {
		return err
	}
	for _, s := range got.Secrets {
		commitment, err := commitmentOf(s.Secret)
		if err != nil || commitment != s.Commitment {
			return fmt.Errorf("%w: secret %s does not match its commitment", ErrReadBack, s.Role)
		}
	}
	got.UpdatedAt, want.UpdatedAt = 0, 0
	if !callsEqual(got, want) {
		return ErrReadBack
	}
	return nil
}

func callsEqual(a, b Call) bool {
	if len(a.SourceEntryIDs) == 0 && len(b.SourceEntryIDs) == 0 {
		a.SourceEntryIDs, b.SourceEntryIDs = nil, nil
	}
	if len(a.Secrets) == 0 && len(b.Secrets) == 0 {
		a.Secrets, b.Secrets = nil, nil
	}
	return reflect.DeepEqual(a, b)
}

// ErrDerivedRecorded is InsertDerived for an id already recorded: another run
// (or process) placed it, so this one only closes the parent.
var ErrDerivedRecorded = errors.New("ledger: this derived request id was already placed")

// InsertDerived records a derived request id before it is placed. Its primary
// key is the cross-process lock: of two processes re-placing one call, only
// one inserts.
func InsertDerived(requestID, parentRequestID string, deadline *int64, keepUntil int64) error {
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	var d sql.NullInt64
	if deadline != nil {
		d = sql.NullInt64{Int64: *deadline, Valid: true}
	}
	_, err = db.Exec(`INSERT INTO derived_ids (request_id, parent_request_id, deadline, keep_until) VALUES (?, ?, ?, ?)`,
		requestID, parentRequestID, d, keepUntil)
	if store.IsUniqueViolation(err) {
		return ErrDerivedRecorded
	}
	return err
}

// RemoveDerived drops a recorded derived id whose call never reached the Hub,
// so the same re-placement may be made again.
func RemoveDerived(requestID string) error {
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`DELETE FROM derived_ids WHERE request_id = ?`, requestID)
	return err
}

// DerivedParent is the request id a recorded derived id was derived from, or
// "" when it was recorded without one (or not at all).
func DerivedParent(requestID string) (string, error) {
	db, err := store.Open()
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()
	var parent string
	err = db.QueryRow(`SELECT parent_request_id FROM derived_ids WHERE request_id = ?`, requestID).Scan(&parent)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return parent, err
}

// DerivedRecorded reports whether a derived id was recorded here.
func DerivedRecorded(requestID string) (bool, error) {
	db, err := store.Open()
	if err != nil {
		return false, err
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM derived_ids WHERE request_id = ?`, requestID).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// pruneMinAge is the least age of a final call that PruneCalls drops.
const pruneMinAge = 30 * 24 * 3600

// PruneCalls drops, at the next Save, final calls (done, not_applied,
// replaced, closed) past deadline + max(sent_h, maxH) + 86400 and older than
// 30 days. An expired or stuck call is never pruned automatically.
func (l *Ledger) PruneCalls(now, maxH int64) int {
	kept := l.Calls[:0]
	pruned := 0
	for _, c := range l.Calls {
		h := max(c.SentH, maxH)
		if !c.Open() && c.State != CallExpired && now > c.Deadline+h+86400 && now-c.CreatedAt > pruneMinAge {
			pruned++
			continue
		}
		kept = append(kept, c)
	}
	l.Calls = kept
	return pruned
}

const callColumns = `call_id, request_id, method, purpose, deadline, sent_h, hub_identity, target,
	target_credential, device_id, amount_millis, chain, authorizer_kind, authorizer_pubkey,
	authorizer_secret, source_entry_ids, params_json, replaces, attempt, state, detail, first_error,
	first_detail, saw_reused, retry_after, created_at, updated_at, result_json, version, zap_context`

// loadCalls reads calls (and their secrets) matching where.
func loadCalls(db *sql.DB, where string, args ...any) ([]Call, map[string]int64, error) {
	rows, err := db.Query(`SELECT `+callColumns+` FROM pending_calls `+where+` ORDER BY rowid`, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("cashctl.db: reading calls: %w", err)
	}
	var calls []Call
	versions := map[string]int64{}
	for rows.Next() {
		var c Call
		var authPub, authSecret, replaces, detail, firstErr, firstDetail, result, zap sql.NullString
		var sources string
		var retryAfter sql.NullInt64
		var amount, version int64
		if err := rows.Scan(&c.CallID, &c.RequestID, &c.Method, &c.Purpose, &c.Deadline, &c.SentH, &c.HubIdentity, &c.Target,
			&c.TargetCredential, &c.DeviceID, &amount, &c.Chain, &c.AuthorizerKind, &authPub,
			&authSecret, &sources, &c.ParamsJSON, &replaces, &c.Attempt, &c.State, &detail, &firstErr,
			&firstDetail, &c.SawReused, &retryAfter, &c.CreatedAt, &c.UpdatedAt, &result, &version, &zap); err != nil {
			_ = rows.Close()
			return nil, nil, fmt.Errorf("cashctl.db: reading calls: %w", err)
		}
		c.AmountMillis = uint64(amount) //nolint:gosec // written from a uint64 by save
		c.AuthorizerPubkey, c.AuthorizerSecret, c.Replaces = authPub.String, authSecret.String, replaces.String
		c.Detail, c.FirstError, c.FirstDetail = detail.String, firstErr.String, firstDetail.String
		c.ResultJSON, c.ZapContext = result.String, zap.String
		if retryAfter.Valid {
			v := retryAfter.Int64
			c.RetryAfter = &v
		}
		if err := json.Unmarshal([]byte(sources), &c.SourceEntryIDs); err != nil {
			_ = rows.Close()
			return nil, nil, fmt.Errorf("cashctl.db: call %s has corrupt source_entry_ids: %w", c.CallID, err)
		}
		calls = append(calls, c)
		versions[c.CallID] = version
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, err
	}
	_ = rows.Close()
	for i := range calls {
		sRows, err := db.Query(`SELECT role, secret, commitment FROM call_secrets WHERE call_id = ? ORDER BY role`, calls[i].CallID)
		if err != nil {
			return nil, nil, fmt.Errorf("cashctl.db: reading call secrets: %w", err)
		}
		for sRows.Next() {
			var s CallSecret
			if err := sRows.Scan(&s.Role, &s.Secret, &s.Commitment); err != nil {
				_ = sRows.Close()
				return nil, nil, fmt.Errorf("cashctl.db: reading call secrets: %w", err)
			}
			calls[i].Secrets = append(calls[i].Secrets, s)
		}
		_ = sRows.Close()
	}
	return calls, versions, nil
}

// refuseForeignOpenCall is ErrConcurrentUpdate when a call this process never
// loaded, still open in the database, spends one of c's sources.
func (l *Ledger) refuseForeignOpenCall(tx *sql.Tx, present map[string]bool, c Call) error {
	if !c.Open() {
		return nil
	}
	for _, id := range c.SourceEntryIDs {
		rows, err := tx.Query(`SELECT call_id FROM pending_calls
			WHERE state NOT IN (?, ?, ?, ?) AND source_entry_ids LIKE ?`,
			CallDone, CallNotApplied, CallReplaced, CallClosed, `%"`+id+`"%`)
		if err != nil {
			return fmt.Errorf("cashctl.db: reading open calls: %w", err)
		}
		var foreign string
		for rows.Next() {
			var callID string
			if err := rows.Scan(&callID); err != nil {
				_ = rows.Close()
				return err
			}
			if _, mine := l.loadedCalls[callID]; !mine && !present[callID] && callID != c.CallID {
				foreign = callID
			}
		}
		_ = rows.Close()
		if foreign != "" {
			return fmt.Errorf("%w (bill %s has an open call %s from another process)", ErrConcurrentUpdate, id, foreign)
		}
	}
	return nil
}

// saveCalls writes the calls this process changed, inside save's transaction:
// new ones inserted with their secrets, changed ones updated under the version
// they were loaded at, removed ones deleted.
func (l *Ledger) saveCalls(tx *sql.Tx) (map[string]int64, error) {
	newVersions := make(map[string]int64, len(l.Calls))
	present := map[string]bool{}
	for _, c := range l.Calls {
		present[c.CallID] = true
		prior, known := l.loadedCalls[c.CallID]
		expected := l.callVersions[c.CallID]
		if known && callsEqual(prior, c) {
			newVersions[c.CallID] = expected
			continue
		}
		sources, err := json.Marshal(nonNilStrings(c.SourceEntryIDs))
		if err != nil {
			return nil, err
		}
		var retryAfter sql.NullInt64
		if c.RetryAfter != nil {
			retryAfter = sql.NullInt64{Int64: *c.RetryAfter, Valid: true}
		}
		args := []any{c.CallID, c.RequestID, c.Method, c.Purpose, c.Deadline, c.SentH, c.HubIdentity, c.Target,
			c.TargetCredential, c.DeviceID, int64(c.AmountMillis), c.Chain, c.AuthorizerKind, nullStr(c.AuthorizerPubkey), //nolint:gosec // amounts fit
			nullStr(c.AuthorizerSecret), string(sources), c.ParamsJSON, nullStr(c.Replaces), c.Attempt, c.State, nullStr(c.Detail),
			nullStr(c.FirstError), nullStr(c.FirstDetail), c.SawReused, retryAfter, c.CreatedAt, c.UpdatedAt, nullStr(c.ResultJSON),
			nullStr(c.ZapContext)}
		if !known {
			// No new call on a bill another process has an open call on (plan
			// R-CP-10): the in-memory guard sees only this process's calls.
			if err := l.refuseForeignOpenCall(tx, present, c); err != nil {
				return nil, err
			}
			_, err := tx.Exec(`INSERT INTO pending_calls (`+callColumns+`)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`, args...)
			if store.IsUniqueViolation(err) {
				return nil, ErrDuplicateCall
			}
			if err != nil {
				return nil, fmt.Errorf("cashctl.db: writing call %s: %w", c.CallID, err)
			}
			newVersions[c.CallID] = 0
		} else {
			res, err := tx.Exec(`UPDATE pending_calls SET call_id = ?, request_id = ?, method = ?, purpose = ?, deadline = ?,
				sent_h = ?, hub_identity = ?, target = ?, target_credential = ?, device_id = ?, amount_millis = ?, chain = ?,
				authorizer_kind = ?, authorizer_pubkey = ?, authorizer_secret = ?, source_entry_ids = ?, params_json = ?,
				replaces = ?, attempt = ?, state = ?, detail = ?, first_error = ?, first_detail = ?, saw_reused = ?,
				retry_after = ?, created_at = ?, updated_at = ?, result_json = ?, zap_context = ?, version = version + 1
				WHERE call_id = ? AND version = ?`, append(args, c.CallID, expected)...)
			if err != nil {
				return nil, fmt.Errorf("cashctl.db: updating call %s: %w", c.CallID, err)
			}
			if n, raErr := res.RowsAffected(); raErr == nil && n == 0 {
				return nil, fmt.Errorf("%w (call %s)", ErrConcurrentUpdate, c.CallID)
			}
			newVersions[c.CallID] = expected + 1
		}
		if known && reflect.DeepEqual(prior.Secrets, c.Secrets) {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM call_secrets WHERE call_id = ?`, c.CallID); err != nil {
			return nil, err
		}
		for _, s := range c.Secrets {
			if _, err := tx.Exec(`INSERT INTO call_secrets (call_id, role, secret, commitment) VALUES (?, ?, ?, ?)`,
				c.CallID, s.Role, s.Secret, s.Commitment); err != nil {
				return nil, fmt.Errorf("cashctl.db: writing a secret of call %s: %w", c.CallID, err)
			}
		}
	}
	for id := range l.loadedCalls {
		if present[id] {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM call_secrets WHERE call_id = ?`, id); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`DELETE FROM pending_calls WHERE call_id = ? AND version = ?`, id, l.callVersions[id]); err != nil {
			return nil, err
		}
	}
	return newVersions, nil
}

func nullStr(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// snapshotCalls records the calls as the baseline the next save diffs against.
func (l *Ledger) snapshotCalls(versions map[string]int64) {
	l.loadedCalls = make(map[string]Call, len(l.Calls))
	for _, c := range l.Calls {
		cp := c
		cp.SourceEntryIDs = slices.Clone(c.SourceEntryIDs)
		cp.Secrets = slices.Clone(c.Secrets)
		if c.RetryAfter != nil {
			v := *c.RetryAfter
			cp.RetryAfter = &v
		}
		l.loadedCalls[c.CallID] = cp
	}
	l.callVersions = versions
}
