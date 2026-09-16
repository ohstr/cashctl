package credential

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nip19"
	"github.com/ohstr/nmilat/nipIC"
	"github.com/ohstr/nmilat/utils"
)

func randomPrivKeyHex(t *testing.T) string {
	t.Helper()
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b[:])
}

func TestParseCash_Pubkey(t *testing.T) {
	priv := randomPrivKeyHex(t)
	cred, err := ParseCash("pubkey:" + priv)
	if err != nil {
		t.Fatalf("ParseCash() error = %v", err)
	}
	if cred == nil {
		t.Fatal("ParseCash() returned nil credential")
	}
}

func TestParseCash_Bearer(t *testing.T) {
	cred, err := ParseCash("bearer:some-secret")
	if err != nil {
		t.Fatalf("ParseCash() error = %v", err)
	}
	if cred == nil {
		t.Fatal("ParseCash() returned nil credential")
	}
}

func TestParseCash_Errors(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"no colon", "pubkey-no-colon"},
		{"empty pubkey", "pubkey:"},
		{"empty bearer", "bearer:"},
		{"unknown kind", "carrier-pigeon:abc"},
		{"connection-key wrong field count", "connection-key:abc,discord"},
		{"connection-key empty field", "connection-key:abc,,482910,file.json"},
		{"connection-key missing file", "connection-key:abc,discord,482910,/does/not/exist.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseCash(tt.in); err == nil {
				t.Errorf("ParseCash(%q) = nil error, want an error", tt.in)
			}
		})
	}
}

func TestParseCash_ConnectionKey_ValidAttestation(t *testing.T) {
	iaPriv := randomPrivKeyHex(t)
	userPriv := randomPrivKeyHex(t)
	userPub, err := utils.GetPublicKey(userPriv)
	if err != nil {
		t.Fatalf("GetPublicKey() error = %v", err)
	}
	connKey := nipIC.NewConnectionKey("discord", "482910")

	attestationEvent, err := nipIC.NewAttestation(nipIC.AttestationParams{
		PrivateKey:    iaPriv,
		ConnectionKey: connKey,
		UserPubkey:    userPub,
		Platform:      "discord",
		Evidence: nipIC.Evidence{
			Platform: "discord", UserID: "482910", Username: "someone", VerifiedAt: 1720000000,
		},
	})
	if err != nil {
		t.Fatalf("NewAttestation() error = %v", err)
	}

	data, err := json.Marshal(attestationEvent)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	file := filepath.Join(t.TempDir(), "attestation.json")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cred, err := ParseCash("connection-key:" + userPriv + ",discord,482910," + file)
	if err != nil {
		t.Fatalf("ParseCash() error = %v", err)
	}
	if cred == nil {
		t.Fatal("ParseCash() returned nil credential")
	}
}

func TestParseCash_ConnectionKey_MalformedAttestationFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(file, []byte("not json"), 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := ParseCash("connection-key:priv,discord,482910," + file); err == nil {
		t.Error("expected an error parsing a malformed attestation file")
	}
}

func TestParseCircle_Pubkey(t *testing.T) {
	priv := randomPrivKeyHex(t)
	if _, err := ParseCircle("pubkey:" + priv); err != nil {
		t.Fatalf("ParseCircle() error = %v", err)
	}
}

func TestParseCircle_RejectsNonPubkeyModes(t *testing.T) {
	tests := []string{"bearer:secret", "connection-key:a,b,c,d", "pubkey:", "no-colon-at-all"}
	for _, in := range tests {
		if _, err := ParseCircle(in); err == nil {
			t.Errorf("ParseCircle(%q) = nil error, want an error (NIP-CW has only pubkey mode)", in)
		}
	}
}

func TestParseTarget_Pubkey(t *testing.T) {
	priv := randomPrivKeyHex(t)
	pub, err := utils.GetPublicKey(priv)
	if err != nil {
		t.Fatalf("GetPublicKey() error = %v", err)
	}
	target, err := ParseTarget("pubkey:" + pub)
	if err != nil {
		t.Fatalf("ParseTarget() error = %v", err)
	}
	if target.Target == nil {
		t.Fatal("ParseTarget() returned nil target")
	}
	if target.Resolved != "" {
		t.Errorf("ParseTarget(pubkey:...) Resolved = %q, want empty (already canonical)", target.Resolved)
	}
}

func TestParseTarget_Connection(t *testing.T) {
	target, err := ParseTarget("connection:discord:482910:deadbeef")
	if err != nil {
		t.Fatalf("ParseTarget() error = %v", err)
	}
	if target.Target == nil {
		t.Fatal("ParseTarget() returned nil target")
	}
}

func TestParseTarget_BearerTarget_GeneratesFreshSecretEachTime(t *testing.T) {
	t1, err := ParseTarget("bearer-target")
	if err != nil {
		t.Fatalf("ParseTarget() error = %v", err)
	}
	t2, err := ParseTarget("bearer-target")
	if err != nil {
		t.Fatalf("ParseTarget() error = %v", err)
	}

	bt1, ok := t1.Target.(interface{ Secret() string })
	if !ok {
		t.Fatal("bearer-target result does not expose Secret()")
	}
	bt2 := t2.Target.(interface{ Secret() string })

	if bt1.Secret() == "" {
		t.Error("Secret() is empty")
	}
	if bt1.Secret() == bt2.Secret() {
		t.Error("two bearer-target calls produced the same secret — should be fresh each time")
	}
}

// TestParseTarget_BearerTarget_ResolvedSurfacesSecret guards a real bug:
// the wire request for a bearer-target transfer only ever carries a
// one-way commitment of this secret (NIP-CASH §Bearer Slices) — the
// secret itself exists nowhere else once ParseTarget returns. An earlier
// version generated it and simply discarded it, making the resulting
// funds permanently unspendable (caught by a live integration test
// against a real Hub, redeeming with the secret extracted from this
// exact field). Resolved is the only place it's recoverable from.
func TestParseTarget_BearerTarget_ResolvedSurfacesSecret(t *testing.T) {
	rt, err := ParseTarget("bearer-target")
	if err != nil {
		t.Fatalf("ParseTarget() error = %v", err)
	}
	bt, ok := rt.Target.(interface{ Secret() string })
	if !ok {
		t.Fatal("bearer-target result does not expose Secret()")
	}
	if rt.Resolved == "" {
		t.Fatal("Resolved is empty — the generated secret would be lost with no way to recover it")
	}
	if !strings.Contains(rt.Resolved, bt.Secret()) {
		t.Errorf("Resolved = %q, want it to contain the actual generated secret %q", rt.Resolved, bt.Secret())
	}
}

func TestParseTarget_UppercaseHex(t *testing.T) {
	priv := randomPrivKeyHex(t)
	pub, err := utils.GetPublicKey(priv)
	if err != nil {
		t.Fatalf("GetPublicKey() error = %v", err)
	}
	target, err := ParseTarget(strings.ToUpper(pub))
	if err != nil {
		t.Fatalf("ParseTarget(uppercase hex) error = %v", err)
	}
	if target.Target == nil {
		t.Fatal("ParseTarget(uppercase hex) returned nil target")
	}
}

func TestParseTarget_BareHexPubkey(t *testing.T) {
	priv := randomPrivKeyHex(t)
	pub, err := utils.GetPublicKey(priv)
	if err != nil {
		t.Fatalf("GetPublicKey() error = %v", err)
	}
	// No pubkey: prefix at all — this is the point of the auto-detection.
	target, err := ParseTarget(pub)
	if err != nil {
		t.Fatalf("ParseTarget(%q) error = %v", pub, err)
	}
	if target.Target == nil {
		t.Fatal("ParseTarget() returned nil target")
	}
	if target.Resolved != "" {
		t.Errorf("ParseTarget(hex) Resolved = %q, want empty (already canonical)", target.Resolved)
	}
}

func TestParseTarget_Npub(t *testing.T) {
	priv := randomPrivKeyHex(t)
	pub, err := utils.GetPublicKey(priv)
	if err != nil {
		t.Fatalf("GetPublicKey() error = %v", err)
	}
	npub, err := nip19.EncodePublicKey(pub)
	if err != nil {
		t.Fatalf("EncodePublicKey() error = %v", err)
	}
	target, err := ParseTarget(npub)
	if err != nil {
		t.Fatalf("ParseTarget(%q) error = %v", npub, err)
	}
	if target.Target == nil {
		t.Fatal("ParseTarget() returned nil target")
	}
	if target.Resolved != "" {
		t.Errorf("ParseTarget(npub) Resolved = %q, want empty (a local decode, nothing new to reveal)", target.Resolved)
	}
}

func TestParseTarget_Npub_Invalid(t *testing.T) {
	if _, err := ParseTarget("npub1thisisnotvalidbech32atall"); err == nil {
		t.Error("expected an error for a malformed npub")
	}
}

func TestParseTarget_NIP05_Resolves(t *testing.T) {
	priv := randomPrivKeyHex(t)
	pub, err := utils.GetPublicKey(priv)
	if err != nil {
		t.Fatalf("GetPublicKey() error = %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"names": map[string]string{"alice": pub}})
	}))
	defer srv.Close()
	restore := stubNIP05Endpoint(srv.URL)
	defer restore()

	target, err := ParseTarget("alice@example.com")
	if err != nil {
		t.Fatalf("ParseTarget() error = %v", err)
	}
	if target.Target == nil {
		t.Fatal("ParseTarget() returned nil target")
	}
	if target.Resolved == "" {
		t.Error("ParseTarget(NIP-05) Resolved is empty, want the resolved pubkey surfaced before it's trusted")
	}
	if !strings.Contains(target.Resolved, pub) {
		t.Errorf("ParseTarget(NIP-05) Resolved = %q, want it to contain the resolved pubkey %q", target.Resolved, pub)
	}
}

// TestParseTarget_NIP05_MalformedPubkeyRejected is a security regression
// test: a NIP-05 domain is untrusted network input, not a value cashctl
// controls — a malicious or misconfigured domain returning something that
// isn't a real 64-hex-char pubkey (garbage, a truncated value, or a string
// crafted with terminal control sequences) must be rejected outright,
// never passed through into the "resolves to:" line ParseTarget's own doc
// comment says exists specifically so a resolved target gets shown back
// before it's trusted — an unvalidated value there defeats that safeguard
// rather than serving it.
func TestParseTarget_NIP05_MalformedPubkeyRejected(t *testing.T) {
	tests := []struct {
		name string
		pub  string
	}{
		{"too short", "deadbeef"},
		{"too long", strings.Repeat("a", 65)},
		{"non-hex characters", strings.Repeat("z", 64)},
		{"empty", ""},
		{"embedded terminal escape sequence", "\x1b[2J\x1b[H" + strings.Repeat("a", 56)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"names": map[string]string{"alice": tt.pub}})
			}))
			defer srv.Close()
			restore := stubNIP05Endpoint(srv.URL)
			defer restore()

			if _, err := ParseTarget("alice@example.com"); err == nil {
				t.Errorf("ParseTarget: expected an error for a malformed resolved pubkey %q, got none", tt.pub)
			}
		})
	}
}

func TestParseTarget_NIP05_NoMatchingName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"names": map[string]string{}})
	}))
	defer srv.Close()
	restore := stubNIP05Endpoint(srv.URL)
	defer restore()

	if _, err := ParseTarget("bob@example.com"); err == nil {
		t.Error("expected an error when the domain's nostr.json has no entry for the requested name")
	}
}

func TestParseTarget_NIP05_UnreachableDomain(t *testing.T) {
	restore := stubNIP05Endpoint("http://127.0.0.1:1") // nothing listens here
	defer restore()

	if _, err := ParseTarget("alice@example.com"); err == nil {
		t.Error("expected an error when the NIP-05 domain can't be reached")
	}
}

func TestParseTarget_NConnection_NeedsIA(t *testing.T) {
	nconn, err := nipIC.EncodeNConnection(nipIC.NewConnectionKey("discord", "482910"), []string{"wss://relay.example.com"}, "discord")
	if err != nil {
		t.Fatalf("EncodeNConnection() error = %v", err)
	}

	_, err = ParseTarget(nconn)
	var needsIA *NeedsIAError
	if !errors.As(err, &needsIA) {
		t.Fatalf("ParseTarget(nconnection) error = %v, want a *NeedsIAError", err)
	}
	if needsIA.Platform != "discord" {
		t.Errorf("NeedsIAError.Platform = %q, want %q", needsIA.Platform, "discord")
	}
	if needsIA.Input != nconn {
		t.Errorf("NeedsIAError.Input = %q, want %q", needsIA.Input, nconn)
	}
	if !strings.Contains(needsIA.Error(), "--ia") {
		t.Errorf("NeedsIAError.Error() = %q, want it to mention --ia", needsIA.Error())
	}
}

// TestParseTarget_NConnection_NoPlatform covers nconnection's platform
// field being optional (nipIC/nconnection.go: "optional, at most once") —
// NeedsIAError.Error() falls back to a generic phrase instead of an empty
// or malformed-looking platform name in the message.
func TestParseTarget_NConnection_NoPlatform(t *testing.T) {
	nconn, err := nipIC.EncodeNConnection(nipIC.NewConnectionKey("discord", "482910"), []string{"wss://relay.example.com"}, "")
	if err != nil {
		t.Fatalf("EncodeNConnection() error = %v", err)
	}

	_, err = ParseTarget(nconn)
	var needsIA *NeedsIAError
	if !errors.As(err, &needsIA) {
		t.Fatalf("ParseTarget(nconnection, no platform) error = %v, want a *NeedsIAError", err)
	}
	if needsIA.Platform != "" {
		t.Errorf("NeedsIAError.Platform = %q, want empty", needsIA.Platform)
	}
	if !strings.Contains(needsIA.Error(), "this connection") {
		t.Errorf("NeedsIAError.Error() = %q, want it to fall back to \"this connection\"", needsIA.Error())
	}
}

func TestResolveConnectionTarget(t *testing.T) {
	key := nipIC.NewConnectionKey("discord", "482910")
	priv := randomPrivKeyHex(t)
	iaHex, err := utils.GetPublicKey(priv)
	if err != nil {
		t.Fatalf("GetPublicKey() error = %v", err)
	}

	t.Run("hex IA", func(t *testing.T) {
		rt, err := ResolveConnectionTarget("nconnection1...", key, "discord", iaHex)
		if err != nil {
			t.Fatalf("ResolveConnectionTarget() error = %v", err)
		}
		if rt.Target == nil {
			t.Fatal("ResolveConnectionTarget() returned nil target")
		}
		if !strings.Contains(rt.Resolved, "discord") || !strings.Contains(rt.Resolved, iaHex) {
			t.Errorf("Resolved = %q, want it to mention the platform and IA pubkey", rt.Resolved)
		}
	})

	t.Run("NIP-05 IA", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"names": map[string]string{"ia": iaHex}})
		}))
		defer srv.Close()
		restore := stubNIP05Endpoint(srv.URL)
		defer restore()

		rt, err := ResolveConnectionTarget("nconnection1...", key, "discord", "ia@example.com")
		if err != nil {
			t.Fatalf("ResolveConnectionTarget() error = %v", err)
		}
		if !strings.Contains(rt.Resolved, "ia@example.com") || !strings.Contains(rt.Resolved, iaHex) {
			t.Errorf("Resolved = %q, want it to show both the NIP-05 identifier and the resolved pubkey", rt.Resolved)
		}
	})

	t.Run("invalid IA identity", func(t *testing.T) {
		if _, err := ResolveConnectionTarget("nconnection1...", key, "discord", "not-an-identity"); err == nil {
			t.Error("expected an error for an IA identity that doesn't parse as hex, npub, or NIP-05")
		}
	})
}

func TestLooksLikeNIP05(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"alice@example.com", true},
		{"alice@sub.example.com", true},
		{"no-at-sign", false},
		{"@example.com", false},   // empty name
		{"alice@", false},         // empty/invalid domain
		{"alice@not_a_domain", false},
		{"pubkey:deadbeef", false}, // must not shadow the explicit prefix form
	}
	for _, tt := range tests {
		if got := looksLikeNIP05(tt.in); got != tt.want {
			t.Errorf("looksLikeNIP05(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// stubNIP05Endpoint redirects nip05Endpoint at srvURL for the duration of a
// test, ignoring the real domain — the only way to exercise resolveNIP05
// against a local httptest.Server instead of a live network call. Returns
// a restore func to undo it.
func stubNIP05Endpoint(srvURL string) (restore func()) {
	orig := nip05Endpoint
	nip05Endpoint = func(_, name string) string {
		return srvURL + "/.well-known/nostr.json?name=" + name
	}
	return func() { nip05Endpoint = orig }
}

func TestParseTarget_Errors(t *testing.T) {
	tests := []string{
		"no-colon",
		"pubkey:",
		"connection:only-one-field",
		"connection:discord:482910",
		"unknown-kind:x",
	}
	for _, in := range tests {
		if _, err := ParseTarget(in); err == nil {
			t.Errorf("ParseTarget(%q) = nil error, want an error", in)
		}
	}
}
