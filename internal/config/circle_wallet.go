package config

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

// CircleWalletType is the "type" of a Circle Wallet's stored fields and of its
// backup file.
const CircleWalletType = "circle_wallet"

// CircleWallet is a Circle Wallet as cashctl holds it: its wallet_pubkey,
// relays and the connection secret the member generated, plus which circle it
// belongs to. It is stored as these fields (a kind circle connection's value)
// and written as a backup file in the same shape; no URI is ever built or
// stored for it.
type CircleWallet struct {
	Type         string   `json:"type"`
	WalletPubkey string   `json:"wallet_pubkey"`
	Relays       []string `json:"relays"`
	Secret       string   `json:"secret"`
	HubPubkey    string   `json:"hub_pubkey"`
	HubIdentity  string   `json:"hub_identity"`
	Chain        string   `json:"chain"`
	// MemberPubkey is the pubkey of the join that created the wallet: the
	// only key whose member requests the Hub acts on.
	MemberPubkey string `json:"member_pubkey,omitempty"`
	// DetailsKnown says the wallet's own terms below were read from its
	// details; a wallet registered from an older backup has none.
	DetailsKnown bool `json:"details_known,omitempty"`
	// RotationDelaySecs is the wallet's rotation delay, nil on a wallet that
	// cannot be rotated (NIP-CW §Recovering a Lost Connection).
	RotationDelaySecs *uint64 `json:"rotation_delay_secs,omitempty"`
	// DustThresholdMillis is the most a member deletion leaves to the host.
	DustThresholdMillis uint64 `json:"dust_threshold_millis,omitempty"`
	// DeleteSentAt is when a delete_circle_wallet was written ahead for this
	// wallet; its reply may have been lost (not in a backup file).
	DeleteSentAt int64 `json:"delete_sent_at,omitempty"`
	// Circle details kept beside the backup fields (not in a backup file).
	HubVerified *bool  `json:"hub_verified,omitempty"`
	Network     string `json:"network,omitempty"`
	Label       string `json:"label,omitempty"`
}

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Encode is w as its stored value.
func (w CircleWallet) Encode() (string, error) {
	w.Type = CircleWalletType
	b, err := json.Marshal(w)
	return string(b), err
}

// Backup is w's backup file: the fields `connect add` reads back.
func (w CircleWallet) Backup() ([]byte, error) {
	w.Type = CircleWalletType
	w.HubVerified, w.Network, w.Label, w.DeleteSentAt = nil, "", "", 0
	return json.MarshalIndent(w, "", "  ")
}

// Rotatable reports whether a member request may name w: false only when
// its details are known and carry no rotation_delay_secs.
func (w CircleWallet) Rotatable() bool { return !w.DetailsKnown || w.RotationDelaySecs != nil }

// ParseCircleWallet reads a Circle Wallet's stored fields or backup file. ok is
// false when value is not one at all; err says why one is unusable.
func ParseCircleWallet(value string) (w CircleWallet, ok bool, err error) {
	v := strings.TrimSpace(value)
	if !strings.HasPrefix(v, "{") {
		return CircleWallet{}, false, nil
	}
	if err := json.Unmarshal([]byte(v), &w); err != nil || w.Type != CircleWalletType {
		return CircleWallet{}, false, nil
	}
	switch {
	case !hex64.MatchString(w.WalletPubkey):
		return w, true, errors.New("circle wallet: wallet_pubkey is not 64 lowercase hex")
	case !hex64.MatchString(w.Secret):
		return w, true, errors.New("circle wallet: secret is not 64 lowercase hex")
	case len(w.Relays) == 0:
		return w, true, errors.New("circle wallet: no relays")
	}
	return w, true, nil
}
