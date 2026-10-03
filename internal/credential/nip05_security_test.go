package credential

// Audit D, CLI surface, finding 2 (D-CLI-2) — NIP-05 resolution.
//
// A NIP-05 identifier's domain comes from a user-supplied target, and cashctl is
// explicitly agent-facing, so that target can come from untrusted input. The
// lookup fetches https://<domain>/.well-known/nostr.json, and two controls were
// missing:
//
//   - Redirects were followed under Go's DEFAULT policy: up to 10 hops, to any
//     host, scheme or port, including a downgrade to http://. One hostile
//     domain could therefore steer the request at http://169.254.169.254/
//     (cloud instance metadata) or at a port on the caller's own loopback.
//   - The body was read unbounded, straight into json.NewDecoder off the
//     socket, so a hostile domain could stream for the whole 15s timeout.
//
// These tests are deliberately written WITHOUT referencing maxNIP05Body or any
// other new identifier, so they compile against the unfixed code and fail on
// behaviour rather than on a build error — a compile failure is a weaker red,
// because it proves nothing about what the code did.
//
// Scope note: a public name whose A record resolves to a private address is NOT
// covered, by decision — a post-DNS address guard strict enough to help breaks
// self-hosted and in-cluster NIP-05. That residual is recorded in the audit doc,
// not silently left.
//
// Mutants these must fail against: removing CheckRedirect; removing the
// io.LimitReader cap; treating an over-cap body as malformed JSON instead of as
// too large; and restoring resp.Status in place of resp.StatusCode.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fixturePubkey is any syntactically valid pubkey — isHexPubkey only checks for
// 64 hex characters, so this needs no key material.
const fixturePubkey = "abababababababababababababababababababababababababababababababab"

// nostrJSONFor is a well-formed nostr.json naming one entry, plus however much
// padding is asked for. The padding sits in a field the decoder ignores, so the
// document stays valid at any size — which is what separates "too large" from
// "malformed" in the tests below.
func nostrJSONFor(t *testing.T, name string, padBytes int) []byte {
	t.Helper()
	doc := map[string]any{"names": map[string]string{name: fixturePubkey}}
	if padBytes > 0 {
		doc["pad"] = strings.Repeat("a", padBytes)
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshalling the fixture document: %v", err)
	}
	return b
}

func TestResolveNIP05_RefusesARedirect(t *testing.T) {
	// The redirect target records whether it was ever reached. Asserting the
	// error alone would not distinguish "refused" from "followed it and then
	// failed for some other reason" — and being reached at all is the whole
	// defect, since in production that target is an address the attacker chose.
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		_, _ = w.Write(nostrJSONFor(t, "alice", 0))
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/.well-known/nostr.json?name=alice", http.StatusFound)
	}))
	defer redirector.Close()

	restore := stubNIP05Endpoint(redirector.URL)
	defer restore()

	got, err := resolveNIP05("alice@example.com")
	if err == nil {
		t.Errorf("resolveNIP05 followed the redirect and resolved to %q; want it refused", got)
	}
	if n := targetHits.Load(); n != 0 {
		t.Errorf("the redirect target was requested %d time(s); a refused redirect must never be dialed", n)
	}
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "redirect") {
		t.Errorf("error = %q, want it to say a redirect was refused so the failure is diagnosable", err)
	}
}

// TestResolveNIP05_RefusesARedirectAimedAtInstanceMetadata is the same control
// stated as the threat it exists for. 169.254.169.254 is the cloud
// instance-metadata address; under Go's default policy this request would have
// been made.
func TestResolveNIP05_RefusesARedirectAimedAtInstanceMetadata(t *testing.T) {
	for _, dest := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:1/",
		"http://[::1]:1/",
	} {
		t.Run(dest, func(t *testing.T) {
			redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, dest, http.StatusFound)
			}))
			defer redirector.Close()
			restore := stubNIP05Endpoint(redirector.URL)
			defer restore()

			_, err := resolveNIP05("alice@example.com")
			if err == nil {
				t.Fatalf("a redirect to %s was followed without error; want it refused", dest)
			}
			// Asserting only "there was an error" would pass vacuously: these
			// destinations are unreachable from a test host, so following the
			// redirect ALSO errors. The error has to show the redirect was
			// declined before it was dialed, which is the actual control.
			if !strings.Contains(strings.ToLower(err.Error()), "redirect") {
				t.Errorf("error = %q, want it to show the redirect was refused rather than merely attempted and failed", err)
			}
		})
	}
}

// TestResolveNIP05_OversizedBodyIsReportedAsTooLarge pins the distinction that
// makes the cap debuggable. A bare io.LimitReader truncates and the failure then
// surfaces as "unexpected end of JSON input", which is indistinguishable from a
// domain serving genuinely malformed JSON and sends whoever is debugging it
// somewhere else entirely.
//
// One mebibyte of padding: comfortably over any cap worth setting, and still a
// VALID document, so before the fix this resolved successfully.
func TestResolveNIP05_OversizedBodyIsReportedAsTooLarge(t *testing.T) {
	body := nostrJSONFor(t, "alice", 1<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	restore := stubNIP05Endpoint(srv.URL)
	defer restore()

	got, err := resolveNIP05("alice@example.com")
	if err == nil {
		t.Fatalf("a %d-byte nostr.json resolved to %q; want it refused as too large", len(body), got)
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "larger") && !strings.Contains(msg, "too large") {
		t.Errorf("error = %q, want it to say the document was too large", err)
	}
	for _, wrong := range []string{"unexpected end", "not valid"} {
		if strings.Contains(msg, wrong) {
			t.Errorf("error = %q, want it reported as too large rather than as malformed (%q)", err, wrong)
		}
	}
}

// TestResolveNIP05_AnOrdinarilyLargeBodyStillResolves is the other half: a cap
// set too tight would break real domains that serve their whole roster. 64 KiB
// is far more than the ?name=-scoped reply a conforming server returns, and must
// keep working.
func TestResolveNIP05_AnOrdinarilyLargeBodyStillResolves(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(nostrJSONFor(t, "alice", 64<<10))
	}))
	defer srv.Close()
	restore := stubNIP05Endpoint(srv.URL)
	defer restore()

	got, err := resolveNIP05("alice@example.com")
	if err != nil {
		t.Fatalf("a 64 KiB nostr.json was refused: %v", err)
	}
	if got != fixturePubkey {
		t.Errorf("resolved to %q, want %q", got, fixturePubkey)
	}
}

// TestResolveNIP05_StatusErrorOmitsTheRemoteReasonPhrase: resp.Status carries
// the reason phrase from the wire, so a hostile server could put terminal escape
// sequences in it and have them printed. resp.StatusCode is an integer and
// carries everything a caller can act on.
//
// Asserted via the canonical phrase Go pairs with the code, since net/http's own
// server will not emit a custom one — if the error says "teapot", it was built
// from Status rather than StatusCode.
func TestResolveNIP05_StatusErrorOmitsTheRemoteReasonPhrase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()
	restore := stubNIP05Endpoint(srv.URL)
	defer restore()

	_, err := resolveNIP05("alice@example.com")
	if err == nil {
		t.Fatal("a non-200 nostr.json resolved without error")
	}
	if !strings.Contains(err.Error(), fmt.Sprint(http.StatusTeapot)) {
		t.Errorf("error = %q, want it to name the status code", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "teapot") {
		t.Errorf("error = %q, want the status CODE only — the reason phrase comes off the wire", err)
	}
}
