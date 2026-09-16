package credential

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ohstr/nmilat/nip05"
)

// nip05HTTPClient bounds a NIP-05 lookup the same way every other network
// call in cashctl is bounded (e.g. the 15s timeouts on cash_receive.go's
// Hub checks) — a misbehaving or unreachable domain fails clearly instead
// of hanging indefinitely.
var nip05HTTPClient = &http.Client{Timeout: 15 * time.Second}

// looksLikeNIP05 reports whether s has the shape of a NIP-05 identifier
// (name@domain) — reusing nip05's own exported name/domain validators
// (nmilat/nip05/identity.go) so cashctl's notion of "looks like NIP-05"
// never drifts from the SDK's own.
func looksLikeNIP05(s string) bool {
	name, domain, ok := strings.Cut(s, "@")
	if !ok || name == "" {
		return false
	}
	return nip05.ValidateName(name) == nil && nip05.IsValidDomain(domain)
}

// nip05Endpoint builds the well-known URL a NIP-05 lookup fetches — a
// package-level var (rather than an inline literal) purely so tests can
// point it at a local httptest.Server instead of a real domain.
var nip05Endpoint = func(domain, name string) string {
	return fmt.Sprintf("https://%s/.well-known/nostr.json?name=%s", domain, url.QueryEscape(name))
}

// resolveNIP05 fetches domain's /.well-known/nostr.json and looks up name
// in it. nmilat/nip05 only implements the *server* side of NIP-05 (serving
// that document) — there's no client-side resolver to import, so this is
// cashctl's own, reusing nip05.IdentityResponse for the response shape to
// stay in sync with the SDK's own understanding of the wire format.
func resolveNIP05(identifier string) (hexPubkey string, err error) {
	name, domain, _ := strings.Cut(identifier, "@")

	resp, err := nip05HTTPClient.Get(nip05Endpoint(domain, name))
	if err != nil {
		return "", fmt.Errorf("could not reach %s: %w", domain, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s returned %s for nostr.json", domain, resp.Status)
	}

	var doc nip05.IdentityResponse
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", fmt.Errorf("%s's nostr.json is not valid: %w", domain, err)
	}
	pub, ok := doc.Names[name]
	if !ok {
		return "", fmt.Errorf("%s's nostr.json has no entry for %q", domain, name)
	}
	// Validated: this value came from a domain cashctl doesn't control,
	// and gets embedded verbatim in the "resolves to:" trust line.
	if !isHexPubkey(pub) {
		return "", fmt.Errorf("%s's nostr.json returned an invalid pubkey for %q", domain, name)
	}
	return pub, nil
}
