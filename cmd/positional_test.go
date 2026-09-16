package cmd

import "testing"

func TestResolvePositionalOrFlag(t *testing.T) {
	cmd := testCmdWithFlags(false, false)
	tests := []struct {
		name       string
		positional string
		flagValue  string
		want       string
		wantErr    bool
	}{
		{"positional only", "alice", "", "alice", false},
		{"flag only", "", "bob", "bob", false},
		{"neither", "", "", "", false},
		{"both, same value", "alice", "alice", "alice", false},
		{"both, different values", "alice", "bob", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolvePositionalOrFlag(cmd, tt.positional, "to", tt.flagValue)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolvePositionalOrFlag(%q, %q) = nil error, want an error", tt.positional, tt.flagValue)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolvePositionalOrFlag(%q, %q) error = %v", tt.positional, tt.flagValue, err)
			}
			if got != tt.want {
				t.Errorf("resolvePositionalOrFlag(%q, %q) = %q, want %q", tt.positional, tt.flagValue, got, tt.want)
			}
		})
	}
}

func TestResolvePositionalOrFlagUint64(t *testing.T) {
	cmd := testCmdWithFlags(false, false)
	tests := []struct {
		name       string
		positional string
		flagValue  uint64
		want       uint64
		wantErr    bool
	}{
		{"positional only", "3000", 0, 3000, false},
		{"flag only, no positional", "", 5000, 5000, false},
		{"neither", "", 0, 0, false},
		{"both, same value", "3000", 3000, 3000, false},
		{"both, different values", "3000", 4000, 0, true},
		{"positional not a number", "not-a-number", 0, 0, true},
		{"positional given, flag zero (unset)", "3000", 0, 3000, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolvePositionalOrFlagUint64(cmd, tt.positional, "split", tt.flagValue)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolvePositionalOrFlagUint64(%q, %d) = nil error, want an error", tt.positional, tt.flagValue)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolvePositionalOrFlagUint64(%q, %d) error = %v", tt.positional, tt.flagValue, err)
			}
			if got != tt.want {
				t.Errorf("resolvePositionalOrFlagUint64(%q, %d) = %d, want %d", tt.positional, tt.flagValue, got, tt.want)
			}
		})
	}
}
