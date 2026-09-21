// Command surface-dump prints cashctl's entire CLI surface as JSON — every
// command path (canonical and top-level shortcut), its local flags, and the
// global flags — walked from the real cobra tree, so it can never drift from
// the code. It is the required-coverage list the campaign's gate script
// (integration/agent-eval/campaign/gate.py) checks executed commands against.
// Not part of cashctl itself; run with `go run ./integration/agent-eval/surface-dump`.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/ohstr/cashctl/cmd"
)

type flagInfo struct {
	Name  string `json:"name"`
	Short string `json:"short,omitempty"`
	Type  string `json:"type"`
}

type cmdInfo struct {
	Path     string     `json:"path"`
	Runnable bool       `json:"runnable"`
	Short    string     `json:"short"`
	Flags    []flagInfo `json:"flags"`
	// Twin is the other spelling of the same command (a top-level
	// docker-style shortcut vs its canonical `group sub` form), "" if none.
	Twin string `json:"twin,omitempty"`
}

type surface struct {
	GlobalFlags []flagInfo `json:"global_flags"`
	Commands    []cmdInfo  `json:"commands"`
}

func main() {
	root := cmd.RootCmd
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()

	s := surface{GlobalFlags: flagsOf(root.PersistentFlags())}
	var walk func(c *cobra.Command, path []string)
	walk = func(c *cobra.Command, path []string) {
		for _, ch := range c.Commands() {
			if ch.Hidden {
				continue
			}
			p := append(append([]string{}, path...), ch.Name())
			s.Commands = append(s.Commands, cmdInfo{
				Path:     strings.Join(p, " "),
				Runnable: ch.Runnable(),
				Short:    ch.Short,
				Flags:    flagsOf(ch.LocalNonPersistentFlags()),
			})
			walk(ch, p)
		}
	}
	walk(root, nil)

	// Twin detection: same Short text and same leaf name, exactly two paths.
	byKey := map[string][]int{}
	for i, c := range s.Commands {
		if !c.Runnable || c.Short == "" {
			continue
		}
		leaf := c.Path[strings.LastIndex(c.Path, " ")+1:]
		byKey[leaf+"|"+c.Short] = append(byKey[leaf+"|"+c.Short], i)
	}
	for _, idx := range byKey {
		if len(idx) == 2 {
			a, b := &s.Commands[idx[0]], &s.Commands[idx[1]]
			a.Twin, b.Twin = b.Path, a.Path
		}
	}

	sort.Slice(s.Commands, func(i, j int) bool { return s.Commands[i].Path < s.Commands[j].Path })
	out, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "surface-dump:", err)
		os.Exit(1)
	}
	fmt.Println(string(out))
}

func flagsOf(fs *pflag.FlagSet) []flagInfo {
	out := []flagInfo{}
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Name == "help" {
			return
		}
		out = append(out, flagInfo{Name: f.Name, Short: f.Shorthand, Type: f.Value.Type()})
	})
	return out
}
