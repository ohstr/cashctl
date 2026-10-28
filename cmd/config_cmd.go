package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/output"
	"github.com/ohstr/cashctl/internal/store"
)

// settingKeys are the settings `cashctl config` manages, each with its check.
var settingKeys = map[string]func(string) error{
	powMaxSetting: func(v string) error { _, err := parsePowMax(powMaxSetting, v); return err },
}

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Read and change cashctl's settings (pow_max_difficulty)",
		RunE:  groupRunE,
	}
	known := func(cmd *cobra.Command, key string) error {
		if _, ok := settingKeys[key]; !ok {
			return output.InvalidInputError(cmd, key, fmt.Errorf("no setting %q (known: %s)", key, powMaxSetting))
		}
		return nil
	}
	print := func(cmd *cobra.Command, key string) error {
		v, ok, err := store.Setting(key)
		if err != nil {
			return output.RuntimeError(cmd, err)
		}
		jsonMode, _ := cmd.Flags().GetBool("json")
		if jsonMode {
			var val any
			if ok {
				val = v
			}
			output.PrintJSON(map[string]any{"key": key, "value": val})
			return nil
		}
		if !ok {
			output.Printf("%s is not set\n", key)
			return nil
		}
		output.Printf("%s = %s\n", key, v)
		return nil
	}
	cmd.AddCommand(&cobra.Command{
		Use: "get <key>", Short: "Show a setting", Args: output.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := known(cmd, args[0]); err != nil {
				return err
			}
			return print(cmd, args[0])
		},
	}, &cobra.Command{
		Use: "set <key> <value>", Short: "Change a setting", Args: output.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := known(cmd, args[0]); err != nil {
				return err
			}
			if args[1] == "" {
				return output.InvalidInputError(cmd, "", errors.New("a value is required; use `config unset` to remove one"))
			}
			if err := settingKeys[args[0]](args[1]); err != nil {
				return output.InvalidInputError(cmd, args[1], err)
			}
			if err := store.SetSetting(args[0], args[1]); err != nil {
				return output.RuntimeError(cmd, err)
			}
			return print(cmd, args[0])
		},
	}, &cobra.Command{
		Use: "unset <key>", Short: "Remove a setting (back to its default)", Args: output.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := known(cmd, args[0]); err != nil {
				return err
			}
			if err := store.SetSetting(args[0], ""); err != nil {
				return output.RuntimeError(cmd, err)
			}
			return print(cmd, args[0])
		},
	})
	return cmd
}
