package cmd

import (
	"errors"
	"strings"

	"github.com/ohstr/nmilat/nipcash"
	"github.com/ohstr/nmilat/nipcw"
	"github.com/ohstr/nmilat/utils"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/identity"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// localPrivKey resolves the local identity's raw private key — re-
// unlocking an ncli vault identity live (prompting for its password,
// honoring NCLI_VAULT_PASSWORD) if that's the identity source; never
// prompts for a local (cashctl-generated) identity.
//
// "No identity configured yet" is classified here, once, as not_found — the
// same class every other "you haven't set that up yet" condition uses
// (AGENTS.md: "no wallet configured yet"), with `cashctl init` as its
// remedy. Every caller used to wrap it as a generic internal error instead
// (exit 1), so the same missing-identity condition came back as a different
// class depending on which command happened to hit it first.
func localPrivKey(cmd *cobra.Command) (string, error) {
	jsonMode, _ := cmd.Flags().GetBool("json")
	priv, err := identity.Resolve(func() (string, error) { return ResolveVaultPassword(jsonMode) })
	if errors.Is(err, identity.ErrNotConfigured) {
		return "", output.NotFoundError(cmd, "", err)
	}
	return priv, err
}

func localPubKeyHex(cmd *cobra.Command) (string, error) {
	priv, err := localPrivKey(cmd)
	if err != nil {
		return "", err
	}
	return utils.GetPublicKey(priv)
}

// signerPubKeyHex is the pubkey a Hub's reply about entry is matched against:
// that of the key resolveCredential signs with — a pubkey:<privkey> --as,
// else a merge's throwaway key, else the local identity. entry may be nil.
func signerPubKeyHex(cmd *cobra.Command, entry *ledger.Entry) (string, error) {
	if priv, ok := strings.CutPrefix(asCredentialValue(cmd), "pubkey:"); ok {
		return utils.GetPublicKey(priv)
	}
	if entry != nil && entry.ThrowawayKey != "" {
		return utils.GetPublicKey(entry.ThrowawayKey)
	}
	return localPubKeyHex(cmd)
}

// credentialPubKeyHex is the pubkey a credential string (consolidate's
// <token>:<amount>:<credential> source form) signs as: its own key for
// pubkey:<privkey>, the local identity's otherwise.
func credentialPubKeyHex(cmd *cobra.Command, credential string) (string, error) {
	if priv, ok := strings.CutPrefix(credential, "pubkey:"); ok {
		return utils.GetPublicKey(priv)
	}
	return localPubKeyHex(cmd)
}

// localCashCredential is the default --as for redeeming/transferring a
// NIP-CASH token you hold locally.
func localCashCredential(cmd *cobra.Command) (nipcash.Credential, error) {
	priv, err := localPrivKey(cmd)
	if err != nil {
		return nil, err
	}
	return nipcash.BySigning(priv), nil
}

// localCircleCredential is the default --as for `cashctl join`.
func localCircleCredential(cmd *cobra.Command) (nipcw.Credential, error) {
	priv, err := localPrivKey(cmd)
	if err != nil {
		return nipcw.Credential{}, err
	}
	return nipcw.BySigning(priv), nil
}
