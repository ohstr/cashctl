package cmd

import (
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/output"
)

// Version is injected at build time via -ldflags
// "-X github.com/ohstr/cashctl/cmd.Version=..." (see .goreleaser.yaml) —
// "dev" for a plain `go build`/`go run`.
var Version = "dev"

// buildInfo is debug.ReadBuildInfo, swapped out in tests.
var buildInfo = debug.ReadBuildInfo

// fullVersion is Version, or for a build without -ldflags the module
// version `go install` recorded and the commit `go build` embedded, so a
// "dev" binary can still be matched to its source.
func fullVersion() string {
	if Version != "dev" {
		return Version
	}
	info, ok := buildInfo()
	if !ok {
		return Version
	}
	v := Version
	if mv := info.Main.Version; mv != "" && mv != "(devel)" {
		v = mv
	}
	var commit string
	dirty := false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			commit = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if len(commit) > 7 {
		commit = commit[:7]
	}
	// A pseudo-version (Go 1.24+ stamps one for a VCS build) already names
	// the commit, and "+dirty" for a modified checkout.
	if commit != "" && !strings.Contains(v, commit) {
		v += "+" + commit
		if dirty {
			v += ".dirty"
		}
	}
	return v
}

func init() {
	RootCmd.Version = fullVersion()
	RootCmd.SetVersionTemplate("{{.Version}}\n")
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the cashctl version",
		Args:  output.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonMode, _ := cmd.Flags().GetBool("json")
			if jsonMode {
				output.PrintJSON(map[string]any{"version": fullVersion(), "go": runtime.Version()})
				return nil
			}
			output.Println(fullVersion())
			return nil
		},
	}
}
