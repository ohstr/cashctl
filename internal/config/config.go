// Package config manages cashctl's multi-wallet inventory: named connections
// (both ones cashctl produced itself — a join result, a redeem destination —
// and foreign ones added via `connect add`) plus a single default pointer
// (see cashctl-plan.md's "Multi-wallet inventory & defaults").
package config

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/ohstr/cashctl/internal/store"
)

// Connection is one named wallet/hub connection cashctl knows about — either
// a raw nostr+walletconnect:// URI, or a bech32 string
// (cashhub1.../circlehub1.../lokicash1...) — stored exactly as given.
// JSON tags matter here even though this is no longer the on-disk shape:
// `connect list`/`wallet show` embed []Connection directly into their
// --json output (cmd/connect.go, cmd/wallet.go) — the --json contract
// doesn't change as part of the storage-layer swap.
type Connection struct {
	Name string `json:"name"`
	// Value is the connection's own actual pairing secret (an NWC URI's
	// secret= query value, or the equivalent embedded in a bech32 hub
	// string's own TLV encoding — every connection kind this field can
	// hold carries one) — json:"-" for the same reason
	// ledger.Entry.Secret/CashSecret are: `connect list`/`wallet show`
	// embed a whole []Connection directly into --json output, so a plain
	// json tag here would leak it to anyone who ever runs the documented
	// way to list registered wallets. Text mode already never prints it
	// either (cmd/connect.go's own list loop only ever prints c.Name) —
	// this makes --json match that, not introduce a new restriction.
	Value   string `json:"-"`
	AddedAt string `json:"added_at"`

	// LastKnownBalanceMloki/LastKnownBalanceAt cache the most recent
	// successful live get_balance result — the only way to show a figure
	// for an expired wallet at all, since get_balance itself is one of the
	// money-moving scopes an expired wallet rejects (only get_info/
	// get_budget survive expiry). `cashctl balance` falls back to this,
	// flagged as stranded, when a live call fails specifically with
	// EXPIRED.
	LastKnownBalanceMloki *int64 `json:"last_known_balance_mloki,omitempty"`
	LastKnownBalanceAt    string `json:"last_known_balance_at,omitempty"`
}

// Store is the in-memory shape of cashctl.db's connections table, loaded
// whole and saved as a diff against what Load read, like ledger.Ledger (see
// its own doc comment) — Default is stored as an is_default column on
// whichever row is current, not a separate table, since it's a property of
// exactly one connection.
type Store struct {
	Connections []Connection
	Default     string

	// loaded/loadedDefault are what Load actually read (by name) — Save
	// diffs against them so a process only ever writes what IT changed.
	// nil/"" on a Store built directly (&Store{}) rather than via Load:
	// everything in Connections is then treated as new, as it is.
	loaded        map[string]Connection
	loadedDefault string
}

// snapshot records s's current contents as the baseline the next Save
// diffs against. Pointer fields are copied, not shared, so a later
// in-place mutation of a live Connection can't silently rewrite its own
// baseline and make a real change look like none.
func (s *Store) snapshot() {
	s.loaded = make(map[string]Connection, len(s.Connections))
	for _, c := range s.Connections {
		if c.LastKnownBalanceMloki != nil {
			v := *c.LastKnownBalanceMloki
			c.LastKnownBalanceMloki = &v
		}
		s.loaded[c.Name] = c
	}
	s.loadedDefault = s.Default
}

// connectionsEqual reports whether a and b hold the same data — a plain ==
// would compare LastKnownBalanceMloki by pointer identity.
func connectionsEqual(a, b Connection) bool {
	if (a.LastKnownBalanceMloki == nil) != (b.LastKnownBalanceMloki == nil) {
		return false
	}
	if a.LastKnownBalanceMloki != nil && *a.LastKnownBalanceMloki != *b.LastKnownBalanceMloki {
		return false
	}
	return a.Name == b.Name && a.Value == b.Value && a.AddedAt == b.AddedAt &&
		a.LastKnownBalanceAt == b.LastKnownBalanceAt
}

// Load reads every connection from cashctl.db, returning an empty (not
// nil) *Store if there are none yet — a fresh install has no connections,
// not an error condition.
func Load() (*Store, error) {
	db, err := store.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	// ORDER BY rowid, not added_at (see ledger.Load's identical comment) —
	// added_at only has 1-second precision, so two connections added in
	// the same second would otherwise sort by name instead of reliably
	// preserving insertion order.
	rows, err := db.Query(`SELECT name, value, added_at, last_known_balance_mloki, last_known_balance_at, is_default
		FROM connections ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("cashctl.db: reading connections: %w", err)
	}
	defer func() { _ = rows.Close() }()

	s := &Store{}
	for rows.Next() {
		var c Connection
		var lastKnownBalance sql.NullInt64
		var lastKnownBalanceAt sql.NullString
		var isDefault bool
		if err := rows.Scan(&c.Name, &c.Value, &c.AddedAt, &lastKnownBalance, &lastKnownBalanceAt, &isDefault); err != nil {
			return nil, fmt.Errorf("cashctl.db: reading connections: %w", err)
		}
		if lastKnownBalance.Valid {
			v := lastKnownBalance.Int64
			c.LastKnownBalanceMloki = &v
		}
		c.LastKnownBalanceAt = lastKnownBalanceAt.String
		if isDefault {
			s.Default = c.Name
		}
		s.Connections = append(s.Connections, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cashctl.db: reading connections: %w", err)
	}
	s.snapshot()
	return s, nil
}

// Save writes s's changes to cashctl.db in one transaction: only what
// differs from what Load read (see Store's own doc comment) —
//
//   - a connection that wasn't there at Load is INSERTed, and a name a
//     concurrent process registered since then surfaces as ErrDuplicateName
//     rather than being silently overwritten;
//   - one that changed is UPDATEd in place (keeping its position);
//   - one that was there at Load and is gone now is DELETEd;
//   - the default pointer is only touched if THIS process changed it.
//
// Anything else — rows another process added or changed in the meantime —
// is left alone. This used to DELETE the whole table and reinsert its own
// snapshot, so two concurrent `connect add`s each loaded the same empty
// set and whichever saved last silently erased the other's connection.
// Retried on a transient SQLITE_BUSY like ledger.Save.
func (s *Store) Save() error {
	if err := store.WithBusyRetry(s.save); err != nil {
		return err
	}
	s.snapshot()
	return nil
}

func (s *Store) save() error {
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

	present := make(map[string]bool, len(s.Connections))
	for _, c := range s.Connections {
		present[c.Name] = true
		prior, existed := s.loaded[c.Name]
		if existed && connectionsEqual(prior, c) {
			continue
		}
		var lastKnownBalance sql.NullInt64
		if c.LastKnownBalanceMloki != nil {
			lastKnownBalance = sql.NullInt64{Int64: *c.LastKnownBalanceMloki, Valid: true}
		}
		if !existed {
			if _, err := tx.Exec(`INSERT INTO connections (name, value, added_at, last_known_balance_mloki, last_known_balance_at, is_default)
				VALUES (?, ?, ?, ?, ?, 0)`,
				c.Name, c.Value, c.AddedAt, lastKnownBalance, c.LastKnownBalanceAt); err != nil {
				if store.IsUniqueViolation(err) {
					return ErrDuplicateName
				}
				return fmt.Errorf("cashctl.db: saving connection %s: %w", c.Name, err)
			}
			continue
		}
		if _, err := tx.Exec(`UPDATE connections SET value = ?, added_at = ?, last_known_balance_mloki = ?, last_known_balance_at = ?
			WHERE name = ?`,
			c.Value, c.AddedAt, lastKnownBalance, c.LastKnownBalanceAt, c.Name); err != nil {
			return fmt.Errorf("cashctl.db: saving connection %s: %w", c.Name, err)
		}
	}
	for name := range s.loaded {
		if present[name] {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM connections WHERE name = ?`, name); err != nil {
			return fmt.Errorf("cashctl.db: removing connection %s: %w", name, err)
		}
	}
	if s.Default != s.loadedDefault {
		if _, err := tx.Exec(`UPDATE connections SET is_default = 0 WHERE is_default != 0`); err != nil {
			return fmt.Errorf("cashctl.db: clearing default: %w", err)
		}
		if s.Default != "" {
			if _, err := tx.Exec(`UPDATE connections SET is_default = 1 WHERE name = ?`, s.Default); err != nil {
				return fmt.Errorf("cashctl.db: setting default: %w", err)
			}
		}
	}
	return tx.Commit()
}

// Find looks up a connection by name.
func (s *Store) Find(name string) (*Connection, bool) {
	for i := range s.Connections {
		if s.Connections[i].Name == name {
			return &s.Connections[i], true
		}
	}
	return nil, false
}

// ErrDuplicateName is returned by Add when name is already taken.
var ErrDuplicateName = errors.New("a connection with that name already exists")

// Add records a new connection under name. Does not touch Default — see
// SetDefault and cashctl-plan.md's "registering a new wallet never silently
// changes the default" rule; callers decide separately whether to also
// call SetDefault (e.g. after prompting the user, or automatically for the
// very first connection — see IsEmpty).
func (s *Store) Add(name, value string) error {
	if _, ok := s.Find(name); ok {
		return ErrDuplicateName
	}
	s.Connections = append(s.Connections, Connection{Name: name, Value: value, AddedAt: nowRFC3339()})
	return nil
}

// Remove deletes the named connection, and clears Default if it pointed at
// the one being removed.
func (s *Store) Remove(name string) bool {
	for i := range s.Connections {
		if s.Connections[i].Name == name {
			s.Connections = append(s.Connections[:i], s.Connections[i+1:]...)
			if s.Default == name {
				s.Default = ""
			}
			return true
		}
	}
	return false
}

// IsEmpty reports whether this is a fresh inventory with no connections
// yet — the one case cashctl-plan.md's default-pointer rule inverts from "ask
// [y/N]" to "ask [Y/n]" (a first wallet has no existing default to
// protect).
func (s *Store) IsEmpty() bool {
	return len(s.Connections) == 0
}

// SetDefault points Default at name. Returns an error if name isn't a
// known connection.
func (s *Store) SetDefault(name string) error {
	if _, ok := s.Find(name); !ok {
		return fmt.Errorf("no connection named %q", name)
	}
	s.Default = name
	return nil
}

// SetLastKnownBalance caches a successful live get_balance result against
// the named connection (see Connection's own doc comment).
func (s *Store) SetLastKnownBalance(name string, mloki int64) {
	if c, ok := s.Find(name); ok {
		c.LastKnownBalanceMloki = &mloki
		c.LastKnownBalanceAt = nowRFC3339()
	}
}

// DefaultConnection returns the current default connection, or false if
// none is set.
func (s *Store) DefaultConnection() (*Connection, bool) {
	if s.Default == "" {
		return nil, false
	}
	return s.Find(s.Default)
}

// slugPattern matches characters SuggestName keeps from a hint — letters,
// digits, and hyphens; everything else (spaces, punctuation from a
// human-entered label like "Ada's Family Circle") is dropped rather than
// erroring, since a hint is advisory, not user input to validate.
var slugPattern = regexp.MustCompile(`[^a-zA-Z0-9-]+`)

// SuggestName returns an available "<prefix>:<slug>" name derived from
// hint (e.g. a decoded circlehub1... token's own Label field), falling
// back to "<prefix>:<n>" if hint is empty, slugifies to nothing, or
// collides with an existing connection.
func (s *Store) SuggestName(prefix, hint string) string {
	slug := strings.ToLower(strings.Trim(slugPattern.ReplaceAllString(hint, "-"), "-"))
	if slug != "" {
		candidate := prefix + ":" + slug
		if _, ok := s.Find(candidate); !ok {
			return candidate
		}
	}
	for n := 1; ; n++ {
		candidate := fmt.Sprintf("%s:%d", prefix, n)
		if _, ok := s.Find(candidate); !ok {
			return candidate
		}
	}
}
