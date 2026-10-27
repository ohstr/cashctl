package cmd

import (
	"errors"
	"fmt"

	"github.com/ohstr/ncli/client/vault"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/identity"
	"github.com/ohstr/cashctl/internal/output"
)

func newWalletInitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "init",
		Short:   "Set up your identity and, optionally, a wallet",
		Long:    `Creates (or reuses) your identity, and optionally registers a default wallet.`,
		Example: `  cashctl init`,
		Args:    output.NoArgs,
		RunE:    runWalletInit,
	}
	return cmd
}

func runWalletInit(cmd *cobra.Command, args []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")

	if exists, err := identity.Exists(); err != nil {
		return output.RuntimeError(cmd, err)
	} else if exists {
		return reportAlreadyConfigured(cmd, jsonMode)
	}

	npub, source, created, err := setUpIdentity(cmd, jsonMode)
	if errors.Is(err, identity.ErrAlreadyConfigured) {
		// Lost a race with another `init` that claimed the identity between
		// the Exists check above and this one's save: report what that run
		// set up — same as if this one had simply started a moment later —
		// rather than announcing a key that was discarded.
		return reportAlreadyConfigured(cmd, jsonMode)
	}
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	output.Linef(jsonMode, "Using identity %s (%s)", npub, source)

	walletName := offerDefaultWallet(cmd, jsonMode)
	if walletName == "" {
		// None registered now: report the default already in effect, if any.
		if s, err := config.Load(); err == nil {
			walletName = s.Default
		}
	}

	if jsonMode {
		identityState := "reused"
		if created {
			identityState = "created"
		}
		output.PrintJSON(map[string]any{
			"identity":        identityState,
			"npub":            npub,
			"identity_source": source,
			"default_wallet":  walletName,
		})
	}
	return nil
}

// reportAlreadyConfigured says which identity is already in place, so a
// caller learns it was kept (not created or replaced) and which one it is.
func reportAlreadyConfigured(cmd *cobra.Command, jsonMode bool) error {
	npub, source, err := currentIdentity()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	output.Linef(jsonMode, "Already configured: identity %s (%s), kept as is. Run `cashctl wallet show` for the rest.", npub, source)
	if jsonMode {
		output.PrintJSON(map[string]any{
			"already_configured": true,
			"identity":           "existing",
			"npub":               npub,
			"identity_source":    source,
		})
	}
	return nil
}

// setUpIdentity offers an existing ncli vault entry if one is available,
// otherwise generates cashctl's own local identity. Returns the resulting
// npub, a short human-readable source label, and whether it was newly
// generated (false: an ncli vault entry was reused).
func setUpIdentity(cmd *cobra.Command, jsonMode bool) (npub, source string, created bool, err error) {
	// Never the vault under --json: README documents that `init --json`
	// generates a fresh local identity. This used to adopt entries[0]
	// silently (`use := jsonMode || Confirm(...)`) — binding whatever
	// personal Nostr key happened to be first in the user's ncli vault to
	// a money tool with no prompt and no way to decline, for exactly the
	// caller (a script or agent) least able to notice. Skipped before the
	// vault is even read.
	if jsonMode {
		return generateLocalIdentity()
	}
	exists, err := vault.Exists()
	if err == nil && exists {
		entries, err := vault.LoadEntries()
		if err == nil && len(entries) > 0 {
			entry := entries[0]
			if len(entries) > 1 {
				output.Notef(false, "Found %d identities in your ncli vault:", len(entries))
				for i, e := range entries {
					output.Notef(false, "  %d. %s (%s)", i+1, e.Label, e.Npub)
				}
				choice, _ := PromptLine(fmt.Sprintf("Use which one for cashctl? [1-%d, Enter to skip] ", len(entries)))
				idx := parseChoice(choice, len(entries))
				if idx < 0 {
					return generateLocalIdentity()
				}
				entry = entries[idx]
			}
			use := Confirm(cmd, true, fmt.Sprintf("Use existing identity %q?", entry.Label))
			if use {
				if err := identity.SaveNcliVaultRef(entry.Npub, entry.Label); err != nil {
					return "", "", false, err
				}
				return entry.Npub, fmt.Sprintf("ncli vault, label %q", entry.Label), false, nil
			}
		}
	}
	return generateLocalIdentity()
}

func generateLocalIdentity() (npub, source string, created bool, err error) {
	npub, err = identity.GenerateAndSaveLocal()
	if err != nil {
		return "", "", false, err
	}
	return npub, "cashctl-local", true, nil
}

func parseChoice(s string, n int) int {
	var i int
	if _, err := fmt.Sscanf(s, "%d", &i); err != nil || i < 1 || i > n {
		return -1
	}
	return i - 1
}

// offerDefaultWallet asks once (skipped entirely under --json, since
// there's no way to interactively supply a connection string non-
// interactively other than a separate `connect add` call) whether the
// user already has a Lightning wallet connection to register as their
// default — answering it is exactly `connect add` with a generated name
// (see cashctl-plan.md's "Local wallet layer"). Returns the resulting
// wallet's name, or "" if skipped.
func offerDefaultWallet(cmd *cobra.Command, jsonMode bool) string {
	if jsonMode {
		return ""
	}
	value, err := PromptLine("Have an NWC wallet to set as default? Paste it, or Enter to skip: ")
	if err != nil || value == "" {
		return ""
	}
	// Anything that isn't a usable connection is skipped, not saved: a
	// stray "n"/"no" typed here (the natural answer to a yes/no-sounding
	// question) used to be registered as the default wallet verbatim.
	if err := validateConnectionValue(value); err != nil {
		var wrong *wrongConnectionKindError
		if errors.As(err, &wrong) {
			output.Notef(false, "%s — skipping.", wrong.msg)
			return ""
		}
		output.Notef(false, "That doesn't look like a wallet connection — skipping. Add one later with `cashctl connect add <name> <connection>`.")
		return ""
	}
	s, err := config.Load()
	if err != nil {
		output.Notef(false, "Couldn't save that connection: %s", err)
		return ""
	}
	name := s.SuggestName("lightning", "default")
	if err := s.Add(name, value); err != nil {
		output.Notef(false, "Couldn't save that connection: %s", err)
		return ""
	}
	_ = s.SetDefault(name) // first wallet ever — see cashctl-plan.md's default-pointer rule
	if err := s.Save(); err != nil {
		output.Notef(false, "Couldn't save that connection: %s", err)
		return ""
	}
	output.Linef(false, "Saved connection: %s.\nDefault wallet set to %s.", name, name)
	return name
}
