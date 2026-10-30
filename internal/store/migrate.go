package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ohstr/cashctl/internal/billform"
)

// liveStatuses are the entry statuses that keep a bill in the ledger's hands
// (ledger.Live); at most one row per wallet pubkey may hold one.
var liveStatuses = map[string]bool{"held": true, "redeeming": true, "zap_claiming": true}

// v3Entry is what migrateV3 reads of one version-3 entries row.
type v3Entry struct {
	rowid                                   int64
	id, token, wallet, status, receivedAt   string
	oldStatus                               string
	throwaway, pendingThrowaway, protection string
	newToken, newWallet                     string
	referenced                              bool
}

// migrateV3 brings a version-3 database to version 4 inside initSchema's
// transaction. Version 3 keyed entries by their token string, so one bill
// received under two spellings (entries reordered, an unknown TLV added) was
// held twice and counted twice. Every token is rewritten in its one spelling
// (billform.Canonical) with the wallet pubkey it names; where one bill has
// several live rows, one stays live (survivorFirst) and each other becomes
// "duplicate": counted nowhere, its secrets kept. The token's UNIQUE goes, and
// idx_entries_live_wallet allows one live row per wallet pubkey from then on.
// discover_events gains the relay that first served each event, withdrawals
// a count of answers in a row without a valid receipt.
func migrateV3(ctx context.Context, conn *sql.Conn) error {
	entries, err := readV3Entries(ctx, conn)
	if err != nil {
		return err
	}
	byWallet := map[string][]*v3Entry{}
	for _, e := range entries {
		if liveStatuses[e.status] && e.newWallet != "" {
			byWallet[e.newWallet] = append(byWallet[e.newWallet], e)
		}
	}
	var setAside []string
	for _, group := range byWallet {
		if len(group) < 2 {
			continue
		}
		sort.SliceStable(group, func(i, j int) bool { return survivorFirst(group[i], group[j]) })
		for _, d := range group[1:] {
			d.status = "duplicate"
			setAside = append(setAside, fmt.Sprintf("%s (kept %s)", d.id, group[0].id))
		}
	}

	for _, stmt := range []string{
		`ALTER TABLE entries RENAME TO entries_v3`,
		`DROP INDEX IF EXISTS idx_entries_status_minter`,
		entriesTableV4(),
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	cols, err := columnsOf(ctx, conn, "entries")
	if err != nil {
		return err
	}
	list := strings.Join(cols, ", ")
	if _, err := conn.ExecContext(ctx, `INSERT INTO entries (`+list+`) SELECT `+list+` FROM entries_v3 ORDER BY rowid`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `DROP TABLE entries_v3`); err != nil {
		return err
	}
	for _, e := range entries {
		if e.newToken == e.token && e.newWallet == e.wallet && e.status == e.oldStatus {
			continue
		}
		if _, err := conn.ExecContext(ctx, `UPDATE entries SET token = ?, wallet_pubkey = ?, status = ?, version = version + 1 WHERE id = ?`,
			e.newToken, e.newWallet, e.status, e.id); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `CREATE INDEX idx_entries_status_minter ON entries(status, minter_pubkey)`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, liveWalletIndex); err != nil {
		return err
	}
	sort.Strings(setAside)
	for _, d := range setAside {
		if _, err := conn.ExecContext(ctx, `INSERT INTO history (at, action, detail) VALUES (?, 'migrate', ?)`,
			time.Now().UTC().Format(time.RFC3339), "set aside "+d+": the same bill was stored twice under two spellings"); err != nil {
			return err
		}
	}
	// The relay that first served a cached event, a recommendation's hint.
	if _, err := conn.ExecContext(ctx, `ALTER TABLE discover_events ADD COLUMN relay TEXT`); err != nil {
		return err
	}
	// How many answers in a row carried no valid receipt (a withdrawal).
	if _, err := conn.ExecContext(ctx, `ALTER TABLE withdrawals ADD COLUMN receipt_invalid_seen INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	// Set the version migrateV3 actually produces (4), not SchemaVersion:
	// initSchema then chains migrateV4 to carry a v3 database the rest of the
	// way to the current schema. A literal, since this step's output is frozen
	// at 4 even as SchemaVersion moves on.
	_, err = conn.ExecContext(ctx, `UPDATE meta SET value = '4' WHERE key = 'schema_version'`)
	return err
}

// migrateV4 brings a version-4 database to version 5 inside initSchema's
// transaction: the entries table gains identity_pubkey, the pubkey a
// pubkey-mode slice is addressed to (R-CP-RECLAIM). Existing rows backfill to
// NULL — "unknown, can't tie": cmd/spent.go's proofOf treats an empty addressee
// as other_key, so a NOT_FOUND on a legacy holding fails SAFE to unknown (never
// terminal reclaimed), while a row received after this column records the key the
// slice was addressed to and gains the full rotated-key safety.
func migrateV4(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, `ALTER TABLE entries ADD COLUMN identity_pubkey TEXT`); err != nil {
		return err
	}
	// Freeze the literal version migrateV4 produces (5), not SchemaVersion, which
	// moves on as later migrations are added (matches migrateV3's '4').
	_, err := conn.ExecContext(ctx, `UPDATE meta SET value = '5' WHERE key = 'schema_version'`)
	return err
}

// readV3Entries reads every entry with its one spelling, and marks the rows an
// open call spends.
func readV3Entries(ctx context.Context, conn *sql.Conn) ([]*v3Entry, error) {
	rows, err := conn.QueryContext(ctx, `SELECT rowid, id, token, wallet_pubkey, status, received_at,
		COALESCE(throwaway_key, ''), COALESCE(pending_throwaway_key, ''), COALESCE(cash_protection, '')
		FROM entries ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	var out []*v3Entry
	byID := map[string]*v3Entry{}
	for rows.Next() {
		e := &v3Entry{}
		if err := rows.Scan(&e.rowid, &e.id, &e.token, &e.wallet, &e.status, &e.receivedAt,
			&e.throwaway, &e.pendingThrowaway, &e.protection); err != nil {
			_ = rows.Close()
			return nil, err
		}
		e.oldStatus = e.status
		e.newToken, e.newWallet = strings.ToLower(e.token), e.wallet
		if c, err := billform.Canonical(e.token); err == nil {
			e.newToken = c
			e.newWallet, _ = billform.WalletPubkey(c)
		}
		out = append(out, e)
		byID[e.id] = e
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	calls, err := conn.QueryContext(ctx, `SELECT source_entry_ids FROM pending_calls
		WHERE state NOT IN ('done', 'not_applied', 'replaced', 'closed')`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = calls.Close() }()
	for calls.Next() {
		var raw string
		if err := calls.Scan(&raw); err != nil {
			return nil, err
		}
		var ids []string
		_ = json.Unmarshal([]byte(raw), &ids)
		for _, id := range ids {
			if e, ok := byID[id]; ok {
				e.referenced = true
			}
		}
	}
	return out, calls.Err()
}

// survivorFirst orders the live rows of one bill, the one to keep live first:
// one an open call spends; then the one this wallet re-keyed furthest (a
// merge's throwaway key, one written ahead, a protected cash secret), since
// re-keying is the only local change of what spends a bill; then the one
// received last, whose secret the Hub accepted most recently.
func survivorFirst(a, b *v3Entry) bool {
	if a.referenced != b.referenced {
		return a.referenced
	}
	if ra, rb := rekeyRank(a), rekeyRank(b); ra != rb {
		return ra > rb
	}
	if a.receivedAt != b.receivedAt {
		return a.receivedAt > b.receivedAt
	}
	return a.rowid > b.rowid
}

func rekeyRank(e *v3Entry) int {
	switch {
	case e.throwaway != "":
		return 3
	case e.pendingThrowaway != "":
		return 2
	case e.protection == "protected":
		return 1
	}
	return 0
}

// columnsOf lists table's columns in their declared order.
func columnsOf(ctx context.Context, conn *sql.Conn, table string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// entriesTableV4 is the version-4 entries table, cut from the current schema
// with the columns added after v4 stripped back out (today just
// identity_pubkey, added by migrateV4), the same way migrate_test.go's schemaV3
// derives an older schema. migrateV3 must create the table EXACTLY as v4 had
// it: it copies the v3 rows by the new table's own column list, so a column v3
// never had would make that SELECT fail — the later migrateV4 adds
// identity_pubkey back on top.
func entriesTableV4() string {
	start := strings.Index(schema, "CREATE TABLE entries (")
	end := strings.Index(schema[start:], ");") + start + 2
	ddl := schema[start:end]
	return strings.Replace(ddl, "\tidentity_pubkey            TEXT,\n", "", 1)
}
