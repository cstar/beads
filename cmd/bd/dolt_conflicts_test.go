package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/dolt"
)

// fakeConflictResolver is a deterministic, no-Dolt-server stand-in for the store
// surface the resolve command drives, so the resolve wiring (table selection,
// per-table resolve, commit) is covered by the standard CI gate.
type fakeConflictResolver struct {
	conflicts   []storage.Conflict
	getErr      error
	resolveErr  error
	commitErr   error
	getCalls    int
	resolved    [][2]string // {table, strategy} per ResolveConflicts call, in order
	commitCalls int
	commitMsg   string
}

func (f *fakeConflictResolver) GetConflicts(_ context.Context) ([]storage.Conflict, error) {
	f.getCalls++
	return f.conflicts, f.getErr
}

func (f *fakeConflictResolver) ResolveConflicts(_ context.Context, table, strategy string) error {
	if f.resolveErr != nil {
		return f.resolveErr
	}
	f.resolved = append(f.resolved, [2]string{table, strategy})
	return nil
}

func (f *fakeConflictResolver) Commit(_ context.Context, message string) error {
	f.commitCalls++
	f.commitMsg = message
	return f.commitErr
}

// TestResolveConflictsCore covers the resolve command's wiring without a live
// Dolt store: explicit-table vs all-conflicted selection, the per-table resolve
// loop, the single commit, and the error/no-op paths.
func TestResolveConflictsCore(t *testing.T) {
	ctx := context.Background()

	t.Run("ExplicitTable", func(t *testing.T) {
		f := &fakeConflictResolver{}
		tables, err := resolveConflictsCore(ctx, f, "issues", "theirs")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(tables) != 1 || tables[0] != "issues" {
			t.Errorf("expected [issues], got %v", tables)
		}
		if f.getCalls != 0 {
			t.Errorf("explicit table must not call GetConflicts, got %d calls", f.getCalls)
		}
		if len(f.resolved) != 1 || f.resolved[0] != [2]string{"issues", "theirs"} {
			t.Errorf("expected resolve(issues, theirs), got %v", f.resolved)
		}
		if f.commitCalls != 1 {
			t.Errorf("expected exactly one commit, got %d", f.commitCalls)
		}
	})

	t.Run("AllConflictedTables", func(t *testing.T) {
		f := &fakeConflictResolver{conflicts: []storage.Conflict{{Field: "issues"}, {Field: "labels"}}}
		tables, err := resolveConflictsCore(ctx, f, "", "ours")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(tables) != 2 || tables[0] != "issues" || tables[1] != "labels" {
			t.Errorf("expected [issues labels], got %v", tables)
		}
		if f.getCalls != 1 {
			t.Errorf("expected one GetConflicts call, got %d", f.getCalls)
		}
		if len(f.resolved) != 2 || f.resolved[0] != [2]string{"issues", "ours"} || f.resolved[1] != [2]string{"labels", "ours"} {
			t.Errorf("expected both tables resolved with ours, got %v", f.resolved)
		}
		if f.commitCalls != 1 {
			t.Errorf("expected exactly one commit, got %d", f.commitCalls)
		}
	})

	t.Run("NoConflictsIsNoOp", func(t *testing.T) {
		f := &fakeConflictResolver{conflicts: nil}
		tables, err := resolveConflictsCore(ctx, f, "", "ours")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(tables) != 0 {
			t.Errorf("expected no tables, got %v", tables)
		}
		if f.commitCalls != 0 {
			t.Errorf("no conflicts must not commit, got %d commits", f.commitCalls)
		}
	})

	t.Run("ResolveErrorAborts", func(t *testing.T) {
		f := &fakeConflictResolver{resolveErr: errors.New("boom")}
		_, err := resolveConflictsCore(ctx, f, "issues", "theirs")
		if err == nil {
			t.Fatal("expected error from ResolveConflicts")
		}
		if f.commitCalls != 0 {
			t.Errorf("resolve failure must not commit, got %d commits", f.commitCalls)
		}
	})

	t.Run("CommitErrorPropagates", func(t *testing.T) {
		f := &fakeConflictResolver{commitErr: errors.New("commit failed")}
		_, err := resolveConflictsCore(ctx, f, "issues", "theirs")
		if err == nil {
			t.Fatal("expected error from Commit")
		}
	})

	t.Run("GetConflictsErrorPropagates", func(t *testing.T) {
		f := &fakeConflictResolver{getErr: errors.New("query failed")}
		_, err := resolveConflictsCore(ctx, f, "", "ours")
		if err == nil {
			t.Fatal("expected error from GetConflicts")
		}
		if f.commitCalls != 0 {
			t.Errorf("must not commit when selection fails, got %d", f.commitCalls)
		}
	})
}

// TestFormatResolveResult covers the resolve output formatter (text, JSON, no-op).
func TestFormatResolveResult(t *testing.T) {
	t.Run("Text", func(t *testing.T) {
		out, err := formatResolveResult([]string{"issues"}, "theirs", false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(out, "issues") || !strings.Contains(out, "theirs") {
			t.Errorf("text summary missing table/strategy: %q", out)
		}
	})

	t.Run("NoOp", func(t *testing.T) {
		out, err := formatResolveResult(nil, "ours", false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(strings.ToLower(out), "no conflicts") {
			t.Errorf("expected no-op message, got %q", out)
		}
	})

	t.Run("JSON", func(t *testing.T) {
		out, err := formatResolveResult([]string{"issues", "labels"}, "ours", true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var parsed struct {
			Strategy string   `json:"strategy"`
			Resolved []string `json:"resolved"`
		}
		if err := json.Unmarshal([]byte(out), &parsed); err != nil {
			t.Fatalf("output is not valid JSON: %v\n%s", err, out)
		}
		if parsed.Strategy != "ours" || len(parsed.Resolved) != 2 {
			t.Errorf("unexpected JSON: %+v", parsed)
		}
	})
}

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

// TestDoltConflictsResolveFlags verifies the `bd dolt conflicts resolve` surface:
// it is registered under `dolt conflicts`, exposes --ours/--theirs/--json, and its
// pure strategy validator enforces exactly-one-of --ours/--theirs.
func TestDoltConflictsResolveFlags(t *testing.T) {
	t.Run("Registered", func(t *testing.T) {
		conflicts := findSubcommand(doltCmd, "conflicts")
		if conflicts == nil {
			t.Fatal("`conflicts` is not registered under `dolt`")
		}
		resolve := findSubcommand(conflicts, "resolve")
		if resolve == nil {
			t.Fatal("`resolve` is not registered under `dolt conflicts`")
		}
		for _, f := range []string{"ours", "theirs", "json"} {
			if resolve.Flags().Lookup(f) == nil {
				t.Errorf("`dolt conflicts resolve` is missing the --%s flag", f)
			}
		}
	})

	t.Run("StrategyValidation", func(t *testing.T) {
		if _, err := resolveStrategy(false, false); err == nil {
			t.Error("expected an error when neither --ours nor --theirs is set")
		}
		if _, err := resolveStrategy(true, true); err == nil {
			t.Error("expected an error when both --ours and --theirs are set")
		}
		if s, err := resolveStrategy(true, false); err != nil || s != "ours" {
			t.Errorf("--ours: got (%q, %v), want (\"ours\", nil)", s, err)
		}
		if s, err := resolveStrategy(false, true); err != nil || s != "theirs" {
			t.Errorf("--theirs: got (%q, %v), want (\"theirs\", nil)", s, err)
		}
	})
}

// TestDoltPull_ConflictGuidance verifies that a pull leaving conflicts is
// classified and surfaced with actionable guidance (and never reports success):
//   - isConflictsRemainErr detects a (wrapped) *ConflictsRemainError from the store,
//   - isInConflictErr detects the pre-pull "table(s) ... are in conflict" wedge,
//   - printConflictResolutionGuidance points the operator at `bd dolt conflicts`.
//
// The doltPullCmd error branches wire these together; because the store now returns
// a non-nil error on unresolved conflicts (T-004), "Pull complete." is never reached.
func TestDoltPull_ConflictGuidance(t *testing.T) {
	t.Run("ClassifyConflictsRemain", func(t *testing.T) {
		base := &dolt.ConflictsRemainError{Counts: map[string]int{"issues": 2}}
		wrapped := fmt.Errorf("failed to pull from origin/main: %w", base)
		if !isConflictsRemainErr(wrapped) {
			t.Error("expected isConflictsRemainErr=true for a wrapped *ConflictsRemainError")
		}
		if isConflictsRemainErr(errors.New("some unrelated error")) {
			t.Error("expected isConflictsRemainErr=false for an unrelated error")
		}
		if isConflictsRemainErr(nil) {
			t.Error("expected isConflictsRemainErr=false for nil")
		}
	})

	t.Run("ClassifyInConflict", func(t *testing.T) {
		wedge := errors.New("failed to commit pending changes before pull: table(s) issues are in conflict")
		if !isInConflictErr(wedge) {
			t.Error("expected isInConflictErr=true for the pre-pull wedge error")
		}
		if isInConflictErr(errors.New("connection refused")) {
			t.Error("expected isInConflictErr=false for an unrelated error")
		}
	})

	t.Run("GuidanceMentionsResolveCommand", func(t *testing.T) {
		out := captureStderr(t, printConflictResolutionGuidance)
		if !strings.Contains(out, "bd dolt conflicts resolve") {
			t.Errorf("guidance should point at `bd dolt conflicts resolve`, got:\n%s", out)
		}
	})
}
