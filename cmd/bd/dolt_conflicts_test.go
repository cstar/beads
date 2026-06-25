package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/storage"
)

// findSubcommand returns the child of parent whose Name() == name, or nil.
func findSubcommand(parent *cobra.Command, name string) *cobra.Command {
	for _, c := range parent.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}

// TestDoltConflictsList verifies the `bd dolt conflicts list` surface: it is
// registered under `dolt conflicts`, exposes a --json flag, and its pure output
// formatter renders per-table counts (text + JSON) and "No conflicts." when clean.
func TestDoltConflictsList(t *testing.T) {
	t.Run("Registered", func(t *testing.T) {
		conflicts := findSubcommand(doltCmd, "conflicts")
		if conflicts == nil {
			t.Fatal("`conflicts` is not registered under `dolt`")
		}
		list := findSubcommand(conflicts, "list")
		if list == nil {
			t.Fatal("`list` is not registered under `dolt conflicts`")
		}
		if list.Flags().Lookup("json") == nil {
			t.Error("`dolt conflicts list` is missing the --json flag")
		}
	})

	t.Run("FormatText", func(t *testing.T) {
		out, err := formatConflictsList([]storage.Conflict{{Field: "issues", Count: 3}}, false)
		if err != nil {
			t.Fatalf("formatConflictsList: %v", err)
		}
		if !strings.Contains(out, "issues") || !strings.Contains(out, "3") {
			t.Errorf("expected table name and count in output, got %q", out)
		}
	})

	t.Run("FormatEmpty", func(t *testing.T) {
		out, err := formatConflictsList(nil, false)
		if err != nil {
			t.Fatalf("formatConflictsList: %v", err)
		}
		if !strings.Contains(out, "No conflicts") {
			t.Errorf("expected 'No conflicts.' for a clean store, got %q", out)
		}
	})

	t.Run("FormatJSON", func(t *testing.T) {
		out, err := formatConflictsList([]storage.Conflict{{Field: "issues", Count: 3}}, true)
		if err != nil {
			t.Fatalf("formatConflictsList(json): %v", err)
		}
		var got []map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("expected valid JSON array, got %q: %v", out, err)
		}
		if len(got) != 1 || got[0]["table"] != "issues" {
			t.Errorf("expected one entry for table 'issues', got %v", got)
		}
	})
}
