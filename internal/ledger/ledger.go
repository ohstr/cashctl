// Package ledger tracks cash tokens cashctl has received or produced for
// itself, and a chronological log of local actions (cashctl-plan.md's
// "Ledger entries store what's needed to act again without re-asking the
// user").
package ledger

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"modernc.org/sqlite"

	"github.com/ohstr/cashctl/internal/store"
)

// Status values a held token moves through.
const (
	StatusHeld         = "held"
	StatusRedeemed     = "redeemed"
	StatusTransferred  = "transferred"
	StatusConsolidated = "consolidated"
)

// Entry is one cash token cashctl knows about. WalletPubkey/Secret/RelayURLs/
// IdentityRequired/AmountMillis are decoded straight from Token (see
// nipcash.Decode) — cached here for display convenience, not as a separate
// source of truth. The connection-key fields are the one thing that can't
// be recovered from the token bytes alone (see cashctl-plan.md): a
// *reference* (attestation event ID + IA pubkey), never a cached copy of
// the attestation event, so redeeming always re-checks live revocation.
type Entry struct {
	ID           string `json:"id"`
	Token        string `json:"token"`
	WalletPubkey string `json:"wallet_pubkey"`
	// Secret is the token's own type-2 TLV field: the NWC connection
	// secret. It lets cashctl dial the wallet (list-recipients, decode,
	// ...) but is NEVER sufficient to redeem/transfer a bearer slice —
	// see BearerSecret's own doc comment (NIP-CASH.md's Redemption
	// Metadata section covers this distinction in full). json:"-": this
	// Entry is embedded directly into --json output in several places
	// (wallet show's held_tokens, receive's entry, transfer's
	// remainder_entry, consolidate's new_entry) — it must never leak a
	// dialing/spending credential just because a command happened to
	// return the whole struct, the same "never print a secret" rule
	// printCashBill/decode already follow for their own hand-built output.
	Secret           string   `json:"-"`
	RelayURLs        []string `json:"relay_urls,omitempty"`
	IdentityRequired *bool    `json:"identity_required,omitempty"`
	AmountMillis     *uint64  `json:"amount_millis,omitempty"`
	ReceivedAt       string   `json:"received_at"`
	Verified         bool     `json:"verified"`
	Status           string   `json:"status"`

	// BearerSecret is the actual spending credential for a bearer-mode
	// token — a value that exists only in mint_cash's own response,
	// returned exactly once, and can never be derived from the token
	// itself (unlike every other cached field on this Entry). Always set
	// for a bearer-mode entry: `cashctl receive` requires the secret
	// embedded in the token itself (`<token>#<bearer_secret>`) and
	// degrades to a read-only report — never saving anything — for a
	// bearer-mode token pasted without one, so an entry with
	// IdentityRequired false is guaranteed to carry its spending secret
	// already — no later `--as bearer:<secret>` override needed.
	// json:"-": see Secret's own doc comment above — this is the actual
	// spending credential for a bearer slice, so leaking it here would be
	// strictly worse than leaking Secret.
	BearerSecret string `json:"-"`

	// Connection-key mode reference — set only when the user has told
	// cashctl this token is connection-key-bound (not derivable from the
	// token itself; IdentityRequired only says a proof is needed, not
	// which mode). Empty for pubkey- and bearer-mode tokens.
	ConnectionKeyPlatform   string `json:"connection_key_platform,omitempty"`
	ConnectionKeyExternalID string `json:"connection_key_external_id,omitempty"`
	AttestationEventID      string `json:"attestation_event_id,omitempty"`
	IAPubkey                string `json:"ia_pubkey,omitempty"`

	// MinterPubkey is set only when the token carries a mint_signature that
	// VerifyProvenance confirms as valid (nil otherwise — an unsigned or
	// invalidly-signed token has no trustworthy minter identity). Populated
	// once, at receive time, from the same VerifyProvenance call already
	// made for display (see cash_receive.go's printCashBill) — this is
	// cash-selection's only client-side signal for "these held tokens came
	// from the same minter" (docs/ux-review.md Part 2).
	MinterPubkey *string `json:"minter_pubkey,omitempty"`
}

// HistoryEntry is one line of cashctl's local action log (`cashctl wallet
// history`).
type HistoryEntry struct {
	At     string `json:"at"`
	Action string `json:"action"`
	Detail string `json:"detail"`
}

// Ledger is the in-memory shape of cashctl.db's entries/history tables —
// loaded whole (Load) into plain slices so every other method on this type
// (Find, Add, SetStatus, ...) is pure in-memory logic, unaware storage is
// SQL at all. Save no longer writes the whole table back wholesale (see its
// own doc comment) — that used to mean two concurrent cashctl processes
// against the same --config-dir/cashctl.db (nothing here is a filesystem
// lock, and there's no OS-level reason two `cashctl` invocations can't
// target the same one) would silently clobber each other's writes.
type Ledger struct {
	Entries []Entry
	History []HistoryEntry

	// loaded is what Load actually read, keyed by ID — Save diffs against
	// it to only touch rows THIS process actually changed (see Save's own
	// doc comment). nil on a Ledger built directly (&Ledger{}, as every
	// unit test in this package and in cmd/*_test.go does) rather than via
	// Load: every entry in Entries then has no baseline to compare against
	// and is unconditionally written, the same as this type's old
	// behavior — a fresh, never-persisted Ledger has nothing to lose by
	// being written in full.
	loaded map[string]Entry
	// historyLoaded is len(History) as of Load — Save only appends rows
	// beyond it. History has no natural per-row key to diff by the way
	// Entries has ID, so "only append the tail this process actually
	// added" is the only safe way to avoid re-touching (or duplicating)
	// rows a concurrent process already committed.
	historyLoaded int
}

// Load reads every entry and history line from cashctl.db, returning an
// empty (not nil) *Ledger if neither table has any rows yet. Retried on a
// transient SQLITE_BUSY (see withBusyRetry) — store.Open's own schema
// migration is a write and can collide with a concurrent process's Save.
func Load() (*Ledger, error) {
	var l *Ledger
	err := withBusyRetry(func() error {
		var err error
		l, err = load()
		return err
	})
	if err != nil {
		return nil, err
	}
	return l, nil
}

// load is Load's single, non-retrying attempt.
func load() (*Ledger, error) {
	db, err := store.Open()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	l := &Ledger{}
	// ORDER BY rowid (SQLite's own implicit insertion-order column — the
	// `id TEXT PRIMARY KEY` above doesn't replace it) rather than
	// received_at: that timestamp only has 1-second precision, so two
	// entries added within the same second would otherwise sort by id (a
	// random string, uncorrelated with real insertion order) instead of
	// reliably preserving the exact order Save wrote them in — the same
	// guarantee the old JSON-array storage always gave for free.
	rows, err := db.Query(`SELECT id, token, wallet_pubkey, minter_pubkey, secret, relay_urls,
		identity_required, amount_millis, received_at, verified, status, bearer_secret,
		connection_key_platform, connection_key_external_id, attestation_event_id, ia_pubkey
		FROM entries ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("cashctl.db: reading entries: %w", err)
	}
	for rows.Next() {
		var e Entry
		var relayURLs sql.NullString
		var identityRequired, amountMillis sql.NullInt64
		if err := rows.Scan(&e.ID, &e.Token, &e.WalletPubkey, &e.MinterPubkey, &e.Secret, &relayURLs,
			&identityRequired, &amountMillis, &e.ReceivedAt, &e.Verified, &e.Status, &e.BearerSecret,
			&e.ConnectionKeyPlatform, &e.ConnectionKeyExternalID, &e.AttestationEventID, &e.IAPubkey); err != nil {
			rows.Close()
			return nil, fmt.Errorf("cashctl.db: reading entries: %w", err)
		}
		if relayURLs.Valid && relayURLs.String != "" {
			if err := json.Unmarshal([]byte(relayURLs.String), &e.RelayURLs); err != nil {
				rows.Close()
				return nil, fmt.Errorf("cashctl.db: entry %s has corrupt relay_urls: %w", e.ID, err)
			}
		}
		if identityRequired.Valid {
			v := identityRequired.Int64 != 0
			e.IdentityRequired = &v
		}
		if amountMillis.Valid {
			v := uint64(amountMillis.Int64)
			e.AmountMillis = &v
		}
		l.Entries = append(l.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cashctl.db: reading entries: %w", err)
	}
	rows.Close()

	histRows, err := db.Query(`SELECT at, action, detail FROM history ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("cashctl.db: reading history: %w", err)
	}
	defer histRows.Close()
	for histRows.Next() {
		var h HistoryEntry
		if err := histRows.Scan(&h.At, &h.Action, &h.Detail); err != nil {
			return nil, fmt.Errorf("cashctl.db: reading history: %w", err)
		}
		l.History = append(l.History, h)
	}
	if err := histRows.Err(); err != nil {
		return nil, fmt.Errorf("cashctl.db: reading history: %w", err)
	}

	l.loaded = make(map[string]Entry, len(l.Entries))
	for _, e := range l.Entries {
		l.loaded[e.ID] = e
	}
	l.historyLoaded = len(l.History)
	return l, nil
}

// Save writes l's current contents to cashctl.db, in one transaction — but,
// unlike the old "DELETE everything, reinsert everything I have" version,
// only the rows this process actually changed since Load:
//
//   - an entry identical (reflect.DeepEqual) to what Load returned for that
//     ID is left alone entirely, never rewritten;
//   - an entry that's new (Add) or was mutated (SetStatus/SetVerified, or a
//     direct field write on an *Entry obtained via Find — Held returns
//     copies, so a caller must go through Find or a setter for a change to
//     stick) is upserted by ID;
//   - history is append-only past whatever Load saw (historyLoaded) — a row
//     already on disk, including one a concurrent process committed after
//     this Ledger's own Load, is never touched, reordered, or duplicated.
//
// This matters because Load and Save are two independent connections with
// no lock spanning the gap between them — the normal shape of a command is
// Load, then a live wire round trip (redeem/transfer/consolidate) that can
// easily run longer than another `cashctl` process's entire
// Load-mutate-Save cycle against the same --config-dir. The old blind
// overwrite meant whichever of two concurrent processes called Save last
// won outright: it silently reverted the other's status transition back to
// "held" (a spent token resurrected in wallet show) and erased its history
// line, using nothing more than its own stale in-memory snapshot. Diffing
// against Load's own baseline means a process only ever asserts the rows it
// actually knows it changed, so two processes touching disjoint entries no
// longer stomp each other — see docs/private/audit-round2-race-adversarial.md.
//
// Retried on a transient SQLITE_BUSY (see withBusyRetry) — without that,
// two processes landing their commit within the same few milliseconds can
// both fail outright with "database is locked" (reproduced live, see
// docs/private/audit-round3-race-adversarial-followup.md), which for a
// redeem/transfer/consolidate whose wire call already succeeded would mean
// real, already-moved money with zero local record of it — the same
// blast radius as the original lost-update bug, via a different mechanism.
func (l *Ledger) Save() error {
	return withBusyRetry(l.save)
}

// save is Save's single, non-retrying attempt.
func (l *Ledger) save() error {
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, e := range l.Entries {
		if prior, ok := l.loaded[e.ID]; ok && entriesEqual(prior, e) {
			continue
		}
		var relayURLs sql.NullString
		if len(e.RelayURLs) > 0 {
			b, err := json.Marshal(e.RelayURLs)
			if err != nil {
				return fmt.Errorf("cashctl.db: encoding relay_urls for %s: %w", e.ID, err)
			}
			relayURLs = sql.NullString{String: string(b), Valid: true}
		}
		var identityRequired sql.NullInt64
		if e.IdentityRequired != nil {
			v := int64(0)
			if *e.IdentityRequired {
				v = 1
			}
			identityRequired = sql.NullInt64{Int64: v, Valid: true}
		}
		var amountMillis sql.NullInt64
		if e.AmountMillis != nil {
			amountMillis = sql.NullInt64{Int64: int64(*e.AmountMillis), Valid: true}
		}
		_, err := tx.Exec(`INSERT INTO entries (id, token, wallet_pubkey, minter_pubkey, secret, relay_urls,
			identity_required, amount_millis, received_at, verified, status, bearer_secret,
			connection_key_platform, connection_key_external_id, attestation_event_id, ia_pubkey)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				token = excluded.token, wallet_pubkey = excluded.wallet_pubkey,
				minter_pubkey = excluded.minter_pubkey, secret = excluded.secret,
				relay_urls = excluded.relay_urls, identity_required = excluded.identity_required,
				amount_millis = excluded.amount_millis, received_at = excluded.received_at,
				verified = excluded.verified, status = excluded.status,
				bearer_secret = excluded.bearer_secret,
				connection_key_platform = excluded.connection_key_platform,
				connection_key_external_id = excluded.connection_key_external_id,
				attestation_event_id = excluded.attestation_event_id, ia_pubkey = excluded.ia_pubkey`,
			e.ID, e.Token, e.WalletPubkey, e.MinterPubkey, e.Secret, relayURLs,
			identityRequired, amountMillis, e.ReceivedAt, e.Verified, e.Status, e.BearerSecret,
			e.ConnectionKeyPlatform, e.ConnectionKeyExternalID, e.AttestationEventID, e.IAPubkey)
		if err != nil {
			// entries.id collisions are handled by ON CONFLICT above — the
			// only other constraint this table has is token's own UNIQUE,
			// so a constraint failure reaching here specifically means
			// this exact token was saved by a concurrent `cashctl receive`
			// (same shape Add's own in-memory check already guards for the
			// non-concurrent case: two processes both Load before either
			// Save, so neither's in-memory check sees the other's token
			// yet — this is that same collision, just caught at the DB
			// instead) — surfaced as the same ErrAlreadyHeld a caller
			// already knows how to handle, not a raw SQL constraint
			// message.
			if isSQLiteConstraint(err) {
				return ErrAlreadyHeld
			}
			return fmt.Errorf("cashctl.db: saving entry %s: %w", e.ID, err)
		}
	}

	if l.historyLoaded < 0 || l.historyLoaded > len(l.History) {
		l.historyLoaded = 0 // defensive — shouldn't happen, never trust a stale index over safety
	}
	for _, h := range l.History[l.historyLoaded:] {
		if _, err := tx.Exec(`INSERT INTO history (at, action, detail) VALUES (?, ?, ?)`, h.At, h.Action, h.Detail); err != nil {
			return fmt.Errorf("cashctl.db: saving history: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Re-baseline against what was just committed — a no-op for every
	// current caller (each only calls Save once per process), but keeps a
	// hypothetical second Save on the same *Ledger correct rather than
	// silently relying on that never happening.
	l.loaded = make(map[string]Entry, len(l.Entries))
	for _, e := range l.Entries {
		l.loaded[e.ID] = e
	}
	l.historyLoaded = len(l.History)
	return nil
}

// entriesEqual is save's own "did this row actually change" check —
// reflect.DeepEqual with one deliberate normalization: a nil RelayURLs and
// an empty-but-non-nil RelayURLs{} compare equal. reflect.DeepEqual(nil,
// []string{}) is false in Go, so without this, an entry whose RelayURLs
// happened to differ only in nilness from what Load saw — while otherwise
// completely unchanged — would be judged "changed" and have its WHOLE row
// rewritten from this process's own in-memory copy, including any other
// field a concurrent process may have changed since this Ledger's own
// Load. That's the exact lost-update shape round 2 fixed (see Save's own
// doc comment), reopened for a field this process never intended to touch.
// Not reachable by any command today — RelayURLs is set once at Add time
// from the decoded token (cmd/cash_receive.go) and nipcash's own decoder
// never produces a non-nil empty slice (it only ever appends), and no code
// anywhere reassigns RelayURLs on an already-loaded entry — but cheap
// enough to guard against unconditionally rather than rely on that holding
// forever. See ledger_test.go's TestSave_NilVsEmptyRelayURLsRoundTrip.
func entriesEqual(a, b Entry) bool {
	if len(a.RelayURLs) == 0 && len(b.RelayURLs) == 0 {
		a.RelayURLs, b.RelayURLs = nil, nil
	}
	return reflect.DeepEqual(a, b)
}

// isSQLiteBusy reports whether err is a transient SQLITE_BUSY ("database is
// locked") — the failure mode load/save can hit when another process (or,
// within this package's own tests, another goroutine) holds cashctl.db's
// write lock at the exact same instant. internal/store.Open sets no
// busy_timeout and every Load/Save gets its own fresh connection, so with
// no retry this fails immediately instead of waiting the lock out. 5 ==
// SQLITE_BUSY, sqlite3.h's own stable numeric code; masked with 0xff so an
// extended busy sub-code (e.g. SQLITE_BUSY_SNAPSHOT) still matches, since
// those share the low byte with the primary code.
func isSQLiteBusy(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == 5
}

// isSQLiteConstraint reports whether err is a SQLITE_CONSTRAINT failure
// (any flavor — UNIQUE, NOT NULL, ...; masked with 0xff same as
// isSQLiteBusy). 19 == SQLITE_CONSTRAINT, sqlite3.h's own stable numeric
// code. Only entries.save's own insert calls this, where entries.id
// collisions are already handled by ON CONFLICT — the only constraint
// that can still fail there is token's own UNIQUE, so this doesn't need
// to (and via the driver's plain error code, can't cheaply) distinguish
// further.
func isSQLiteConstraint(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == 19
}

// withBusyRetry runs op, retrying a bounded number of times on a transient
// isSQLiteBusy error before giving up and returning it. SQLite's write lock
// is only ever held for the length of one transaction commit — a handful of
// local INSERTs, no network I/O inside it — so a short backoff is enough to
// let a concurrent process's own load/save clear. Retrying the WHOLE
// operation (not just one statement) is safe: on any error, save's own
// transaction is rolled back (defer tx.Rollback()) before this wrapper ever
// sees it, so a retried attempt starts from a clean slate, and save's
// upserts/append-only history writes are themselves idempotent — replaying
// a successful one is a no-op modulo re-deriving the same values.
func withBusyRetry(op func() error) error {
	const maxAttempts = 25
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err = op(); err == nil || !isSQLiteBusy(err) {
			return err
		}
		time.Sleep(time.Duration(2+attempt) * time.Millisecond)
	}
	return err
}

// ErrAlreadyHeld is returned by Add when token has already been recorded.
var ErrAlreadyHeld = errors.New("this token is already in your ledger")

// FindByToken looks up an entry by its original token string — used to
// reject re-receiving the same token twice.
func (l *Ledger) FindByToken(token string) (*Entry, bool) {
	for i := range l.Entries {
		if l.Entries[i].Token == token {
			return &l.Entries[i], true
		}
	}
	return nil, false
}

// Find looks up an entry by its short local ID (e.g. "tok-8e21").
func (l *Ledger) Find(id string) (*Entry, bool) {
	for i := range l.Entries {
		if l.Entries[i].ID == id {
			return &l.Entries[i], true
		}
	}
	return nil, false
}

// Held returns every entry whose Status is StatusHeld — the pool `redeem`/
// `transfer`/`consolidate` auto-pick from when no local ID is given
// explicitly.
func (l *Ledger) Held() []Entry {
	var held []Entry
	for _, e := range l.Entries {
		if e.Status == StatusHeld {
			held = append(held, e)
		}
	}
	return held
}

// Add records a new entry, generating a short local ID and setting
// ReceivedAt/Status/Verified. Rejects a token already in the ledger.
func (l *Ledger) Add(e Entry) (*Entry, error) {
	if _, ok := l.FindByToken(e.Token); ok {
		return nil, ErrAlreadyHeld
	}
	e.ID = l.newID()
	e.ReceivedAt = nowRFC3339()
	e.Status = StatusHeld
	l.Entries = append(l.Entries, e)
	return &l.Entries[len(l.Entries)-1], nil
}

// SetStatus transitions an entry's status (e.g. to StatusRedeemed once
// cash_redeem succeeds) — a redeemed/transferred/consolidated token is
// never removed outright, only marked, so `wallet history`/an audit trail
// stays intact.
func (l *Ledger) SetStatus(id, status string) error {
	for i := range l.Entries {
		if l.Entries[i].ID == id {
			l.Entries[i].Status = status
			return nil
		}
	}
	return fmt.Errorf("no held token %q", id)
}

// SetVerified marks an entry as Hub-cross-checked — every entry `cashctl
// receive` itself saves is already verified this way; this setter exists
// for resolveAmount's own on-demand discovery of an entry that wasn't
// (e.g. a transfer's split remainder).
func (l *Ledger) SetVerified(id string, verified bool) error {
	for i := range l.Entries {
		if l.Entries[i].ID == id {
			l.Entries[i].Verified = verified
			return nil
		}
	}
	return fmt.Errorf("no held token %q", id)
}

// AppendHistory adds one line to the local action log.
func (l *Ledger) AppendHistory(action, detail string) {
	l.History = append(l.History, HistoryEntry{At: nowRFC3339(), Action: action, Detail: detail})
}

// newID generates a short, human-typeable local ID in the "tok-xxxx" shape
// shown throughout cashctl-plan.md's walkthroughs, retrying on the
// astronomically unlikely collision with an existing entry.
func (l *Ledger) newID() string {
	for {
		var b [2]byte
		_, _ = rand.Read(b[:])
		id := "tok-" + hex.EncodeToString(b[:])
		if _, ok := l.Find(id); !ok {
			return id
		}
	}
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
