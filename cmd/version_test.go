package cmd

import (
	"runtime/debug"
	"testing"
)

func TestFullVersion(t *testing.T) {
	defer func(v string, f func() (*debug.BuildInfo, bool)) { Version, buildInfo = v, f }(Version, buildInfo)
	info := func(main string, settings ...string) func() (*debug.BuildInfo, bool) {
		bi := &debug.BuildInfo{Main: debug.Module{Version: main}}
		for i := 0; i+1 < len(settings); i += 2 {
			bi.Settings = append(bi.Settings, debug.BuildSetting{Key: settings[i], Value: settings[i+1]})
		}
		return func() (*debug.BuildInfo, bool) { return bi, true }
	}
	tests := []struct {
		name, version string
		info          func() (*debug.BuildInfo, bool)
		want          string
	}{
		{"release ldflags win", "0.6.0+abc1234", info("(devel)", "vcs.revision", "fffffffffff"), "0.6.0+abc1234"},
		{"go build in a checkout", "dev", info("(devel)", "vcs.revision", "3a506ce0123", "vcs.modified", "false"), "dev+3a506ce"},
		{"dirty checkout", "dev", info("(devel)", "vcs.revision", "3a506ce0123", "vcs.modified", "true"), "dev+3a506ce.dirty"},
		{"pseudo-version names the commit", "dev", info("v0.6.0-rc.12.0.20261015102044-3a506ce6e3fb+dirty", "vcs.revision", "3a506ce6e3fb99", "vcs.modified", "true"), "v0.6.0-rc.12.0.20261015102044-3a506ce6e3fb+dirty"},
		{"go install @version", "dev", info("v0.6.0-rc.13"), "v0.6.0-rc.13"},
		{"no build info", "dev", func() (*debug.BuildInfo, bool) { return nil, false }, "dev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Version, buildInfo = tt.version, tt.info
			if got := fullVersion(); got != tt.want {
				t.Errorf("fullVersion() = %q, want %q", got, tt.want)
			}
		})
	}
}
