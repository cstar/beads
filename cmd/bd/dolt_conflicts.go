package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/storage"
)

// doltConflictsCmd is the parent for inspecting and resolving merge conflicts
// left in the Dolt working set (e.g. after a pull that surfaced a non-metadata
// data conflict). It is the first-class surface that replaces dropping to raw
// CALL DOLT_CONFLICTS_RESOLVE against the live server.
var doltConflictsCmd = &cobra.Command{
	Use:   "conflicts",
	Short: "Inspect and resolve Dolt merge conflicts",
	Long: `Inspect and resolve merge conflicts sitting in the Dolt working set.

A pull/merge that leaves a data conflict (e.g. on the issues table) wedges the
store: every subsequent write fails with "table(s) ... are in conflict". These
commands surface the conflicts and resolve them without dropping to raw SQL.

Subcommands:
  list                          Show per-table conflict counts
  resolve --ours|--theirs [t]   Resolve (and commit) conflicts, all tables or one`,
}

var doltConflictsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List tables with unresolved merge conflicts",
	Long: `List each table that has unresolved merge conflicts in the working set,
with its conflicting-row count. Prints "No conflicts." when the working set is
clean. Use --json for machine-readable output.`,
	Run: func(cmd *cobra.Command, args []string) {
		ctx := context.Background()
		st := storeForRawDoltSync(ctx, CapabilityDoltConflicts)
		if st == nil {
			fmt.Fprintf(os.Stderr, "Error: no store available\n")
			os.Exit(1)
		}
		conflicts, err := st.GetConflicts(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		asJSON, _ := cmd.Flags().GetBool("json")
		out, err := formatConflictsList(conflicts, asJSON)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(out)
	},
}

// formatConflictsList renders per-table conflict counts. Output is deterministic
// (sorted by table name). With asJSON it returns a JSON array of {table,count};
// otherwise a tab-separated table, or "No conflicts." when there are none.
func formatConflictsList(conflicts []storage.Conflict, asJSON bool) (string, error) {
	sorted := make([]storage.Conflict, len(conflicts))
	copy(sorted, conflicts)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Field < sorted[j].Field })

	if asJSON {
		type entry struct {
			Table string `json:"table"`
			Count int    `json:"count"`
		}
		entries := make([]entry, 0, len(sorted))
		for _, c := range sorted {
			entries = append(entries, entry{Table: c.Field, Count: c.Count})
		}
		b, err := json.MarshalIndent(entries, "", "  ")
		if err != nil {
			return "", fmt.Errorf("marshal conflicts: %w", err)
		}
		return string(b), nil
	}

	if len(sorted) == 0 {
		return "No conflicts.", nil
	}
	var sb strings.Builder
	sb.WriteString("TABLE\tCONFLICTS")
	for _, c := range sorted {
		sb.WriteString(fmt.Sprintf("\n%s\t%d", c.Field, c.Count))
	}
	return sb.String(), nil
}
