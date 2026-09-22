//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"bytes"
)

// Tests here pin the documented output/error contract in AGENTS.md
// ("Output conventions"): secrets are never printed in errors or listings,
// every error is one structured report on stderr with a consistent class,
// and what the binary writes to disk is private. They are offline (no lokihub
// needed) — each drives the compiled binary with fake, worthless values.

// rawRun runs the binary with EXACTLY args (unlike fixture.run, which always
// prepends --json), still isolated to the fixture's config dir / XDG home.
func (f *fixture) rawRun(args ...string) result {
	f.t.Helper()
	cmd := exec.Command(f.bin, args...)
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+f.xdgHome)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			f.t.Fatalf("running cashctl %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code}
}

// errorCode returns the "code" field of a --json error report on stderr.
func errorCode(t *testing.T, res result) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(res.Stderr), &body); err != nil {
		t.Fatalf("stderr is not a JSON error report: %v\nstderr: %s", err, res.Stderr)
	}
	return body.Code
}

// AGENTS.md: `input` is "never raw secret material ... a `pubkey:<privkey>`
// credential string has just its secret component blanked". A bad amount in
// one `--sources <token>:<amount>:<credential>` item must not echo the
// private key back.
func TestErrors_ConsolidateSourcesBadAmount_DoesNotEchoPrivateKey(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")
	priv := fakeHex32(t)
	tok := fakeCashToken(t, nil)
	res := f.run("consolidate", "--sources", tok+":notanumber:pubkey:"+priv+","+tok+":1:pubkey:"+priv)
	if res.ExitCode == 0 {
		t.Fatalf("consolidate with a non-numeric amount succeeded: %s", res.Stdout)
	}
	if strings.Contains(res.Stderr, priv) || strings.Contains(res.Stdout, priv) {
		t.Errorf("the private key of a pubkey:<privkey> credential was echoed back in the error:\n%s", res.Stderr)
	}
}

// Same promise for `--as`: a malformed credential must not be echoed with
// its secret intact.
func TestErrors_AsCredential_Malformed_DoesNotEchoSecret(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")
	secret := fakeHex32(t)
	for _, as := range []string{"cash:" + secret + "\n", "pubkey:" + secret + ",extra", "connection-key:" + secret + ",only-two"} {
		res := f.run("transfer", "1", "--as", as, "--yes")
		if strings.Contains(res.Stderr, secret) || strings.Contains(res.Stdout, secret) {
			t.Errorf("--as %q: the credential secret was echoed:\n%s", strings.ReplaceAll(as, secret, "<secret>"), res.Stderr)
		}
	}
}

// A registered wallet's connection URI carries its secret; listing wallets
// must not print it in either mode (`wallet show` embeds the same list).
func TestListings_DoNotPrintWalletConnectionSecret(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")
	secret := fakeHex32(t)
	uri := "nostr+walletconnect://" + fakeHex32(t) + "?relay=wss%3A%2F%2Ffake.invalid&secret=" + secret
	f.mustJSON("connect", "add", "w", uri)

	for name, res := range map[string]result{
		"connect list --json": f.run("connect", "list"),
		"connect list (text)": f.runInteractive("", "connect", "list"),
		"wallet show --json":  f.run("wallet", "show"),
		"wallet show (text)":  f.runInteractive("", "wallet", "show"),
	} {
		if res.ExitCode != 0 {
			t.Errorf("%s: exit %d: %s", name, res.ExitCode, res.Stderr)
			continue
		}
		if strings.Contains(res.Stdout, secret) || strings.Contains(res.Stderr, secret) {
			t.Errorf("%s printed the wallet connection secret (secret=<hex> in the URI)", name)
		}
	}
}

// "You haven't set that up yet" is one kind of condition and must have one
// error class — not_found, exit 4 (AGENTS.md lists "no wallet configured
// yet" there) — whichever thing is missing. A missing identity used to come
// back as `internal`, exit 1, from `join`. (`wallet show` deliberately works
// without an identity now: a cash-mode-only wallet has none.)
func TestErrors_MissingIdentity_SameClassEverywhere(t *testing.T) {
	f := newFixture(t) // never initialised: no identity, no wallet
	noIdentity := f.run("join", fakeCircleHubConnection(t), "10")
	noWallet := f.run("wallet", "get-info")
	if noIdentity.ExitCode == 0 || noWallet.ExitCode == 0 {
		t.Fatalf("expected both to fail: join exit %d, get-info exit %d", noIdentity.ExitCode, noWallet.ExitCode)
	}
	if a, b := errorCode(t, noIdentity), errorCode(t, noWallet); a != b || a != "not_found" {
		t.Errorf("no identity is reported as %q (exit %d) and no wallet as %q (exit %d) — both are 'not set up yet' and want not_found",
			a, noIdentity.ExitCode, b, noWallet.ExitCode)
	}
	if e := parseErrorReport(t, noIdentity); e.Retryable {
		t.Errorf("a missing identity is not retryable: running `cashctl init` fixes it, waiting does not")
	}
	if !strings.Contains(noIdentity.Stderr, "cashctl init") {
		t.Errorf("the error should name its remedy, `cashctl init`:\n%s", noIdentity.Stderr)
	}
}

// AGENTS.md: "a group command invoked without a subcommand" is a `usage`
// error, exit 2, reported on stderr — never a help dump on stdout, never 0.
func TestContract_BareGroupCommand_IsUsageError(t *testing.T) {
	f := newFixture(t)
	for _, group := range []string{"cash", "wallet", "connect", "circle"} {
		for _, mode := range []string{"--json", ""} {
			args := []string{"--config-dir", f.configDir, group}
			if mode != "" {
				args = append([]string{mode}, args...)
			}
			res := f.rawRun(args...)
			label := group + " " + mode
			if res.ExitCode != 2 {
				t.Errorf("`cashctl %s` (bare group): exit %d, want 2 (usage)", strings.TrimSpace(label), res.ExitCode)
			}
			if mode == "--json" && res.Stdout != "" {
				t.Errorf("`cashctl --json %s`: stdout must stay empty on a usage error, got %q", group, res.Stdout)
			}
		}
	}
}

// TestContract_InvocationErrors_ShowFullHelp verifies the UX for a
// genuinely malformed invocation — here, a missing required positional
// argument: the usual "Error: ..." line, followed by the command's own
// full --help content (same as `cashctl decode --help` prints, just
// redirected to stderr instead of cobra's own stdout default), in text
// mode only. Never under --json (an agent has no use for any of this human
// framing, and mixing it into stderr would break the single-structured-
// document contract TestContract_TextModeErrorsGoToStderrOnly/others
// already pin). This is InvocationError's contract
// (internal/output/errors.go) — deliberately narrower than every CodeUsage
// error: a runtime refusal that merely shares the usage exit code
// (transfer's "funds are fragmented", consolidate's "needs at least 2
// sources", ...) must keep the plain "Error: ..." line with no help
// appended (see UsageError's own doc comment) — not exercised here since
// those need live held tokens/a Hub, out of scope for this offline file;
// covered instead by internal/output's own unit tests.
func TestContract_InvocationErrors_ShowFullHelp(t *testing.T) {
	f := newFixture(t)

	textRes := f.rawRun("--config-dir", f.configDir, "decode")
	if textRes.ExitCode != 2 {
		t.Fatalf("cashctl decode (no arg): exit %d, want 2 (usage)", textRes.ExitCode)
	}
	if !strings.Contains(textRes.Stderr, "Error: accepts 1 arg(s), received 0") {
		t.Errorf("cashctl decode (no arg): stderr = %q, want the usual \"Error: ...\" line, unchanged", textRes.Stderr)
	}
	if !strings.Contains(textRes.Stderr, "Usage:\n  cashctl decode <string> [flags]") {
		t.Errorf("cashctl decode (no arg): stderr = %q, want the command's own Usage line", textRes.Stderr)
	}
	if !strings.Contains(textRes.Stderr, "--check") {
		t.Errorf("cashctl decode (no arg): stderr = %q, want the full Flags block (--check), not just a short synopsis", textRes.Stderr)
	}
	if !strings.Contains(textRes.Stderr, "Global Flags:") {
		t.Errorf("cashctl decode (no arg): stderr = %q, want the Global Flags section too — this is the same content --help prints", textRes.Stderr)
	}
	// Ordering: the error must stay the most prominent (first) line,
	// matching every CLI's own convention — the help block comes after it.
	msgIdx := strings.Index(textRes.Stderr, "accepts 1 arg(s), received 0")
	usageIdx := strings.Index(textRes.Stderr, "Usage:")
	if msgIdx < 0 || usageIdx < 0 || msgIdx > usageIdx {
		t.Errorf("cashctl decode (no arg): stderr = %q, want the message before the help block", textRes.Stderr)
	}

	jsonRes := f.rawRun("--json", "--config-dir", f.configDir, "decode")
	if jsonRes.ExitCode != 2 {
		t.Fatalf("cashctl --json decode (no arg): exit %d, want 2 (usage)", jsonRes.ExitCode)
	}
	if strings.Contains(jsonRes.Stderr, "--help") || strings.Contains(jsonRes.Stderr, "Usage:") {
		t.Errorf("cashctl --json decode (no arg): stderr = %q, want no help/usage framing under --json", jsonRes.Stderr)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(jsonRes.Stderr), &body); err != nil {
		t.Fatalf("cashctl --json decode (no arg): stderr is not valid JSON: %v\nstderr: %s", err, jsonRes.Stderr)
	}
}

// `--json=true` is the same flag as `--json`; a usage mistake with it must
// still produce the structured error and no stdout.
func TestContract_JSONEqualsTrue_UnknownCommandStillStructured(t *testing.T) {
	f := newFixture(t)
	for _, args := range [][]string{
		{"--json=true", "no-such-command"},
		{"--config-dir", f.configDir, "--json=true", "version", "--no-such-flag"},
	} {
		res := f.rawRun(args...)
		if res.ExitCode != 2 {
			t.Errorf("cashctl %v: exit %d, want 2", args, res.ExitCode)
		}
		if res.Stdout != "" {
			t.Errorf("cashctl %v: stdout must be empty on a usage error, got %q", args, res.Stdout)
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(res.Stderr), &body); err != nil || body["code"] != "usage" {
			t.Errorf("cashctl %v: stderr is not a structured usage error (--json=true was not honored): %q", args, res.Stderr)
		}
	}
}

// AGENTS.md: errors go to stderr, results to stdout — in text mode too.
func TestContract_TextModeErrorsGoToStderrOnly(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")
	for _, args := range [][]string{
		{"receive", "not-a-token"},
		{"decode", ""},
		{"transfer", "1"}, // nothing held
		{"wallet", "get-info"},
	} {
		res := f.runInteractive("", args...)
		if res.ExitCode == 0 {
			t.Errorf("cashctl %v unexpectedly succeeded", args)
			continue
		}
		if strings.TrimSpace(res.Stdout) != "" {
			t.Errorf("cashctl %v: an error leaked onto stdout: %q", args, res.Stdout)
		}
		if strings.TrimSpace(res.Stderr) == "" {
			t.Errorf("cashctl %v: failed with nothing on stderr", args)
		}
	}
}

// Everything the compiled binary writes under its config dir holds secrets
// (plaintext identity key, cash secrets, wallet URIs): no file may be
// group/world accessible, even under a permissive umask, and neither may a
// directory the binary itself created.
func TestConfigDir_BinaryCreatedFilesArePrivate(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)

	f := newFixture(t)
	nested := filepath.Join(f.configDir, "a", "b", "state") // created by the binary, not the harness
	run := func(args ...string) {
		t.Helper()
		full := append([]string{"--config-dir", nested, "--json"}, args...)
		cmd := exec.Command(f.bin, full...)
		cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+f.xdgHome)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("cashctl %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("connect", "add", "w", "nostr+walletconnect://"+fakeHex32(t)+"?relay=wss%3A%2F%2Ffake.invalid&secret="+fakeHex32(t))
	run("wallet", "show")

	err := filepath.Walk(filepath.Join(f.configDir, "a"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s is %v (%s) — group/world access on something under the private config dir",
				strings.TrimPrefix(path, f.configDir), perm, map[bool]string{true: "dir", false: "file"}[info.IsDir()])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// AGENTS.md: an error's `input` is "never raw secret material". A wallet
// connection URI (secret=<hex>) or a hub connection string pasted into the
// wrong command is exactly that; it must not be echoed back.
func TestErrors_InputNeverEchoesConnectionSecrets(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")
	secret := fakeHex32(t)
	nwc := "nostr+walletconnect://" + fakeHex32(t) + "?relay=wss%3A%2F%2Ffake.invalid&secret=" + secret
	cases := map[string][]string{
		"receive <nwc uri>":    {"receive", nwc},
		"decode <nwc uri>":     {"decode", nwc, "--check"},
		"connect use <uri>":    {"connect", "use", nwc},
		"connect rm <uri>":     {"connect", "rm", nwc},
		"balance --from <uri>": {"balance", "--from", nwc},
	}
	for name, args := range cases {
		res := f.run(args...)
		if strings.Contains(res.Stderr, secret) || strings.Contains(res.Stdout, secret) {
			t.Errorf("%s: the wallet connection secret was echoed back:\nstdout: %s\nstderr: %s", name, res.Stdout, res.Stderr)
		}
	}
}

// A cash gift (token#secret) or a Circle Hub connection string mis-pasted
// into `join` must not echo its own secret back — same class as
// TestErrors_InputNeverEchoesConnectionSecrets, different positional
// argument and a different secret shape (a gift's #<secret> suffix,
// rather than an NWC URI's secret= query value).
func TestErrors_JoinMisPaste_DoesNotEchoSecrets(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")
	secret := fakeHex32(t)
	gift := fakeCashToken(t, nil) + "#" + secret

	res := f.run("join", gift, "10")
	if strings.Contains(res.Stderr, secret) {
		t.Errorf("join <cash gift> echoed the secret:\nstderr: %s", res.Stderr)
	}
}

// join --as pubkey:<near-miss form> (leading space, uppercase prefix,
// quoting) must still be redacted — RedactSecretInput's own whole-string
// match used to require an exact, unpadded, lowercase "pubkey:" prefix.
func TestErrors_JoinAsPubkey_NearMissFormsStillRedacted(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")
	priv := fakeHex32(t)
	for _, as := range []string{" pubkey:" + priv, "PUBKEY:" + priv, priv + " "} {
		res := f.run("join", fakeCircleHubConnection(t), "10", "--as", as)
		if strings.Contains(res.Stderr, priv) {
			t.Errorf("join --as %q echoed the private key:\nstderr: %s", as, res.Stderr)
		}
	}
}

// A mistyped subcommand of a domain group (cash/wallet/connect/circle)
// used to print that group's own help and exit 0 — cobra's default
// fallback when a command has subcommands but no RunE of its own: unable
// to resolve the typo as a child, it invokes the PARENT's own Run with
// the typo as a positional arg instead of raising "unknown command".
func TestContract_TypoSubcommand_IsUsageError(t *testing.T) {
	f := newFixture(t)
	for _, args := range [][]string{
		{"cash", "recieve"},
		{"wallet", "shwo"},
		{"connect", "lsit"},
		{"circle", "jion"},
	} {
		res := f.run(args...)
		if res.ExitCode != 2 {
			t.Errorf("cashctl %v: exit %d, want 2 (usage)\nstdout: %s\nstderr: %s", args, res.ExitCode, res.Stdout, res.Stderr)
		}
		if res.Stdout != "" {
			t.Errorf("cashctl %v: stdout must stay empty on a usage error, got %q", args, res.Stdout)
		}
	}
}
