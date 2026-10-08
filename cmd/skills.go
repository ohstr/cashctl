package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/output"
	"github.com/ohstr/cashctl/skills"
)

func newSkillsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "List, print, or install the agent skills built into this binary",
		Example: `  cashctl skills list
  cashctl skills show cashctl-cash
  cashctl skills install --dir ~/.claude/skills`,
		RunE: groupRunE,
	}
	cmd.AddCommand(newSkillsListCmd(), newSkillsShowCmd(), newSkillsInstallCmd())
	return cmd
}

func newSkillsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the built-in agent skills with their descriptions",
		Args:  output.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonMode, _ := cmd.Flags().GetBool("json")
			all, err := skills.All()
			if err != nil {
				return output.RuntimeError(cmd, err)
			}
			if jsonMode {
				output.PrintJSON(all)
				return nil
			}
			for _, s := range all {
				output.Printf("%s\n  %s\n", s.Name, s.Description)
			}
			return nil
		},
	}
}

func newSkillsShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "show <name>",
		Short:   "Print one built-in skill's SKILL.md to stdout",
		Example: `  cashctl skills show cashctl-wallet`,
		Args:    output.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonMode, _ := cmd.Flags().GetBool("json")
			s, ok, err := skills.Get(args[0])
			if err != nil {
				return output.RuntimeError(cmd, err)
			}
			if !ok {
				return output.NotFoundError(cmd, args[0], fmt.Errorf(
					"no skill named %q — see `cashctl skills list`", args[0]))
			}
			if jsonMode {
				output.PrintJSON(map[string]any{
					"name":        s.Name,
					"description": s.Description,
					"content":     s.Content,
				})
				return nil
			}
			output.Printf("%s", s.Content)
			return nil
		},
	}
}

// installResult is one skill's outcome under `skills install`.
type installResult struct {
	Name   string `json:"name"`
	Path   string `json:"path"`   // the skill's directory
	Status string `json:"status"` // installed, updated, or unchanged
}

func newSkillsInstallCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Copy the built-in skills into an agent's skills directory",
		Long: `Writes every built-in skill's files (SKILL.md and anything it references)
under <dir>/<name>/, replacing older copies so they match this binary.
--dir defaults to ~/.claude/skills.`,
		Example: `  cashctl skills install
  cashctl skills install --dir .agents/skills`,
		Args: output.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonMode, _ := cmd.Flags().GetBool("json")
			dir, _ := cmd.Flags().GetString("dir")
			if dir == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return output.RuntimeError(cmd, fmt.Errorf("no home directory to default --dir to: %w", err))
				}
				dir = filepath.Join(home, ".claude", "skills")
			}
			all, err := skills.All()
			if err != nil {
				return output.RuntimeError(cmd, err)
			}
			results := make([]installResult, 0, len(all))
			for _, s := range all {
				r, err := installSkill(dir, s)
				if err != nil {
					return output.RuntimeError(cmd, err)
				}
				results = append(results, r)
			}
			if jsonMode {
				output.PrintJSON(map[string]any{"dir": dir, "skills": results})
				return nil
			}
			for _, r := range results {
				output.Printf("%-9s %s\n", r.Status, r.Path)
			}
			return nil
		},
	}
	cmd.Flags().String("dir", "", "skills directory to install into (default ~/.claude/skills)")
	return cmd
}

// installSkill writes every file of s under dir/<name>/, leaving files that
// already match alone. Status is per skill: installed when no SKILL.md was
// there yet, updated when any file changed, unchanged otherwise.
func installSkill(dir string, s skills.Skill) (installResult, error) {
	root := filepath.Join(dir, s.Name)
	r := installResult{Name: s.Name, Path: root, Status: "unchanged"}
	if _, err := os.Stat(filepath.Join(root, "SKILL.md")); errors.Is(err, os.ErrNotExist) {
		r.Status = "installed"
	} else if err != nil {
		return r, err
	}
	for _, rel := range s.Files {
		want, err := s.ReadFile(rel)
		if err != nil {
			return r, err
		}
		path := filepath.Join(root, filepath.FromSlash(rel))
		existing, err := os.ReadFile(path)
		if err == nil && bytes.Equal(existing, want) {
			continue
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return r, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return r, err
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			return r, err
		}
		if r.Status == "unchanged" {
			r.Status = "updated"
		}
	}
	return r, nil
}
