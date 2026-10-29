package store

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nipcash"

	"github.com/ohstr/cashctl/internal/testmint"
)

// schemaV3 is the version-3 schema cashctl 0.7.0-rc.1 created: entries keyed
// by their token string.
func schemaV3(t *testing.T) string {
	t.Helper()
	s := strings.Replace(schema, "token                      TEXT NOT NULL,", "token                      TEXT NOT NULL UNIQUE,", 1)
	s = strings.Replace(s, liveWalletIndex, "", 1)
	s = strings.Replace(s, "fetched_at INTEGER NOT NULL,\n\trelay      TEXT\n", "fetched_at INTEGER NOT NULL\n", 1)
	s = strings.Replace(s, "recovered          INTEGER NOT NULL DEFAULT 0,\n\treceipt_invalid_seen INTEGER NOT NULL DEFAULT 0\n", "recovered          INTEGER NOT NULL DEFAULT 0\n", 1)
	if strings.Contains(s, "receipt_invalid_seen") || strings.Contains(s, "relay      TEXT") {
		t.Fatal("setup: the v3 schema still has a version-4 column")
	}
	if s == schema {
		t.Fatal("setup: the v3 schema is the current one")
	}
	return s
}

// v3Database writes a version-3 cashctl.db with rows, each (id, token,
// wallet, status, received_at, cash_protection, throwaway_key).
func v3Database(t *testing.T, rows [][7]string, openCallSources string) {
	t.Helper()
	path, _ := Path()
	raw, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(schemaV3(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO meta (key, value) VALUES ('schema_version', '3')`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO discover_events (id, kind, pubkey, created_at, raw, origin, fetched_at) VALUES ('ev1', 31190, 'p', 1, '{}', 'trusted', 1)`); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if _, err := raw.Exec(`INSERT INTO entries (id, token, wallet_pubkey, secret, received_at, verified, status, cash_protection, throwaway_key, cash_secret)
			VALUES (?, ?, ?, 's', ?, 1, ?, NULLIF(?, ''), NULLIF(?, ''), 'secret-of-'||?)`, r[0], r[1], r[2], r[4], r[3], r[5], r[6], r[0]); err != nil {
			t.Fatalf("v3 row %s: %v", r[0], err)
		}
	}
	if openCallSources != "" {
		if _, err := raw.Exec(`INSERT INTO pending_calls (call_id, request_id, method, purpose, deadline, sent_h, hub_identity, target,
			target_credential, device_id, amount_millis, chain, authorizer_kind, source_entry_ids, params_json, state, created_at, updated_at)
			VALUES ('c1', 'r1', 'cash_transfer', 'transfer', 0, 0, 'h', 't', 'tc', 'd', 0, 'bitcoin', 'local_identity', ?, '{}', 'lookup', 0, 0)`, openCallSources); err != nil {
			t.Fatal(err)
		}
	}
}

type migrated struct{ token, wallet, status, cashSecret string }

func readEntries(t *testing.T, db *sql.DB) map[string]migrated {
	t.Helper()
	rows, err := db.Query(`SELECT id, token, wallet_pubkey, status, COALESCE(cash_secret, '') FROM entries`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]migrated{}
	for rows.Next() {
		var id string
		var m migrated
		if err := rows.Scan(&id, &m.token, &m.wallet, &m.status, &m.cashSecret); err != nil {
			t.Fatal(err)
		}
		out[id] = m
	}
	return out
}

func TestOpen_MigratesV3_OneLiveRowPerBill(t *testing.T) {
	withTempConfigDir(t)
	a := testmint.Bill(t, nipcash.Token{}, "")
	b := testmint.Bill(t, nipcash.Token{}, "")
	c := testmint.Bill(t, nipcash.Token{}, "")
	d := testmint.Bill(t, nipcash.Token{}, "")
	e := testmint.Bill(t, nipcash.Token{}, "")
	w := func(s string) string { tok, _ := nipcash.Decode(s); return tok.WalletPubkey }
	v3Database(t, [][7]string{
		// a: held twice under two spellings; the later receive stays live.
		{"tok-a1", a, w(a), "held", "2026-10-20T10:00:00Z", "", ""},
		{"tok-a2", testmint.WithUnknownTLV(t, a), w(a), "held", "2026-10-21T10:00:00Z", "", ""},
		// b: the protected copy stays live, whatever the order received.
		{"tok-b1", b, w(b), "held", "2026-10-20T10:00:00Z", "protected", ""},
		{"tok-b2", testmint.Reordered(t, b), w(b), "held", "2026-10-21T10:00:00Z", "shared", ""},
		// c: the copy an open call spends stays live.
		{"tok-c1", c, w(c), "held", "2026-10-20T10:00:00Z", "", ""},
		{"tok-c2", testmint.WithUnknownTLV(t, c), w(c), "redeeming", "2026-10-21T10:00:00Z", "", "k"},
		// d: one row only, stored in another spelling: only rewritten.
		{"tok-d", strings.ToUpper(testmint.Reordered(t, d)), w(d), "held", "2026-10-20T10:00:00Z", "", ""},
		// e: a copy let go and a live one: both stay as they are.
		{"tok-e1", e, w(e), "transferred", "2026-10-20T10:00:00Z", "", ""},
		{"tok-e2", testmint.WithUnknownTLV(t, e), w(e), "held", "2026-10-21T10:00:00Z", "", ""},
		// a string no decoder reads is left alone.
		{"tok-x", "lokicash1notabill", "wx", "held", "2026-10-20T10:00:00Z", "", ""},
	}, `["tok-c1"]`)

	db, err := Open()
	if err != nil {
		t.Fatalf("Open() of a v3 database: %v", err)
	}
	defer func() { _ = db.Close() }()
	var v string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v); err != nil || v != SchemaVersion {
		t.Fatalf("schema_version = %q (%v), want %s", v, err, SchemaVersion)
	}
	got := readEntries(t, db)
	want := map[string]struct{ token, status string }{
		"tok-a1": {a, "duplicate"}, "tok-a2": {a, "held"},
		"tok-b1": {b, "held"}, "tok-b2": {b, "duplicate"},
		"tok-c1": {c, "held"}, "tok-c2": {c, "duplicate"},
		"tok-d":  {d, "held"},
		"tok-e1": {e, "transferred"}, "tok-e2": {e, "held"},
		"tok-x": {"lokicash1notabill", "held"},
	}
	if len(got) != len(want) {
		t.Fatalf("%d rows after the migration, want %d (no row is deleted)", len(got), len(want))
	}
	for id, w := range want {
		g := got[id]
		if g.token != w.token || g.status != w.status {
			t.Errorf("%s: token %s.. status %s; want %s.. %s", id, g.token[:16], g.status, w.token[:16], w.status)
		}
		if g.cashSecret != "secret-of-"+id {
			t.Errorf("%s: cash secret %q lost", id, g.cashSecret)
		}
	}
	var notes int
	_ = db.QueryRow(`SELECT COUNT(*) FROM history WHERE action = 'migrate'`).Scan(&notes)
	if notes != 3 {
		t.Errorf("%d migrate history lines, want 3 (one per row set aside)", notes)
	}

	// From now on the database itself holds one live row per bill.
	if _, err := db.Exec(`UPDATE entries SET status = 'held' WHERE id = 'tok-a1'`); err == nil {
		t.Error("a second live row of one bill was accepted after the migration")
	}
	if _, err := db.Exec(`UPDATE entries SET status = 'held' WHERE id = 'tok-e1'`); err == nil {
		t.Error("a let-go copy was made live beside the held one")
	}
	if _, err := db.Exec(`INSERT INTO entries (id, token, wallet_pubkey, secret, received_at, verified, status)
		VALUES ('tok-new', ?, ?, 's', 'now', 1, 'held')`, testmint.Bill(t, nipcash.Token{}, ""), "another-wallet"); err != nil {
		t.Errorf("another bill was refused after the migration: %v", err)
	}
	_ = db.Close()

	// A second Open finds version 4 and changes nothing.
	db2, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db2.Close() }()
	if again := readEntries(t, db2); len(again) != len(want)+1 || again["tok-a1"].status != "duplicate" {
		t.Errorf("re-opening changed the migrated rows: %v", again["tok-a1"])
	}
}

func TestOpen_MigratesAnEmptyV3Database(t *testing.T) {
	withTempConfigDir(t)
	v3Database(t, nil, "")
	db, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	// A cached event is kept, its relay unknown, and a new one records its relay.
	var relay sql.NullString
	if err := db.QueryRow(`SELECT relay FROM discover_events WHERE id = 'ev1'`).Scan(&relay); err != nil || relay.Valid {
		t.Errorf("cached event after the migration: relay %v (%v), want NULL", relay, err)
	}
	if _, err := db.Exec(`INSERT INTO discover_events (id, kind, pubkey, created_at, raw, origin, fetched_at, relay) VALUES ('ev2', 1, 'p', 1, '{}', 'trusted', 1, 'wss://r')`); err != nil {
		t.Errorf("a new cached event with its relay: %v", err)
	}
	var seen int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('withdrawals') WHERE name = 'receipt_invalid_seen'`).Scan(&seen); err != nil || seen != 1 {
		t.Errorf("withdrawals.receipt_invalid_seen after the migration: %d (%v)", seen, err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name IN ('idx_entries_live_wallet', 'idx_entries_status_minter')`).Scan(&n); err != nil || n != 2 {
		t.Errorf("indexes after the migration = %d (%v), want 2", n, err)
	}
}
