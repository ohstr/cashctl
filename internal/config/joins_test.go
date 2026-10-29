package config

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ohstr/cashctl/internal/store"
)

func rotationRow() Join {
	return Join{ConnectionPubkey: strings.Repeat("a1", 32), ConnectionSecret: strings.Repeat("b2", 32),
		HubPubkey: strings.Repeat("c3", 32), MemberPubkey: strings.Repeat("d4", 32), HubConnection: "circlehub1x",
		HubIdentity: strings.Repeat("e5", 32), Chain: "bitcoin", Deadline: 1_800_000_000, DeviceID: "dev",
		Purpose: JoinPurposeRotation, WalletPubkey: strings.Repeat("f6", 32), WalletName: "family",
		Event: json.RawMessage(`{"kind":23199,"content":"a signed request","tags":[["p","c3"]]}`)}
}

// A rotation row keeps its purpose, wallet and signed request in the terms
// column as {"purpose":"rotation","wallet_pubkey":…,"event":…}, and reads back
// exactly; its state moves like a join's.
func TestRotationRow_RoundTrip(t *testing.T) {
	withTempConfigDir(t)
	in := rotationRow()
	if err := WriteJoin(in); err != nil {
		t.Fatal(err)
	}
	got, ok, err := FindJoin(in.ConnectionPubkey)
	if err != nil || !ok {
		t.Fatalf("FindJoin: %v %v", ok, err)
	}
	if !got.IsRotation() || got.WalletPubkey != in.WalletPubkey || got.WalletName != in.WalletName || !bytes.Equal(got.Event, in.Event) {
		t.Fatalf("read back %+v", got)
	}
	var terms map[string]json.RawMessage
	if err := json.Unmarshal([]byte(got.Terms), &terms); err != nil || len(terms) != 3 ||
		string(terms["purpose"]) != `"rotation"` || string(terms["wallet_pubkey"]) != `"`+in.WalletPubkey+`"` {
		t.Errorf("terms column %s, want the documented three keys", got.Terms)
	}
	if open, ok, _ := OpenRotation(in.WalletPubkey); !ok || open.ConnectionPubkey != in.ConnectionPubkey {
		t.Error("OpenRotation does not find it")
	}
	if err := SetJoinState(in.ConnectionPubkey, JoinDone, "", 0); err != nil {
		t.Fatal(err)
	}
	if after, _, _ := FindJoin(in.ConnectionPubkey); !after.IsRotation() || after.State != JoinDone {
		t.Errorf("after its state moved: %+v", after)
	}
	if _, ok, _ := OpenRotation(in.WalletPubkey); ok {
		t.Error("a finished rotation is still open")
	}
}

// A join's terms column is never read as a rotation, whatever else it holds.
func TestJoinRow_IsNeverARotation(t *testing.T) {
	withTempConfigDir(t)
	j := rotationRow()
	j.Purpose, j.WalletPubkey, j.Event = "", "", nil
	j.Terms = `{"max_amount_millis":1000,"network":"mainnet"}`
	if err := WriteJoin(j); err != nil {
		t.Fatal(err)
	}
	got, _, _ := FindJoin(j.ConnectionPubkey)
	if got.IsRotation() || got.WalletPubkey != "" || got.Terms != j.Terms {
		t.Errorf("a join read as %+v", got)
	}
	for _, terms := range []string{`{"purpose":"rotate"}`, `not json`, ``} {
		db, err := store.Open()
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec(`UPDATE joins SET terms = ? WHERE connection_pubkey = ?`, sql.NullString{String: terms, Valid: terms != ""}, j.ConnectionPubkey)
		_ = db.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got, _, _ := FindJoin(j.ConnectionPubkey); got.IsRotation() {
			t.Errorf("terms %q read as a rotation", terms)
		}
	}
}

// A rotation row written by 0.7.0-rc.1 (schema 3) reads the same after the
// migration to schema 4.
func TestRotationRow_SurvivesTheSchema3Migration(t *testing.T) {
	withTempConfigDir(t)
	in := rotationRow()
	if err := WriteJoin(in); err != nil {
		t.Fatal(err)
	}
	// Make the file what 0.7.0-rc.1 left: version 3, without the version-4
	// columns and index.
	path, _ := store.Path()
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`UPDATE meta SET value = '3' WHERE key = 'schema_version'`,
		`DROP INDEX idx_entries_live_wallet`,
		`ALTER TABLE discover_events DROP COLUMN relay`,
		`ALTER TABLE withdrawals DROP COLUMN receipt_invalid_seen`,
	} {
		if _, err := raw.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	_ = raw.Close()
	got, ok, err := FindJoin(in.ConnectionPubkey)
	if err != nil || !ok || !got.IsRotation() || got.WalletPubkey != in.WalletPubkey || !bytes.Equal(got.Event, in.Event) || got.ConnectionSecret != in.ConnectionSecret {
		t.Fatalf("after the migration: %+v %v %v", got, ok, err)
	}
}
