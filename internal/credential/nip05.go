package credential

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ohstr/nmilat/nip05"
)

// maxNIP05Body bounds how much of a domain's nostr.json cashctl will read.
//
// The document only has to carry one entry: nip05Endpoint appends
// ?name=<name>, which a conforming server uses to answer with just that name.
// 256 KiB is about 2,900 entries at ~90 bytes each (a name, a 64-hex pubkey
// and JSON punctuation), so it is generous even for a domain that ignores the
// scope and serves its whole roster, while still bounded.
//
// Unbounded was the actual defect: json.NewDecoder read straight off the
// socket, so a hostile domain could stream for the whole 15s timeout.
const maxNIP05Body = 256 << 10

// errNIP05Redirect marks a refused redirect so resolveNIP05 can tell it apart
// from an ordinary transport failure and say something useful about it. Wrapped
// by CheckRedirect and reachable through the *url.Error the client returns,
// hence errors.Is rather than a string match.
var errNIP05Redirect = errors.New("nostr.json redirected")

// nip05HTTPClient bounds a NIP-05 lookup the same way every other network
// call in cashctl is bounded (e.g. the 15s timeouts on cash_receive.go's
// Hub checks) — a misbehaving or unreachable domain fails clearly instead
// of hanging indefinitely.
//
// It also refuses to follow redirects at all. Go's default policy follows up
// to 10 hops to ANY host, scheme or port, including a downgrade to http://, so
// a single hostile NIP-05 domain could steer this request at
// http://169.254.169.254/ (cloud instance metadata) or at a port on the
// caller's own loopback — a server-side request forgery reachable from
// anything that hands cashctl a name@domain target, which for an agent-facing
// CLI means untrusted input.
//
// A NIP-05 endpoint has no legitimate reason to redirect, so refusing is
// cheap. The accepted cost is real though: a domain serving
// /.well-known/nostr.json behind an apex->www redirect stops resolving, which
// is why the refusal names the scheme and host it declined rather than
// surfacing as a bare transport error.
//
// Note what this deliberately does NOT do. A public name whose A record points
// at a private address still resolves: there is no post-DNS address guard,
// because one strict enough to help would break self-hosted and in-cluster
// NIP-05. nip05.IsValidDomain's own regex rejects `localhost` and bare IP
// literals (it requires a dot and a 2+ letter alpha TLD) but that is shape
// validation that happens to exclude them, not a security control, and it
// would not survive someone relaxing the regex.
var nip05HTTPClient = &http.Client{
	Timeout: 15 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return fmt.Errorf("%w to %s://%s", errNIP05Redirect, req.URL.Scheme, req.URL.Host)
	},
}

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
		if errors.Is(err, errNIP05Redirect) {
			return "", fmt.Errorf("%s's nostr.json redirects, and cashctl does not follow redirects on an identity lookup: %w", domain, err)
		}
		return "", fmt.Errorf("could not reach %s: %w", domain, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// StatusCode, not Status: the reason phrase in Status comes verbatim off
	// the wire, so a hostile (or merely odd) server could put terminal escape
	// sequences in it and have them printed. The number carries everything a
	// caller can act on.
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s returned HTTP %d for nostr.json", domain, resp.StatusCode)
	}

	// Read one byte past the cap so "too large" can be reported as itself. A
	// bare io.LimitReader would hand json.Unmarshal a truncated document, and
	// that surfaces as "unexpected end of JSON input" — indistinguishable from
	// a domain serving genuinely malformed JSON, which sends whoever is
	// debugging it in the wrong direction.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxNIP05Body+1))
	if err != nil {
		return "", fmt.Errorf("could not read %s's nostr.json: %w", domain, err)
	}
	if len(body) > maxNIP05Body {
		return "", fmt.Errorf("%s's nostr.json is larger than the %d KiB cashctl will read", domain, maxNIP05Body>>10)
	}

	var doc nip05.IdentityResponse
	if err := json.Unmarshal(body, &doc); err != nil {
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
