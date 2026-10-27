package cmd

// Audit D, CLI / local-state hardening role, surface 3 item 5: kill-window fund
// safety. A cash-mode destination's secret is generated client-side and only its
// commitment crosses the wire, so between the request leaving and the result
// being saved it must already be on disk: every call is written ahead with its
// secrets (auditD_cli_writeahead_test.go). This file keeps the probe helpers and
// the record that no signal handler narrows that window.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/store"
)

func auditDTempLedger(t *testing.T) {
	t.Helper()
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
}

// auditDLedgerBytesContain reports whether the raw cashctl.db file on disk
// contains needle anywhere in it. Deliberately the whole file rather than a
// targeted SELECT: the question is "could a process that restarts from scratch
// find this value anywhere in local state", and a byte scan cannot be fooled by
// looking in the wrong column.
func auditDLedgerBytesContain(t *testing.T, needle string) bool {
	t.Helper()
	path, err := store.Path()
	if err != nil {
		t.Fatalf("store.Path() error = %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false
		}
		t.Fatalf("reading %s: %v", filepath.Base(path), err)
	}
	return strings.Contains(string(b), needle)
}

// TestAuditD_CLI_NoSignalHandlerNarrowsTheKillWindow records the aggravating
// factor for D-CLI-1: cashctl installs no SIGINT/SIGTERM handler anywhere, so
// Ctrl-C during the "Consolidating…"/"Transferring…" spinner is an immediate,
// uninterruptible process death rather than something that could flush the
// ledger first. That turns a rare crash into an ordinary user action: the
// spinner is on screen for the whole duration of the wire call, which is
// exactly the moment an impatient user reaches for Ctrl-C.
//
// A source-structure test, with the same honest limit as auditC's AST test: it
// proves no handler is registered, not that registering one would be
// sufficient. Write-ahead persistence is the real fix; a handler alone would
// still lose a SIGKILL or a power loss.
func TestAuditD_CLI_NoSignalHandlerNarrowsTheKillWindow(t *testing.T) {
	roots := []string{".", "../internal", ".."}
	var hits []string
	seen := map[string]bool{}
	for _, root := range roots {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			abs, _ := filepath.Abs(path)
			if seen[abs] {
				return nil
			}
			seen[abs] = true
			b, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil
			}
			src := string(b)
			for _, marker := range []string{"signal.Notify", "signal.NotifyContext", "os.Interrupt", "syscall.SIGTERM"} {
				if strings.Contains(src, marker) {
					hits = append(hits, path+": "+marker)
				}
			}
			return nil
		})
	}
	if len(hits) > 0 {
		t.Fatalf("UNEXPECTED (aggravating factor would be refuted): signal handling found: %v", hits)
	}
	t.Log("cashctl registers no SIGINT/SIGTERM handler in any non-test source file, so " +
		"Ctrl-C during the in-flight window of D-CLI-1 is an immediate kill.")
}
