//go:build integration

// command_coverage_test.go runs, through the compiled binary, the commands,
// flags and environment variables nothing else in this suite exercised: the
// skills and version commands, every --as credential form and CASHCTL_AS,
// "-" for a secret on stdin, NO_COLOR, NCLI_VAULT_PASSWORD, the literal
// connection: target, consolidate --ia, the canonical `cash ...`/`wallet ...`
// forms on a live flow, and a redeem --all where one bill fails.
package integration

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	nipIC "github.com/ohstr/nmilat/nipIC"
	"github.com/ohstr/nmilat/nipcash"
)

// identityPrivHex reads f's local identity key straight from cashctl.db.
func identityPrivHex(t *testing.T, f *fixture) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.configDir, "cashctl.db"))
	if err != nil {
		t.Fatalf("open cashctl.db: %v", err)
	}
	defer db.Close()
	var priv string
	if err := db.QueryRow(`SELECT priv_hex FROM identity LIMIT 1`).Scan(&priv); err != nil {
		t.Fatalf("read identity: %v", err)
	}
	return priv
}

// replaceIdentityPrivHex swaps f's stored identity key for priv, so only a
// credential passed with --as can still spend what the old key holds.
func replaceIdentityPrivHex(t *testing.T, f *fixture, priv string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.configDir, "cashctl.db"))
	if err != nil {
		t.Fatalf("open cashctl.db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE identity SET priv_hex = ?`, priv); err != nil {
		t.Fatalf("replace identity: %v", err)
	}
}

func requireOK(t *testing.T, step string, res result) map[string]any {
	t.Helper()
	if res.ExitCode != 0 {
		t.Fatalf("%s: exit %d\nstdout: %s\nstderr: %s", step, res.ExitCode, res.Stdout, res.Stderr)
	}
	return mustDecodeJSON(t, step, res.Stdout)
}

func TestSkills_ListShowInstall(t *testing.T) {
	f := newFixture(t)

	var list []map[string]any
	if err := json.Unmarshal([]byte(f.run("skills", "list").Stdout), &list); err != nil || len(list) == 0 {
		t.Fatalf("skills list --json: %v (%d skills)", err, len(list))
	}
	names := map[string]bool{}
	for _, s := range list {
		name, _ := s["name"].(string)
		names[name] = true
	}
	for _, want := range []string{"cashctl-cash", "cashctl-circle", "cashctl-wallet"} {
		if !names[want] {
			t.Errorf("skills list lacks %s: %v", want, names)
		}
	}

	shown := f.mustJSON("skills", "show", "cashctl-cash")
	if content, _ := shown["content"].(string); !strings.Contains(content, "cashctl receive") {
		t.Errorf("skills show cashctl-cash: content does not look like its SKILL.md")
	}
	requireFailure(t, "skills show nope", f.run("skills", "show", "nope"), 4, "not_found", "")
	requireFailure(t, "skills install without --dir", f.run("skills", "install"), 2, "usage", "")

	dir := t.TempDir()
	statuses := func() map[string]string {
		out := f.mustJSON("skills", "install", "--dir", dir)
		rows, _ := out["skills"].([]any)
		got := map[string]string{}
		for _, r := range rows {
			row, _ := r.(map[string]any)
			got[row["name"].(string)], _ = row["status"].(string)
		}
		return got
	}
	for name, status := range statuses() {
		if status != "installed" {
			t.Errorf("first install: %s = %s, want installed", name, status)
		}
	}
	for name, status := range statuses() {
		if status != "unchanged" {
			t.Errorf("second install: %s = %s, want unchanged", name, status)
		}
	}

	// A symlink planted where a skill file goes is replaced, not written through.
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	skillFile := filepath.Join(dir, "cashctl-cash", "SKILL.md")
	if err := os.Remove(skillFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, skillFile); err != nil {
		t.Fatal(err)
	}
	f.mustJSON("skills", "install", "--dir", dir)
	if got, _ := os.ReadFile(victim); string(got) != "keep me" {
		t.Errorf("skills install wrote through the planted symlink: %q", got)
	}
}

func TestVersion(t *testing.T) {
	f := newFixture(t)
	if v, _ := f.mustJSON("version")["version"].(string); v == "" {
		t.Error("version --json: empty version")
	}
	if res := f.runInteractive("", "version"); res.ExitCode != 0 || strings.TrimSpace(res.Stdout) == "" {
		t.Errorf("version: exit %d stdout %q", res.ExitCode, res.Stdout)
	}
}

// TestNoColor runs an error under a pseudo-terminal, where cashctl colors
// its "Error:" prefix unless NO_COLOR is set. Over a pipe it never colors, so
// only a terminal tells the two apart.
func TestNoColor(t *testing.T) {
	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("skipping: no `script` to run cashctl under a pseudo-terminal")
	}
	f := newFixture(t)
	underTTY := func(noColor bool) string {
		cmdline := f.bin + " --config-dir " + f.configDir + " decode not-a-token"
		cmd := exec.Command("script", "-qec", cmdline, "/dev/null")
		cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+f.xdgHome)
		cmd.Env = append(cmd.Env, "NO_COLOR=")
		if noColor {
			cmd.Env = append(cmd.Env, "NO_COLOR=1")
		}
		out, _ := cmd.CombinedOutput()
		return string(out)
	}
	if out := underTTY(false); !strings.Contains(out, "\x1b[") {
		t.Skipf("skipping: no color even without NO_COLOR under this pseudo-terminal: %q", out)
	}
	if out := underTTY(true); strings.Contains(out, "\x1b[") {
		t.Errorf("NO_COLOR=1 still printed ANSI escapes: %q", out)
	}
}

func TestWalletBalance_GetBalanceAliasIsTheSame(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	f.mustJSON("receive", mintCashGift(t, hub, 20_000))

	balance := f.mustJSON("wallet", "balance")
	alias := f.mustJSON("wallet", "get-balance")
	if balance["total_mloki"] == nil || balance["total_mloki"] != alias["total_mloki"] {
		t.Errorf("wallet get-balance total = %v, wallet balance total = %v", alias["total_mloki"], balance["total_mloki"])
	}
}

// TestCashStatus_AllListsCoRecipients: by default a holder sees only their
// own slice; --all asks for the whole roster.
func TestCashStatus_AllListsCoRecipients(t *testing.T) {
	_, hub := plainClientHub(t)
	f, pub := newMember(t)
	_, otherPub := newIdentity(t)
	id := entryID(t, f.mustJSON("receive", mintTwoRecipientBill(t, hub, pub, otherPub, 3_000, 2_000))["entry"])

	recipients := func(args ...string) []any {
		out := f.mustJSON(append([]string{"cash", "status", "--token", id}, args...)...)
		r, _ := out["recipients"].([]any)
		return r
	}
	if mine := recipients(); len(mine) != 1 {
		t.Errorf("cash status: %d recipients, want just this holder's slice", len(mine))
	}
	all := recipients("--all")
	if len(all) != 2 {
		t.Fatalf("cash status --all: %d recipients, want both", len(all))
	}
	found := false
	for _, r := range all {
		if row, _ := r.(map[string]any); row["identity_value"] == otherPub {
			found = true
		}
	}
	if !found {
		t.Errorf("cash status --all does not list the co-recipient %s: %v", otherPub, all)
	}
}

// TestAsCredential_CashSecret: with the stored cash secret wrong, only the
// credential passed in --as or CASHCTL_AS can spend the holding, and the
// flag wins when both are set.
func TestAsCredential_CashSecret(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)

	held := func(t *testing.T) (*fixture, string, string) {
		f := newFixture(t)
		f.mustJSON("wallet", "init")
		id := entryID(t, f.mustJSON("receive", mintCashGift(t, hub, 30_000))["entry"])
		secret := currentCashSecret(t, f, id)
		if secret == "" {
			t.Skip("no cash secret stored after receive — nothing to override")
		}
		corruptCashSecret(t, f, id, "not-the-real-secret", "")
		return f, id, secret
	}

	t.Run("redeem --as", func(t *testing.T) {
		f, id, secret := held(t)
		requireOK(t, "connect add", f.run("connect", "add", "payout", hub.PairingUri))
		requireOK(t, "redeem --as cash:", f.run("redeem", "--token", id, "--into", "payout", "--as", "cash:"+secret, "--yes"))
	})
	t.Run("CASHCTL_AS", func(t *testing.T) {
		f, id, secret := held(t)
		out := requireOK(t, "transfer via CASHCTL_AS", f.runWith([]string{"CASHCTL_AS=cash:" + secret}, "", "transfer", "10", "cash", "--token", id, "--yes"))
		if gift, _ := out["cash_to_send"].(string); !strings.Contains(gift, "#") {
			t.Errorf("no spendable cash note: %v", out)
		}
	})
	t.Run("--as wins over CASHCTL_AS", func(t *testing.T) {
		f, id, secret := held(t)
		res := f.runWith([]string{"CASHCTL_AS=cash:" + secret}, "", "transfer", "10", "cash", "--token", id, "--as", "cash:"+strings.Repeat("ab", 32), "--yes")
		if res.ExitCode == 0 {
			t.Fatalf("transfer spent with CASHCTL_AS's secret although --as named another: %s", res.Stdout)
		}
	})
}

// TestAsCredential_Pubkey: with the stored identity key replaced, a pubkey
// holding is spendable only through --as pubkey:<the original key>.
func TestAsCredential_Pubkey(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f, pub := newMember(t)
	priv := identityPrivHex(t, f)
	redeemID := entryID(t, f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 20_000))["entry"])
	transferID := entryID(t, f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 20_000))["entry"])
	replaceIdentityPrivHex(t, f, randomHex32(t))
	requireOK(t, "connect add", f.run("connect", "add", "payout", hub.PairingUri))

	if res := f.run("redeem", "--token", redeemID, "--into", "payout", "--yes"); res.ExitCode == 0 {
		t.Fatalf("redeem with the wrong stored key succeeded, so --as below proves nothing: %s", res.Stdout)
	}
	requireOK(t, "redeem --as pubkey:", f.run("redeem", "--token", redeemID, "--into", "payout", "--as", "pubkey:"+priv, "--yes"))

	_, recipientPub := newIdentity(t)
	out := requireOK(t, "transfer --as pubkey:", f.run("transfer", "10", recipientPub, "--token", transferID, "--as", "pubkey:"+priv, "--yes"))
	if tok, _ := out["recipient_token"].(string); tok == "" {
		t.Errorf("transfer --as pubkey: no recipient_token: %v", out)
	}
}

// TestStdinDash takes each secret-bearing argument from stdin with "-".
func TestStdinDash(t *testing.T) {
	admin := adminOrSkip(t)
	cashHub := setUpCashHub(t, admin)
	f, pub := newMember(t)

	gift := mintCashGift(t, cashHub, 20_000)
	received := requireOK(t, "receive -", f.runWith(nil, gift+"\n", "receive", "-"))
	if entryID(t, received["entry"]) == "" {
		t.Errorf("receive -: no entry: %v", received)
	}

	requireOK(t, "connect add -", f.runWith(nil, cashHub.PairingUri+"\n", "connect", "add", "payout", "-"))
	if list, _ := f.mustJSON("connect", "list")["connections"].([]any); len(list) != 1 {
		t.Errorf("connect add -: connections = %v, want the one read from stdin", list)
	}

	circleHub := setUpCircleHub(t, admin, pub)
	requireOK(t, "join -", f.runWith(nil, *circleHub.CircleHubToken+"\n", "join", "-", "10"))

	requireFailure(t, "receive - with empty stdin", f.run("receive", "-"), 2, "usage", "")
}

// TestVaultIdentity_SignsUnattendedWithPassword: a vault-backed identity
// signs under --json only when NCLI_VAULT_PASSWORD unlocks the vault.
func TestVaultIdentity_SignsUnattendedWithPassword(t *testing.T) {
	admin := adminOrSkip(t)
	f := newFixture(t)
	npub := seedNcliVault(t, f, "personal")
	if res := f.runInteractive("y\n", "init"); res.ExitCode != 0 || !strings.Contains(res.Stdout, npub) {
		t.Fatalf("init adopting the vault identity: exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	pub, err := npubToHex(npub)
	if err != nil {
		t.Fatal(err)
	}
	hub := setUpCircleHub(t, admin, pub)

	res := f.run("join", *hub.CircleHubToken, "10")
	if res.ExitCode == 0 || !strings.Contains(res.Stderr, "NCLI_VAULT_PASSWORD") {
		t.Fatalf("join without NCLI_VAULT_PASSWORD: exit %d, want a refusal naming it\nstderr: %s", res.ExitCode, res.Stderr)
	}
	requireOK(t, "join with NCLI_VAULT_PASSWORD", f.runWith([]string{"NCLI_VAULT_PASSWORD=vault-password-for-test"}, "", "join", *hub.CircleHubToken, "10"))
}

// TestConnectionTarget_LiteralFormAndConsolidateIA sends to a connection-key
// recipient through the literal connection:<platform>:<id>:<ia> target, and
// merges two bills straight to an nconnection1 target with --ia.
func TestConnectionTarget_LiteralFormAndConsolidateIA(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	ia := newTrustedIA(t, admin)
	f, pub := newMember(t)
	platform, externalID := nipIC.WebIdentity("discord"), "77310045"

	id := entryID(t, f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 20_000))["entry"])
	target := "connection:" + string(platform) + ":" + externalID + ":" + ia.PubHex
	out := requireOK(t, "transfer to connection:", f.run("transfer", "10", target, "--token", id, "--yes"))
	if tok, _ := out["recipient_token"].(string); tok == "" {
		t.Errorf("transfer to %s: no recipient_token: %v", target, out)
	}

	a := entryID(t, f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 20_000))["entry"])
	b := entryID(t, f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 20_000))["entry"])
	nconn, err := nipIC.EncodeNConnection(mustConnKey(t, platform, externalID), []string{"wss://relay.invalid"}, platform)
	if err != nil {
		t.Fatal(err)
	}
	merged := requireOK(t, "consolidate --to nconnection --ia", f.run("consolidate", a, b, "--to", nconn, "--ia", ia.PubHex, "--yes"))
	if resolved, _ := merged["target_resolved"].(string); !strings.Contains(resolved, ia.PubHex) {
		t.Errorf("consolidate --ia: target_resolved = %q, want it to name the IA", resolved)
	}
}

// TestCanonicalForms_LiveFlow drives a whole holding through the canonical,
// fully-namespaced commands rather than their shortcuts.
func TestCanonicalForms_LiveFlow(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f, pub := newMember(t)
	requireOK(t, "connect add", f.run("connect", "add", "payout", hub.PairingUri))

	a := entryID(t, requireOK(t, "cash receive", f.run("cash", "receive", mintPubkeyTokenFromHub(t, hub, pub, 30_000)))["entry"])
	b := entryID(t, requireOK(t, "cash receive", f.run("cash", "receive", mintPubkeyTokenFromHub(t, hub, pub, 30_000)))["entry"])
	merged := requireOK(t, "cash consolidate", f.run("cash", "consolidate", a, b, "--yes"))
	newEntry, _ := merged["new_entry"].(map[string]any)
	id := entryID(t, newEntry)
	_, recipientPub := newIdentity(t)
	requireOK(t, "cash transfer", f.run("cash", "transfer", "10", recipientPub, "--token", id, "--yes"))
	requireOK(t, "cash redeem", f.run("cash", "redeem", "--all", "--into", "payout", "--yes"))
	requireOK(t, "wallet invoice", f.run("wallet", "invoice", "5"))
	requireOK(t, "wallet balance", f.run("wallet", "balance"))

	// The human-readable forms of the read-only views print something and exit 0.
	for _, args := range [][]string{{"wallet", "history"}, {"wallet", "show"}, {"wallet", "balance"}} {
		if res := f.runInteractive("", args...); res.ExitCode != 0 || strings.TrimSpace(res.Stdout) == "" {
			t.Errorf("%v (text): exit %d, stdout %q\nstderr: %s", args, res.ExitCode, res.Stdout, res.Stderr)
		}
	}
}

// TestRedeemAll_OneBillFailing reports every bill and exits nonzero when one
// of them cannot be redeemed (AGENTS.md §Partial success).
func TestRedeemAll_OneBillFailing(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f, pub := newMember(t)
	requireOK(t, "connect add", f.run("connect", "add", "payout", hub.PairingUri))

	good := mintPubkeyTokenFromHub(t, hub, pub, 20_000)
	gone := mintPubkeyTokenFromHub(t, hub, pub, 20_000)
	f.mustJSON("receive", good)
	goneID := entryID(t, f.mustJSON("receive", gone)["entry"])

	tok, err := nipcash.Decode(gone)
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.deleteCashWallet(hub.ID, walletAppIDForPubkey(t, admin, hub.ID, tok.WalletPubkey)); err != nil {
		t.Fatalf("reclaim one bill hub-side: %v", err)
	}

	res := f.run("redeem", "--all", "--into", "payout", "--yes")
	if res.ExitCode == 0 {
		t.Fatalf("redeem --all with one bill gone: exit 0, want nonzero\nstdout: %s", res.Stdout)
	}
	out := mustDecodeJSON(t, "redeem --all", res.Stdout)
	rows, _ := out["redeemed"].([]any)
	if len(rows) != 2 {
		t.Fatalf("redeemed = %v, want a row per bill", out["redeemed"])
	}
	statuses := map[string]string{}
	for _, r := range rows {
		row, _ := r.(map[string]any)
		statuses[row["token"].(string)], _ = row["status"].(string)
	}
	ok, failed := 0, 0
	for _, s := range statuses {
		switch s {
		case "ok":
			ok++
		case "failed":
			failed++
		}
	}
	if ok != 1 || failed != 1 || statuses[goneID] != "failed" {
		t.Errorf("row statuses = %v, want the reclaimed bill %s failed and the other ok", statuses, goneID)
	}
}
