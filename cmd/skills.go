package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/output"
	"github.com/ohstr/cashctl/skills"
)

func newSkillsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "List, show, or install the agent skills built into this binary",
		Example: `  cashctl skills list
  cashctl skills show cashctl-cash
  cashctl skills install --dir ~/.agents/skills`,
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
Only <dir>/cashctl-*/ is written; nothing else in <dir> is touched.
--dir is required: point it wherever your agent reads skills from.`,
		Example: `  cashctl skills install --dir ~/.agents/skills
  cashctl skills install --dir .agents/skills`,
		Args: output.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonMode, _ := cmd.Flags().GetBool("json")
			dir, _ := cmd.Flags().GetString("dir")
			// Checked explicitly (not cobra's MarkFlagRequired) so a missing
			// --dir is a classified usage error, like join's --hub.
			if dir == "" {
				return output.InvocationError(cmd, fmt.Errorf("--dir is required — pass the skills directory your agent reads from"))
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
	cmd.Flags().String("dir", "", "skills directory to install into (required)")
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
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return r, err
		}
		if err := refuseSymlinkedDirs(dir, filepath.Dir(path)); err != nil {
			return r, err
		}
		if fi, err := os.Lstat(path); err == nil && fi.Mode().IsRegular() {
			if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, want) {
				continue
			}
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return r, err
		}
		if err := replaceFile(path, want); err != nil {
			return r, err
		}
		if r.Status == "unchanged" {
			r.Status = "updated"
		}
	}
	return r, nil
}

// refuseSymlinkedDirs fails when sub, or any directory between dir and sub,
// is a symlink: writing through one would put the skill's files wherever it
// points instead of under dir.
func refuseSymlinkedDirs(dir, sub string) error {
	rel, err := filepath.Rel(dir, sub)
	if err != nil {
		return err
	}
	cur := dir
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink; remove it to install there", cur)
		}
	}
	return nil
}

// replaceFile writes data to path through a temporary file renamed over it:
// a symlink already at path is replaced, never written through to its target.
func replaceFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
