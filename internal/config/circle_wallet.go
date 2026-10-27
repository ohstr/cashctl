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
	w.HubVerified, w.Network, w.Label = nil, "", ""
	return json.MarshalIndent(w, "", "  ")
}

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
