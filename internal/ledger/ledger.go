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
	"strings"
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

// BearerProtection values — see Entry.BearerProtection's own doc comment.
const (
	BearerShared    = "shared"
	BearerProtected = "protected"
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

	// PendingBearerSecret is a not-yet-confirmed replacement for
	// BearerSecret, written BEFORE the wire call that's meant to make it
	// the real one (see cmd/receive_secure.go's protectBearerReceipt) —
	// generating a bearer target's secret is a purely local operation
	// (NIP-CASH §Bearer Slices: only a commitment ever goes over the
	// wire), so it's known before the call is even placed. A kill or lost
	// response during that call leaves this genuinely ambiguous — NIP-CASH
	// has no read-only way to ask the Hub which of the two secrets it
	// accepted (nipcashclient.CheckClaim's own doc comment: a bearer match
	// only proves *some* recipient exists, never *which* secret) — so
	// resolveCredential retries a decline with this value instead of
	// trusting BearerSecret alone. Empty once reconciled either way. Same
	// leak sensitivity as BearerSecret itself.
	PendingBearerSecret string `json:"-"`

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
	// invalidly-signed token has no recoverable minter identity at all).
	// "Valid" here is deliberately narrow: VerifyProvenance uses ECDSA
	// signature RECOVERY (ecdsa.RecoverCompact), not verification against
	// any known/trusted key — it proves the signature is internally
	// self-consistent with WalletPubkey+AttestedAmountMillis and recovers
	// WHICH pubkey produced it, never that the recovered pubkey is a Hub
	// cashctl (or its user) has any reason to trust. Anyone can mint-sign
	// their own token with a disposable key and get MinterPubkey set to
	// that key, "valid" and all — this field is a same-signer grouping
	// signal, not an authenticity guarantee (see formatMinterStatus's own
	// doc comment in cmd/decode.go for the exact user-facing wording this
	// constrains). Populated once, at receive time, from the same
	// VerifyProvenance call already made for display (see
	// cash_receive.go's printCashBill) — this is cash-selection's only
	// client-side signal for "these held tokens were signed by the same
	// key" (docs/ux-review.md Part 2).
	//
	// A token cashctl itself derives from verified ones (a split's
	// remainder, a consolidate's merged output) is a brand-new wallet no
	// mint signature can verify against, so it INHERITS its sources'
	// minter instead (cmd's sharedMinter) — still nil unless every source
	// was verified and they all agree.
	MinterPubkey *string `json:"minter_pubkey,omitempty"`

	// ExpiresAt is the Hub-side redemption deadline (unix seconds) as of
	// the most recent live check — receive's own mandatory CheckClaim, or
	// a later resolveAmount/resolveRedeemQuote refresh — nil if never
	// learned (e.g. a split remainder cashctl minted itself, which has no
	// CheckClaim call of its own until something needs its amount). Not
	// re-verified continuously: a token can expire on the Hub between one
	// command and the next without this field changing, but it's the only
	// local signal `balance` has to keep an expired-and-worthless token
	// out of the total without a live round trip per held token on every
	// call.
	ExpiresAt *int64 `json:"expires_at,omitempty"`

	// BearerProtection reports whether a bearer-mode entry's BearerSecret
	// is exclusively known to this ledger (BearerProtected, re-keyed by
	// cmd/receive_secure.go's protectBearerReceipt/protectRekeyOnly or a
	// later manual `wallet protect`) or still the one embedded in the
	// original token/gift string, spendable by anyone else who was shown
	// it too (BearerShared) — the whole reason `receive` offers to
	// protect a bearer gift automatically. Empty ("") for a pubkey-mode
	// entry, where this concept doesn't apply at all: no bearer secret,
	// nothing to protect. Set once at receive time (BearerShared for
	// every bearer-mode entry, whatever happens to the automatic protect
	// offer next) and updated to BearerProtected on a successful re-key —
	// before this field existed, a declined/failed protect left the exact
	// same Entry shape as a genuinely protected one, with no way to tell
	// them apart later (`wallet show`, `wallet history`, ...).
	BearerProtection string `json:"bearer_protection,omitempty"`
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
// transient SQLITE_BUSY (see store.WithBusyRetry) — store.Open's own schema
// migration is a write and can collide with a concurrent process's Save.
func Load() (*Ledger, error) {
	var l *Ledger
	err := store.WithBusyRetry(func() error {
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
	defer func() { _ = db.Close() }()

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
		connection_key_platform, connection_key_external_id, attestation_event_id, ia_pubkey,
		pending_bearer_secret, expires_at, bearer_protection
		FROM entries ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("cashctl.db: reading entries: %w", err)
	}
	for rows.Next() {
		var e Entry
		var relayURLs, pendingBearerSecret, bearerProtection sql.NullString
		var identityRequired, amountMillis, expiresAt sql.NullInt64
		if err := rows.Scan(&e.ID, &e.Token, &e.WalletPubkey, &e.MinterPubkey, &e.Secret, &relayURLs,
			&identityRequired, &amountMillis, &e.ReceivedAt, &e.Verified, &e.Status, &e.BearerSecret,
			&e.ConnectionKeyPlatform, &e.ConnectionKeyExternalID, &e.AttestationEventID, &e.IAPubkey,
			&pendingBearerSecret, &expiresAt, &bearerProtection); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("cashctl.db: reading entries: %w", err)
		}
		// sql.NullString, not a plain string like BearerSecret's own scan
		// above: every row's bearer_secret has been through this code's own
		// Save (never legacy-NULL), but pending_bearer_secret/bearer_protection
		// are columns ADDED after rows already existed (store.addColumnsIfMissing)
		// — those rows genuinely have SQL NULL here, which Scan can't take
		// directly into a plain string.
		e.PendingBearerSecret = pendingBearerSecret.String
		e.BearerProtection = bearerProtection.String
		if relayURLs.Valid && relayURLs.String != "" {
			if err := json.Unmarshal([]byte(relayURLs.String), &e.RelayURLs); err != nil {
				_ = rows.Close()
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
		if expiresAt.Valid {
			v := expiresAt.Int64
			e.ExpiresAt = &v
		}
		l.Entries = append(l.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cashctl.db: reading entries: %w", err)
	}
	_ = rows.Close()

	histRows, err := db.Query(`SELECT at, action, detail FROM history ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("cashctl.db: reading history: %w", err)
	}
	defer func() { _ = histRows.Close() }()
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
// Retried on a transient SQLITE_BUSY (see store.WithBusyRetry) — without that,
// two processes landing their commit within the same few milliseconds can
// both fail outright with "database is locked" (reproduced live, see
// docs/private/audit-round3-race-adversarial-followup.md), which for a
// redeem/transfer/consolidate whose wire call already succeeded would mean
// real, already-moved money with zero local record of it — the same
// blast radius as the original lost-update bug, via a different mechanism.
func (l *Ledger) Save() error {
	return store.WithBusyRetry(l.save)
}

// save is Save's single, non-retrying attempt.
func (l *Ledger) save() error {
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

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
		var expiresAt sql.NullInt64
		if e.ExpiresAt != nil {
			expiresAt = sql.NullInt64{Int64: *e.ExpiresAt, Valid: true}
		}
		_, err := tx.Exec(`INSERT INTO entries (id, token, wallet_pubkey, minter_pubkey, secret, relay_urls,
			identity_required, amount_millis, received_at, verified, status, bearer_secret,
			connection_key_platform, connection_key_external_id, attestation_event_id, ia_pubkey,
			pending_bearer_secret, expires_at, bearer_protection)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				token = excluded.token, wallet_pubkey = excluded.wallet_pubkey,
				minter_pubkey = excluded.minter_pubkey, secret = excluded.secret,
				relay_urls = excluded.relay_urls, identity_required = excluded.identity_required,
				amount_millis = excluded.amount_millis, received_at = excluded.received_at,
				verified = excluded.verified, status = excluded.status,
				bearer_secret = excluded.bearer_secret,
				connection_key_platform = excluded.connection_key_platform,
				connection_key_external_id = excluded.connection_key_external_id,
				attestation_event_id = excluded.attestation_event_id, ia_pubkey = excluded.ia_pubkey,
				pending_bearer_secret = excluded.pending_bearer_secret,
				expires_at = excluded.expires_at,
				bearer_protection = excluded.bearer_protection`,
			e.ID, e.Token, e.WalletPubkey, e.MinterPubkey, e.Secret, relayURLs,
			identityRequired, amountMillis, e.ReceivedAt, e.Verified, e.Status, e.BearerSecret,
			e.ConnectionKeyPlatform, e.ConnectionKeyExternalID, e.AttestationEventID, e.IAPubkey,
			e.PendingBearerSecret, expiresAt, e.BearerProtection)
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

// isSQLiteConstraint reports whether err is SPECIFICALLY a UNIQUE
// constraint violation — 2067 == SQLITE_CONSTRAINT_UNIQUE, sqlite3.h's own
// stable extended result code (the driver's *sqlite.Error.Code() returns
// the full extended code, e.g. also SQLITE_IOERR's own many sub-codes
// elsewhere in this package's ErrorCodeString map — not just the coarse
// primary code masked with 0xff, which would conflate this with every
// other constraint flavor SQLite has: NOT NULL, CHECK, FOREIGN KEY, and —
// confirmed live — a trigger's own RAISE(ABORT, ...), all of which share
// the exact same primary code 19). entries.save's own insert calls this,
// where entries.id collisions are already handled by ON CONFLICT — the
// only constraint that can still fail there is token's own UNIQUE — but
// the check has to actually SAY that specifically, not just "some
// constraint fired": a caught-but-misclassified failure of a different
// kind (a future CHECK constraint, an operator-added trigger, or — this
// package's own withBusyRetry aside — any local write failure that
// happens to surface through SQLite's constraint machinery) would
// otherwise be silently reported as ErrAlreadyHeld below, telling the
// caller its money is safely held under a completely different name than
// what actually went wrong.
func isSQLiteConstraint(err error) bool {
	var sqliteErr *sqlite.Error
	const sqliteConstraintUnique = 2067
	return errors.As(err, &sqliteErr) && sqliteErr.Code() == sqliteConstraintUnique
}

// ErrAlreadyHeld is returned by Add when token has already been recorded.
var ErrAlreadyHeld = errors.New("this token is already in your ledger")

// FindByToken looks up an entry by its original token string — used to
// reject re-receiving the same token twice.
func (l *Ledger) FindByToken(token string) (*Entry, bool) {
	// Case-insensitive (see Add's own doc comment on why): a caller pasting
	// the uppercase spelling of an already-held token still matches it,
	// rather than looking like a brand new one. EqualFold rather than
	// lowercasing both sides so rows saved before Add started normalizing
	// (stored exactly as pasted) still match too.
	for i := range l.Entries {
		if strings.EqualFold(l.Entries[i].Token, token) {
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
// ReceivedAt/Status/Verified. Rejects a token already HELD — a token
// that's already in the ledger but no longer held (transferred, redeemed,
// consolidated away) instead REACTIVATES that same row (see below) rather
// than being rejected outright.
//
// e.Token is normalized to lowercase before either check: bech32 (BIP-173)
// is single-case by construction — a real encoder never mixes case — but
// case-INsensitive to decode, so the uppercase spelling of an already-held
// token is the exact same token, not a second one. Comparing/storing raw
// let it slip through as a distinct entry, silently doubling the token's
// counted balance.
func (l *Ledger) Add(e Entry) (*Entry, error) {
	e.Token = strings.ToLower(e.Token)
	if existing, ok := l.FindByToken(e.Token); ok {
		if existing.Status == StatusHeld {
			return nil, ErrAlreadyHeld
		}
		// The exact same token string came back — the one way this
		// happens legitimately is a full-amount transfer, which
		// reassigns a wallet's identity IN PLACE (NIP-CASH §Transferring
		// and Splitting a Slice) rather than minting a new token string,
		// so the very same string can genuinely be received again later
		// by whoever it comes back to. entries.token is UNIQUE in the
		// schema (rightly so — one wallet, one token string, however
		// many times its identity changes hands), so this reactivates
		// the SAME row (keeping its ID, so history/status transitions
		// recorded before this stay attributable) instead of trying to
		// INSERT a second one for it, which the schema would reject
		// regardless of what this in-memory check decided.
		id := existing.ID
		e.ID = id
		e.ReceivedAt = nowRFC3339()
		e.Status = StatusHeld
		*existing = e
		return existing, nil
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
