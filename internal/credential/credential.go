// Package credential parses cashctl's flag-string syntax for identity
// credentials and cash-transfer targets (cashctl-plan.md's Cash command
// tree) into the nipcash/nipcw types the SDK itself expects. This syntax
// is only needed for the override case — acting as/for someone else — the
// default path resolves a held token's credential from the ledger
// directly, never through this parser (see internal/ledger).
package credential

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nip19"
	"github.com/ohstr/nmilat/nipIC"
	"github.com/ohstr/nmilat/nipcash"
	"github.com/ohstr/nmilat/nipcw"
)

// ParseCash parses a NIP-CASH credential string — one of:
//
//	pubkey:<privkey>
//	connection-key:<privkey>,<platform>,<external-id>,<attestation-file>
//	cash:<secret>
func ParseCash(s string) (nipcash.Credential, error) {
	prefix, rest, ok := strings.Cut(s, ":")
	if !ok {
		return nil, fmt.Errorf("credential must be pubkey:<privkey>, connection-key:<privkey>,<platform>,<external-id>,<attestation-file>, or cash:<secret>")
	}
	switch prefix {
	case "pubkey":
		if rest == "" {
			return nil, fmt.Errorf("pubkey: credential is missing a private key")
		}
		return nipcash.BySigning(rest), nil
	case "cash":
		if rest == "" {
			return nil, fmt.Errorf("cash: credential is missing a secret")
		}
		return nipcash.BySecret(rest), nil
	case "connection-key":
		privKey, platform, externalID, attestationFile, err := splitConnectionKey(rest)
		if err != nil {
			return nil, err
		}
		attestation, err := loadAttestation(attestationFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load attestation file %q: %w", attestationFile, err)
		}
		return nipcash.BySigningConnectionKey(privKey, nipIC.WebIdentity(platform), externalID, attestation), nil
	default:
		return nil, fmt.Errorf("unknown credential kind %q (want pubkey, connection-key, or cash)", prefix)
	}
}

// ParseCircle parses a NIP-CW credential string. NIP-CW has only one mode:
//
//	pubkey:<privkey>
func ParseCircle(s string) (nipcw.Credential, error) {
	prefix, rest, ok := strings.Cut(s, ":")
	if !ok || prefix != "pubkey" || rest == "" {
		return nipcw.Credential{}, fmt.Errorf("circle credential must be pubkey:<privkey>")
	}
	return nipcw.BySigning(rest), nil
}

// ResolvedTarget carries a parsed cash_transfer/consolidate target
// alongside a human-readable summary of what, if anything, ParseTarget
// resolved on the way to it.
type ResolvedTarget struct {
	Target nipcash.Target
	// Kind classifies Target's shape — cash_transfer/cash_consolidate's
	// own user-facing commands use this to pick their "as cash"/"as
	// identity cash"/"as web identity cash" wording without reaching into
	// nipcash's own unexported concrete types (namedIdentity is the same
	// Go type for both a pubkey and a connection-key target; only the
	// package that built it, here, knows which).
	Kind TargetKind
	// Input is exactly what the user typed.
	Input string
	// Resolved is "" when Input already *is* the canonical form (hex,
	// npub, the verbose prefixed forms — a deterministic local decode with
	// nothing new to reveal); populated whenever something was looked up
	// or decoded into a value the human should see before trusting it
	// (e.g. a NIP-05 lookup, or the resolved connection target below).
	Resolved string
}

// TargetKind is ResolvedTarget's own classification of what it carries —
// see its doc comment for why this exists instead of a type switch on
// Target itself.
type TargetKind int

const (
	// TargetKindPubkey is a native Nostr identity: a hex pubkey, npub1...,
	// or a NIP-05 identifier (name@domain) resolved down to its pubkey.
	// The zero value: callers that build a ResolvedTarget without setting
	// Kind (every test fixture predating this field) land here rather than
	// on TargetKindCash, since cash-mode detection elsewhere is done by
	// type-asserting Target itself (*nipcash.CashTarget), never by Kind
	// — so an unset Kind can only ever under-specify a named target, never
	// misrender a real cash-mode one.
	TargetKindPubkey TargetKind = iota
	// TargetKindConnection is a platform-vouched Web Identity with no
	// Nostr keypair of its own yet: connection:<platform>:<external-id>:
	// <ia-pubkey>, or a resolved nconnection1....
	TargetKindConnection
	// TargetKindCash is a not-yet-realized cash-mode target (the "cash"
	// keyword — no destination was given at all), redeemable by whoever
	// ends up holding the resulting cash.
	TargetKindCash
)

// NeedsIAError is returned by ParseTarget for a syntactically valid
// nconnection1... string. nconnection deliberately never carries an
// Identity Authority — that's a local, sender-side trust decision per
// NIP-CASH, not something the shareable connection string should encode
// (see docs/ux-review.md's Part 1) — so resolving one all the way to a
// Target needs one more piece of information this package, being a pure
// parser with no I/O of its own, can't obtain by itself. A caller with a
// terminal should ask the human for an IA identity and finish resolution
// via ResolveConnectionTarget; a non-interactive caller should surface
// this as a usage error naming its --ia escape hatch.
type NeedsIAError struct {
	Input    string
	Key      nipIC.ConnectionKey
	Platform nipIC.WebIdentity
}

func (e *NeedsIAError) Error() string {
	platform := string(e.Platform)
	if platform == "" {
		platform = "this connection"
	}
	return fmt.Sprintf("this nconnection doesn't specify who to trust as Identity Authority for %s — pass --ia <identity> (hex pubkey or NIP-05) or use the verbose connection:<platform>:<external-id>:<ia-pubkey> form instead", platform)
}

// resolveIdentityString sniffs s as a hex pubkey, npub1..., or NIP-05
// identifier — the one shared sniffer behind both ParseTarget's own
// --to/--as auto-detection and IA-identity resolution (the nconnection
// wizard prompt / --ia flag), so both stay byte-identical in what they
// accept and no divergent second sniffer needs to exist.
func resolveIdentityString(s string) (hexPubkey string, viaNIP05 bool, err error) {
	if isHexPubkey(s) {
		return strings.ToLower(s), false, nil
	}
	if strings.HasPrefix(s, "npub1") {
		hexPub, err := nip19.DecodePublicKey(s)
		if err != nil {
			return "", false, fmt.Errorf("not a valid npub: %w", err)
		}
		return hexPub, false, nil
	}
	if looksLikeNIP05(s) {
		hexPub, err := resolveNIP05(s)
		if err != nil {
			return "", false, fmt.Errorf("resolving %s via NIP-05: %w", s, err)
		}
		return hexPub, true, nil
	}
	return "", false, fmt.Errorf("must be a hex pubkey, npub1..., or a NIP-05 identifier (name@domain)")
}

// ParseTarget parses a cash_transfer --to target string. The common case
// needs no prefix at all — a bare 64-hex pubkey, an npub1... (nip19), a
// NIP-05 identifier (name@domain, resolved live — the one form below that
// isn't a local decode), or an nconnection1... are all sniffed by shape
// before falling back to the explicit, scripted/advanced forms:
//
//	pubkey:<hex>
//	connection:<platform>:<external-id>:<ia-pubkey>
//	cash
//
// The prefixed forms keep working exactly as before — this is additive
// sniffing in front of the existing parser, not a replacement of it.
func ParseTarget(s string) (ResolvedTarget, error) {
	if s == "cash" {
		target := nipcash.NewCashTarget()
		// The wire request only ever carries a one-way commitment of this
		// secret (NIP-CASH §Cash-Mode Slices: "the caller supplies the
		// commitment themselves") — the secret itself exists nowhere else
		// once this call returns. Losing it here is equivalent to losing
		// the funds, same as any other cash note, so it MUST be
		// surfaced via Resolved rather than silently discarded — the
		// caller shows it before/alongside committing, exactly like any
		// other value this field carries. Deliberately doesn't say
		// whether it's shown again later: `consolidate --to cash`
		// (self-securing) stores it in the caller's own ledger entry and
		// never re-displays it raw; `transfer`'s own cash-mode case (a gift
		// to someone else) *does* re-display it, combined with the
		// resulting token, once the call completes — a caller-specific
		// claim this shared parser has no way to make accurately for both.
		secret := target.Secret()
		return ResolvedTarget{
			Target:   target,
			Kind:     TargetKindCash,
			Input:    s,
			Resolved: fmt.Sprintf("Generated a cash secret: %s", secret),
		}, nil
	}
	if isHexPubkey(s) || strings.HasPrefix(s, "npub1") || looksLikeNIP05(s) {
		hexPub, viaNIP05, err := resolveIdentityString(s)
		if err != nil {
			return ResolvedTarget{}, err
		}
		rt := ResolvedTarget{Target: nipcash.Pubkey(hexPub), Kind: TargetKindPubkey, Input: s}
		if viaNIP05 {
			rt.Resolved = fmt.Sprintf("pubkey %s", hexPub)
		}
		return rt, nil
	}
	if strings.HasPrefix(s, nipIC.NConnectionPrefix+"1") {
		key, _, platform, err := nipIC.DecodeNConnection(s)
		if err != nil {
			return ResolvedTarget{}, fmt.Errorf("not a valid nconnection: %w", err)
		}
		return ResolvedTarget{}, &NeedsIAError{Input: s, Key: key, Platform: platform}
	}
	prefix, rest, ok := strings.Cut(s, ":")
	if !ok {
		return ResolvedTarget{}, fmt.Errorf("target must be a hex pubkey, npub1..., a NIP-05 identifier (name@domain), an nconnection1..., pubkey:<hex>, connection:<platform>:<external-id>:<ia-pubkey>, or cash")
	}
	switch prefix {
	case "pubkey":
		if rest == "" {
			return ResolvedTarget{}, fmt.Errorf("pubkey: target is missing a hex pubkey")
		}
		return ResolvedTarget{Target: nipcash.Pubkey(rest), Kind: TargetKindPubkey, Input: s}, nil
	case "connection":
		parts := strings.Split(rest, ":")
		if len(parts) != 3 {
			return ResolvedTarget{}, fmt.Errorf("connection: target needs platform:external-id:ia-pubkey, got %d field(s)", len(parts))
		}
		return ResolvedTarget{Target: nipcash.ConnectionKey(nipIC.WebIdentity(parts[0]), parts[1], parts[2]), Kind: TargetKindConnection, Input: s}, nil
	default:
		return ResolvedTarget{}, fmt.Errorf("unknown target kind %q (want pubkey, connection, or cash)", prefix)
	}
}

// ResolveConnectionTarget finishes resolving a target that ParseTarget's
// NeedsIAError deferred: key/platform came from a decoded nconnection1...
// string (originalInput); iaIdentity is whatever the caller obtained
// afterward (a human via a wizard prompt, or --ia) — hex or NIP-05, the
// same shapes ParseTarget itself accepts for a plain --to target.
func ResolveConnectionTarget(originalInput string, key nipIC.ConnectionKey, platform nipIC.WebIdentity, iaIdentity string) (ResolvedTarget, error) {
	iaHex, viaNIP05, err := resolveIdentityString(iaIdentity)
	if err != nil {
		return ResolvedTarget{}, fmt.Errorf("resolving IA identity %q: %w", iaIdentity, err)
	}
	iaDisplay := iaHex
	if viaNIP05 {
		iaDisplay = fmt.Sprintf("%s (%s)", iaIdentity, iaHex)
	}
	platformLabel := string(platform)
	if platformLabel == "" {
		platformLabel = "unspecified-platform"
	}
	return ResolvedTarget{
		Target:   nipcash.ResolvedConnectionKey(key, platform, iaHex),
		Kind:     TargetKindConnection,
		Input:    originalInput,
		Resolved: fmt.Sprintf("%s connection; Identity Authority: %s", platformLabel, iaDisplay),
	}, nil
}

// LooksLikeTarget reports whether s has the SHAPE of a cash_transfer
// target — every branch ParseTarget itself dispatches on, checked
// LOCALLY and without resolving anything live (unlike looksLikeNIP05's
// own caller inside ParseTarget, which — once this shape check passes —
// goes on to actually look the identifier up; this function only asks
// "could this plausibly be one," the same question ParseAmount answers
// for an amount). Exists for a caller that needs to guess which of two
// positional args is the target BEFORE committing to parsing either one
// as anything in particular — see cash_transfer.go's
// disambiguateTransferArgs, whose own bug this closes: when neither
// argument parses as an amount (a malformed amount typo, most often),
// blindly assuming positional order used to blame whichever argument
// happened to land in the "amount" slot, even when it was obviously the
// target (an npub) and the OTHER argument was the actually-malformed
// amount.
func LooksLikeTarget(s string) bool {
	if s == "cash" {
		return true
	}
	if isHexPubkey(s) || strings.HasPrefix(s, "npub1") || looksLikeNIP05(s) {
		return true
	}
	if strings.HasPrefix(s, nipIC.NConnectionPrefix+"1") {
		return true
	}
	prefix, _, ok := strings.Cut(s, ":")
	return ok && (prefix == "pubkey" || prefix == "connection")
}

// isHexPubkey reports whether s is a bare 64-character hex string — the
// shape a raw Nostr pubkey always has. Deliberately strict (exact length,
// valid hex) so it can never misfire against some other unprefixed value
// that merely contains hex-looking characters.
func isHexPubkey(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func splitConnectionKey(rest string) (privKey, platform, externalID, attestationFile string, err error) {
	parts := strings.Split(rest, ",")
	if len(parts) != 4 {
		return "", "", "", "", fmt.Errorf("connection-key: needs privkey,platform,external-id,attestation-file, got %d field(s)", len(parts))
	}
	for i, p := range parts {
		if p == "" {
			names := []string{"privkey", "platform", "external-id", "attestation-file"}
			return "", "", "", "", fmt.Errorf("connection-key: %s is empty", names[i])
		}
	}
	return parts[0], parts[1], parts[2], parts[3], nil
}

func loadAttestation(file string) (*nipIC.Attestation, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var ev nip01.Event
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil, fmt.Errorf("not a valid Nostr event JSON: %w", err)
	}
	return nipIC.ParseAttestation(&ev)
}
