package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// pflag reads backquoted text in a flag's usage as its value placeholder, so
// "(see `cashctl wallet show --json`)" rendered as `--token cashctl wallet show
// --json` (I-28). No flag's usage may contain a backquote.
func TestFlagUsage_HasNoBackquotes(t *testing.T) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		c.Flags().VisitAll(func(f *pflag.Flag) {
			if strings.Contains(f.Usage, "`") {
				t.Errorf("%s --%s: usage %q has a backquote, which pflag turns into the placeholder", c.CommandPath(), f.Name, f.Usage)
			}
		})
		for _, ch := range c.Commands() {
			walk(ch)
		}
	}
	walk(RootCmd)
}
