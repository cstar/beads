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

// resolveStrategy validates the mutually-exclusive --ours/--theirs flags and
// returns the Dolt strategy name. Exactly one must be set.
func resolveStrategy(ours, theirs bool) (string, error) {
	switch {
	case ours && theirs:
		return "", fmt.Errorf("--ours and --theirs are mutually exclusive")
	case ours:
		return "ours", nil
	case theirs:
		return "theirs", nil
	default:
		return "", fmt.Errorf("one of --ours or --theirs is required")
	}
}

// formatResolveResult renders the outcome of a resolve. With asJSON it returns a
// JSON object {strategy, resolved:[...tables]}; otherwise a one-line summary, or a
// no-op message when there was nothing to resolve.
func formatResolveResult(tables []string, strategy string, asJSON bool) (string, error) {
	if asJSON {
		b, err := json.MarshalIndent(map[string]any{
			"strategy": strategy,
			"resolved": tables,
		}, "", "  ")
		if err != nil {
			return "", fmt.Errorf("marshal resolve result: %w", err)
		}
		return string(b), nil
	}
	if len(tables) == 0 {
		return "No conflicts to resolve.", nil
	}
	return fmt.Sprintf("Resolved %d table(s) with --%s: %s", len(tables), strategy, strings.Join(tables, ", ")), nil
}

var doltConflictsResolveCmd = &cobra.Command{
	Use:   "resolve [table]",
	Short: "Resolve merge conflicts with --ours or --theirs (and commit)",
	Long: `Resolve merge conflicts in the working set and commit the resolution.

Exactly one of --ours / --theirs selects which side wins. With a [table]
argument only that table is resolved; with no argument every currently-conflicted
table is resolved with the same strategy. The resolution is committed, clearing
the wedged state so writes succeed again. Use --json for machine-readable output.`,
	Args: cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		ctx := context.Background()
		ours, _ := cmd.Flags().GetBool("ours")
		theirs, _ := cmd.Flags().GetBool("theirs")
		strategy, err := resolveStrategy(ours, theirs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		asJSON, _ := cmd.Flags().GetBool("json")

		st := storeForRawDoltSync(ctx, CapabilityDoltConflicts)
		if st == nil {
			fmt.Fprintf(os.Stderr, "Error: no store available\n")
			os.Exit(1)
		}

		// Target tables: the explicit arg, or every currently-conflicted table.
		var tables []string
		if len(args) == 1 {
			tables = []string{args[0]}
		} else {
			conflicts, err := st.GetConflicts(ctx)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
			for _, c := range conflicts {
				tables = append(tables, c.Field)
			}
		}

		if len(tables) == 0 {
			out, _ := formatResolveResult(nil, strategy, asJSON)
			fmt.Println(out)
			return
		}

		for _, tbl := range tables {
			if err := st.ResolveConflicts(ctx, tbl, strategy); err != nil {
				fmt.Fprintf(os.Stderr, "Error: resolving %s: %v\n", tbl, err)
				os.Exit(1)
			}
		}
		msg := fmt.Sprintf("bd dolt conflicts resolve --%s (%s)", strategy, strings.Join(tables, ", "))
		if err := st.Commit(ctx, msg); err != nil {
			fmt.Fprintf(os.Stderr, "Error: committing resolution: %v\n", err)
			os.Exit(1)
		}

		out, err := formatResolveResult(tables, strategy, asJSON)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(out)
	},
}
