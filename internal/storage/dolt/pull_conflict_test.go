package dolt

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/steveyegge/beads/internal/storage/versioncontrolops"
)

// TestPullAutoResolveFKConstraintViolations verifies ADR-0018 Layer 2:
// a 3-way merge that deletes a parent issue on OURS (cascade-removing its
// children) while THEIRS adds a child row (label) for that still-live issue
// leaves an FK constraint violation in dolt_constraint_violations_labels.
// tryAutoResolveFKConstraintViolations must re-insert the parent issue from
// THEIRS (the safe path) so the commit succeeds and both the issue and its
// label survive.
func TestPullAutoResolveFKConstraintViolations(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	db := store.db

	var currentBranch string
	if err := db.QueryRowContext(ctx, "SELECT active_branch()").Scan(&currentBranch); err != nil {
		t.Fatalf("failed to get current branch: %v", err)
	}

	// Base commit: issue X exists (no children yet). This is the common
	// ancestor both sides diverge from.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type) VALUES ('fk-test-x', 'X', '', '', '', '', 'open', 2, 'task')"); err != nil {
		t.Fatalf("failed to insert base issue: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'base: issue X')"); err != nil {
		t.Fatalf("failed to commit base: %v", err)
	}

	// THEIRS branch from the base (HEAD): adds a label for X (X stays live).
	remoteBranch := currentBranch + "_fkremote"
	if _, err := db.ExecContext(ctx, "CALL DOLT_BRANCH(?, 'HEAD')", remoteBranch); err != nil {
		t.Fatalf("failed to create remote branch: %v", err)
	}
	defer func() {
		db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", currentBranch)
		db.ExecContext(ctx, "CALL DOLT_BRANCH('-D', ?)", remoteBranch)
	}()

	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", remoteBranch); err != nil {
		t.Fatalf("failed to checkout remote branch: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO labels (issue_id, label) VALUES ('fk-test-x', 'urgent')"); err != nil {
		t.Fatalf("failed to insert label on remote branch: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'theirs: label on X')"); err != nil {
		t.Fatalf("failed to commit on remote branch: %v", err)
	}

	// OURS (current branch): DELETE issue X (FK ON DELETE CASCADE removes any
	// children of X on OURS — there are none yet).
	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", currentBranch); err != nil {
		t.Fatalf("failed to checkout current branch: %v", err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM issues WHERE id = 'fk-test-x'"); err != nil {
		t.Fatalf("failed to delete issue on current branch: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'ours: delete X')"); err != nil {
		t.Fatalf("failed to commit delete on current branch: %v", err)
	}

	// Merge THEIRS into OURS. The merge combines OURS deleting X with THEIRS
	// adding labels(X) — Dolt does not re-fire the cascade, so the label row
	// orphans into dolt_constraint_violations_labels.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "SET @@dolt_allow_commit_conflicts = 1"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("failed to set dolt_allow_commit_conflicts: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "SET @@dolt_force_transaction_commit = 1"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("failed to set dolt_force_transaction_commit: %v", err)
	}
	_, mergeErr := tx.ExecContext(ctx, "CALL DOLT_MERGE(?)", remoteBranch)
	t.Logf("merge result: %v", mergeErr)

	// Verify the violation actually landed; if Dolt auto-handled it, skip.
	var nViol int
	_ = tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(num_violations),0) FROM dolt_constraint_violations").Scan(&nViol)
	t.Logf("constraint violations after merge: %d", nViol)
	if nViol == 0 {
		_ = tx.Rollback()
		t.Skip("merge produced no FK constraint violations on this Dolt version — cannot exercise resolution path")
	}

	// The helper uses theirsRef = remotes/<remote>/<branch>. The test has no
	// real remote tracking branch, so point AS OF reads at the local THEIRS
	// branch by setting the store's remote/branch so the computed ref resolves.
	// remotes/<remoteBranch> is not valid; instead we directly call the
	// per-table resolver with the local THEIRS branch as the ref to exercise
	// the core logic, then mirror the wrapper's commit.
	resolved, resolveErr := store.tryAutoResolveFKConstraintViolationsWithRef(ctx, tx, remoteBranch)
	if resolveErr != nil {
		_ = tx.Rollback()
		t.Fatalf("tryAutoResolveFKConstraintViolations error: %v (mergeErr: %v)", resolveErr, mergeErr)
	}
	if !resolved {
		_ = tx.Rollback()
		t.Fatalf("FK violations were not auto-resolved (mergeErr: %v)", mergeErr)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("failed to commit after FK auto-resolve: %v", err)
	}

	// Issue X must be live again (re-inserted from THEIRS) AND its label present.
	var idCount int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM issues WHERE id = 'fk-test-x'").Scan(&idCount); err != nil {
		t.Fatalf("failed to count issue X: %v", err)
	}
	if idCount != 1 {
		t.Errorf("expected issue X to be re-inserted (count 1), got %d", idCount)
	}
	var labelCount int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM labels WHERE issue_id = 'fk-test-x' AND label = 'urgent'").Scan(&labelCount); err != nil {
		t.Fatalf("failed to count label: %v", err)
	}
	if labelCount != 1 {
		t.Errorf("expected label on X to survive (count 1), got %d", labelCount)
	}
}

// TestPullAutoResolveFKViolationsOrphanBothSides verifies the both-sides-absent
// path: when the parent issue is gone on BOTH the THEIRS ref and local HEAD,
// the orphaned child rows are DELETED (not re-inserted) and resolution
// succeeds.
//
// The orphan is manufactured exactly as in the positive test (OURS deletes Y,
// THEIRS adds a label to still-live Y → FK violation on labels). To exercise
// the delete branch deterministically we drive the resolver with a THEIRS ref
// on which Y does NOT exist — the OURS pre-merge commit (HEAD~1), where Y has
// already been deleted. With Y absent on both that ref and local HEAD the
// resolver must delete the orphaned label rows.
func TestPullAutoResolveFKViolationsOrphanBothSides(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	db := store.db

	var currentBranch string
	if err := db.QueryRowContext(ctx, "SELECT active_branch()").Scan(&currentBranch); err != nil {
		t.Fatalf("failed to get current branch: %v", err)
	}

	// Base: issue Y exists (no children yet).
	if _, err := db.ExecContext(ctx,
		"INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type) VALUES ('fk-orphan-y', 'Y', '', '', '', '', 'open', 2, 'task')"); err != nil {
		t.Fatalf("failed to insert base issue: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'base: issue Y')"); err != nil {
		t.Fatalf("failed to commit base: %v", err)
	}

	// THEIRS branch from base: add a label for still-live Y.
	remoteBranch := currentBranch + "_orphanremote"
	if _, err := db.ExecContext(ctx, "CALL DOLT_BRANCH(?, 'HEAD')", remoteBranch); err != nil {
		t.Fatalf("failed to create remote branch: %v", err)
	}
	defer func() {
		db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", currentBranch)
		db.ExecContext(ctx, "CALL DOLT_BRANCH('-D', ?)", remoteBranch)
	}()

	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", remoteBranch); err != nil {
		t.Fatalf("failed to checkout remote branch: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO labels (issue_id, label) VALUES ('fk-orphan-y', 'extra')"); err != nil {
		t.Fatalf("failed to insert label on remote branch: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'theirs: label on Y')"); err != nil {
		t.Fatalf("failed to commit on remote branch: %v", err)
	}

	// OURS: delete Y (cascade removes nothing locally). This commit becomes
	// HEAD~1 after the merge commit, and Y is absent on it.
	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", currentBranch); err != nil {
		t.Fatalf("failed to checkout current branch: %v", err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM issues WHERE id = 'fk-orphan-y'"); err != nil {
		t.Fatalf("failed to delete issue on current branch: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'ours: delete Y')"); err != nil {
		t.Fatalf("failed to commit delete on current branch: %v", err)
	}
	// Capture the OURS pre-merge commit hash to use as a THEIRS ref where Y is
	// absent (ValidateRef rejects '~', so we cannot pass HEAD~1 literally).
	var oursCommit string
	if err := db.QueryRowContext(ctx, "SELECT commit_hash FROM dolt_log LIMIT 1").Scan(&oursCommit); err != nil {
		t.Fatalf("failed to get OURS commit hash: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "SET @@dolt_allow_commit_conflicts = 1"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("failed to set dolt_allow_commit_conflicts: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "SET @@dolt_force_transaction_commit = 1"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("failed to set dolt_force_transaction_commit: %v", err)
	}
	_, mergeErr := tx.ExecContext(ctx, "CALL DOLT_MERGE(?)", remoteBranch)
	t.Logf("merge result: %v", mergeErr)

	var nViol int
	_ = tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(num_violations),0) FROM dolt_constraint_violations").Scan(&nViol)
	t.Logf("constraint violations after merge: %d", nViol)
	if nViol == 0 {
		_ = tx.Rollback()
		t.Skip("merge produced no FK constraint violations on this Dolt version — cannot exercise orphan-delete path")
	}

	// Drive the resolver with the pre-merge OURS commit as the THEIRS ref. Y is
	// absent there AND on local HEAD → resolver must DELETE the orphaned label
	// rows, not re-insert Y.
	resolved, resolveErr := store.tryAutoResolveFKConstraintViolationsWithRef(ctx, tx, oursCommit)
	if resolveErr != nil {
		_ = tx.Rollback()
		t.Fatalf("tryAutoResolveFKConstraintViolations error: %v (mergeErr: %v)", resolveErr, mergeErr)
	}
	if !resolved {
		_ = tx.Rollback()
		t.Fatalf("FK violations were not auto-resolved (mergeErr: %v)", mergeErr)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("failed to commit after FK auto-resolve: %v", err)
	}

	// Y must stay absent, and no label rows for Y should remain.
	var idCount int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM issues WHERE id = 'fk-orphan-y'").Scan(&idCount); err != nil {
		t.Fatalf("failed to count issue Y: %v", err)
	}
	if idCount != 0 {
		t.Errorf("expected issue Y to stay deleted (count 0), got %d", idCount)
	}
	var labelCount int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM labels WHERE issue_id = 'fk-orphan-y'").Scan(&labelCount); err != nil {
		t.Fatalf("failed to count labels for Y: %v", err)
	}
	if labelCount != 0 {
		t.Errorf("expected orphaned labels for Y to be deleted (count 0), got %d", labelCount)
	}
}

// TestPullAutoResolveMetadataConflicts verifies that merge conflicts limited to
// the metadata table are automatically resolved with "theirs" strategy (GH#2466).
// This simulates the scenario where two machines each write different
// dolt_auto_push_* values to the metadata table, causing recurring conflicts on pull.
func TestPullAutoResolveMetadataConflicts(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	db := store.db

	// Record the current branch (our test branch).
	var currentBranch string
	if err := db.QueryRowContext(ctx, "SELECT active_branch()").Scan(&currentBranch); err != nil {
		t.Fatalf("failed to get current branch: %v", err)
	}

	// Insert a metadata row on the current branch and commit.
	if _, err := db.ExecContext(ctx, "INSERT INTO metadata (`key`, value) VALUES ('dolt_auto_push_commit', 'aaa')"); err != nil {
		t.Fatalf("failed to insert metadata on current branch: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'local metadata')"); err != nil {
		t.Fatalf("failed to commit on current branch: %v", err)
	}

	// Create a divergent branch to simulate the remote.
	remoteBranch := currentBranch + "_remote"
	// Branch from current branch's parent (HEAD~1).
	if _, err := db.ExecContext(ctx, "CALL DOLT_BRANCH(?, 'HEAD~1')", remoteBranch); err != nil {
		t.Fatalf("failed to create remote branch: %v", err)
	}
	defer func() {
		db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", currentBranch)
		db.ExecContext(ctx, "CALL DOLT_BRANCH('-D', ?)", remoteBranch)
	}()

	// Switch to remote branch and insert conflicting metadata.
	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", remoteBranch); err != nil {
		t.Fatalf("failed to checkout remote branch: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO metadata (`key`, value) VALUES ('dolt_auto_push_commit', 'bbb')"); err != nil {
		t.Fatalf("failed to insert metadata on remote branch: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'remote metadata')"); err != nil {
		t.Fatalf("failed to commit on remote branch: %v", err)
	}

	// Switch back to current branch.
	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", currentBranch); err != nil {
		t.Fatalf("failed to checkout current branch: %v", err)
	}

	// Merge the remote branch in a transaction with dolt_allow_commit_conflicts.
	// This simulates what pullWithAutoResolve does internally.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}

	if _, err := tx.ExecContext(ctx, "SET @@dolt_allow_commit_conflicts = 1"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("failed to set dolt_allow_commit_conflicts: %v", err)
	}

	_, mergeErr := tx.ExecContext(ctx, "CALL DOLT_MERGE(?)", remoteBranch)
	// mergeErr may or may not be nil depending on Dolt version.

	// Try auto-resolve.
	resolved, resolveErr := store.tryAutoResolveMetadataConflicts(ctx, tx)
	if resolveErr != nil {
		_ = tx.Rollback()
		t.Fatalf("tryAutoResolveMetadataConflicts error: %v (mergeErr: %v)", resolveErr, mergeErr)
	}
	if !resolved {
		_ = tx.Rollback()
		if mergeErr != nil {
			t.Fatalf("merge failed and metadata conflicts were not auto-resolved: %v", mergeErr)
		}
		// Clean merge, no conflicts to resolve — verify the value.
		t.Log("merge succeeded without conflicts (auto-merge)")
		return
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("failed to commit after auto-resolve: %v", err)
	}

	// Verify the metadata value is "theirs" (bbb from remote).
	var value string
	if err := db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE `key` = 'dolt_auto_push_commit'").Scan(&value); err != nil {
		t.Fatalf("failed to read resolved metadata: %v", err)
	}
	if value != "bbb" {
		t.Errorf("expected metadata value 'bbb' (theirs), got %q", value)
	}
}

// fkMergeViolation is shared setup for the FK tests: on a base commit issue
// `issueID` exists; THEIRS (returned remoteBranch) adds a child row referencing
// it via `childInsert`; OURS deletes the issue. It performs the merge inside the
// returned tx and reports how many constraint violations resulted. Callers must
// Rollback/Commit the tx. Returns nViol==0 when Dolt auto-merged (caller should
// skip).
func fkMergeViolation(t *testing.T, store *DoltStore, ctx context.Context, issueID, childInsert string) (tx *sql.Tx, remoteBranch string, mergeErr error, nViol int) {
	t.Helper()
	db := store.db

	var currentBranch string
	if err := db.QueryRowContext(ctx, "SELECT active_branch()").Scan(&currentBranch); err != nil {
		t.Fatalf("failed to get current branch: %v", err)
	}

	if _, err := db.ExecContext(ctx,
		"INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type) VALUES (?, 'parent', '', '', '', '', 'open', 2, 'task')", issueID); err != nil {
		t.Fatalf("failed to insert base issue: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'base: parent issue')"); err != nil {
		t.Fatalf("failed to commit base: %v", err)
	}

	remoteBranch = currentBranch + "_fkr"
	if _, err := db.ExecContext(ctx, "CALL DOLT_BRANCH(?, 'HEAD')", remoteBranch); err != nil {
		t.Fatalf("failed to create remote branch: %v", err)
	}
	t.Cleanup(func() {
		db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", currentBranch)
		db.ExecContext(ctx, "CALL DOLT_BRANCH('-D', ?)", remoteBranch)
	})

	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", remoteBranch); err != nil {
		t.Fatalf("failed to checkout remote branch: %v", err)
	}
	if _, err := db.ExecContext(ctx, childInsert); err != nil {
		t.Fatalf("failed to insert child on remote branch: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'theirs: child row')"); err != nil {
		t.Fatalf("failed to commit on remote branch: %v", err)
	}

	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", currentBranch); err != nil {
		t.Fatalf("failed to checkout current branch: %v", err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM issues WHERE id = ?", issueID); err != nil {
		t.Fatalf("failed to delete issue on current branch: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'ours: delete parent')"); err != nil {
		t.Fatalf("failed to commit delete on current branch: %v", err)
	}

	var err error
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "SET @@dolt_allow_commit_conflicts = 1"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("failed to set dolt_allow_commit_conflicts: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "SET @@dolt_force_transaction_commit = 1"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("failed to set dolt_force_transaction_commit: %v", err)
	}
	_, mergeErr = tx.ExecContext(ctx, "CALL DOLT_MERGE(?)", remoteBranch)
	t.Logf("merge result: %v", mergeErr)
	_ = tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(num_violations),0) FROM dolt_constraint_violations").Scan(&nViol)
	t.Logf("constraint violations after merge: %d", nViol)
	return tx, remoteBranch, mergeErr, nViol
}

// TestPullAutoResolveFKViolationsEvents verifies the re-insert safe path on the
// events child table (not just labels): OURS deletes issue E, THEIRS adds an
// event for still-live E → FK violation on events → parent re-inserted, event
// survives.
func TestPullAutoResolveFKViolationsEvents(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()
	db := store.db

	tx, remoteBranch, mergeErr, nViol := fkMergeViolation(t, store, ctx, "fk-evt-e",
		"INSERT INTO events (issue_id, event_type, actor) VALUES ('fk-evt-e', 'commented', 'tester')")
	if nViol == 0 {
		_ = tx.Rollback()
		t.Skip("merge produced no FK constraint violations on this Dolt version")
	}

	resolved, resolveErr := store.tryAutoResolveFKConstraintViolationsWithRef(ctx, tx, remoteBranch)
	if resolveErr != nil {
		_ = tx.Rollback()
		t.Fatalf("resolve error: %v (mergeErr: %v)", resolveErr, mergeErr)
	}
	if !resolved {
		_ = tx.Rollback()
		t.Fatalf("events FK violation not auto-resolved (mergeErr: %v)", mergeErr)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit after resolve: %v", err)
	}

	var issueCount, eventCount int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM issues WHERE id = 'fk-evt-e'").Scan(&issueCount); err != nil {
		t.Fatalf("count issue: %v", err)
	}
	if issueCount != 1 {
		t.Errorf("expected issue E re-inserted (1), got %d", issueCount)
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM events WHERE issue_id = 'fk-evt-e'").Scan(&eventCount); err != nil {
		t.Fatalf("count event: %v", err)
	}
	if eventCount != 1 {
		t.Errorf("expected event on E to survive (1), got %d", eventCount)
	}
}

// TestPullAutoResolveFKViolationsUnknownTableSurfaces verifies the "surface,
// never guess" contract at the table-allowlist gate: when a constraint violation
// exists on a table that is NOT a known issue-child table, the resolver returns
// (false, nil) so pullWithAutoResolve falls back to the original error path
// rather than touching data it does not understand.
//
// child_counters has FOREIGN KEY (parent_id) REFERENCES issues(id) ON DELETE
// CASCADE but is intentionally OUTSIDE issueChildTables (L2 scope). A merge that
// deletes the parent on OURS while THEIRS adds a counter row leaves a violation
// on child_counters, which must NOT be auto-resolved.
func TestPullAutoResolveFKViolationsUnknownTableSurfaces(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()
	db := store.db

	// Require child_counters with the parent_id FK (migration 0008).
	var hasTable int
	_ = db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'child_counters'").Scan(&hasTable)
	if hasTable == 0 {
		t.Skip("child_counters table not present")
	}

	var currentBranch string
	if err := db.QueryRowContext(ctx, "SELECT active_branch()").Scan(&currentBranch); err != nil {
		t.Fatalf("get current branch: %v", err)
	}

	if _, err := db.ExecContext(ctx,
		"INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type) VALUES ('fk-cc-p', 'P', '', '', '', '', 'open', 2, 'task')"); err != nil {
		t.Fatalf("insert base issue: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'base: P')"); err != nil {
		t.Fatalf("commit base: %v", err)
	}

	remoteBranch := currentBranch + "_ccr"
	if _, err := db.ExecContext(ctx, "CALL DOLT_BRANCH(?, 'HEAD')", remoteBranch); err != nil {
		t.Fatalf("create remote branch: %v", err)
	}
	defer func() {
		db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", currentBranch)
		db.ExecContext(ctx, "CALL DOLT_BRANCH('-D', ?)", remoteBranch)
	}()

	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", remoteBranch); err != nil {
		t.Fatalf("checkout remote: %v", err)
	}
	// Insert a child_counters row referencing P. Use a permissive column set;
	// fall back to skipping if the schema differs.
	if _, err := db.ExecContext(ctx, "INSERT INTO child_counters (parent_id) VALUES ('fk-cc-p')"); err != nil {
		t.Skipf("could not insert child_counters row (schema differs): %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'theirs: counter for P')"); err != nil {
		t.Fatalf("commit theirs: %v", err)
	}

	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", currentBranch); err != nil {
		t.Fatalf("checkout current: %v", err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM issues WHERE id = 'fk-cc-p'"); err != nil {
		t.Fatalf("delete P on current: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'ours: delete P')"); err != nil {
		t.Fatalf("commit ours: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "SET @@dolt_allow_commit_conflicts = 1"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("set allow_commit_conflicts: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "SET @@dolt_force_transaction_commit = 1"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("set force_transaction_commit: %v", err)
	}
	_, mergeErr := tx.ExecContext(ctx, "CALL DOLT_MERGE(?)", remoteBranch)
	t.Logf("merge result: %v", mergeErr)
	defer tx.Rollback()

	var nViol int
	var hasCCViol int
	_ = tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(num_violations),0) FROM dolt_constraint_violations").Scan(&nViol)
	_ = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_constraint_violations WHERE `table` = 'child_counters'").Scan(&hasCCViol)
	t.Logf("constraint violations after merge: %d (child_counters rows: %d)", nViol, hasCCViol)
	if nViol == 0 || hasCCViol == 0 {
		t.Skip("merge produced no child_counters FK violation on this Dolt version")
	}

	// An unknown violating table must cause (false, nil) — surfaced, not resolved.
	resolved, resolveErr := store.tryAutoResolveFKConstraintViolationsWithRef(ctx, tx, remoteBranch)
	if resolveErr != nil {
		t.Fatalf("expected (false,nil) for unknown table, got error: %v", resolveErr)
	}
	if resolved {
		t.Errorf("expected unknown-table violation NOT to be auto-resolved (got resolved=true)")
	}
}

// TestPullAutoResolveFKViolationsDependencies exercises the dependencies table,
// which has TWO issue-referencing FK columns (issue_id and depends_on_issue_id).
// OURS deletes issue P; THEIRS adds a dependency whose depends_on_issue_id
// references still-live P → FK violation on dependencies.depends_on_issue_id →
// P re-inserted, the dependency survives.
func TestPullAutoResolveFKViolationsDependencies(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()
	db := store.db

	// Require the split-dependencies schema (migration 0041+).
	var hasCol int
	_ = db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'dependencies' AND COLUMN_NAME = 'depends_on_issue_id'").Scan(&hasCol)
	if hasCol == 0 {
		t.Skip("dependencies.depends_on_issue_id not present (pre-0041 schema)")
	}

	var currentBranch string
	if err := db.QueryRowContext(ctx, "SELECT active_branch()").Scan(&currentBranch); err != nil {
		t.Fatalf("get current branch: %v", err)
	}

	// Base: parent P and source S both exist (S is the dependency's issue_id,
	// P is the depends_on target). Only P is deleted on OURS to isolate the
	// depends_on_issue_id FK column.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type) VALUES ('fk-dep-p', 'P', '', '', '', '', 'open', 2, 'task'), ('fk-dep-s', 'S', '', '', '', '', 'open', 2, 'task')"); err != nil {
		t.Fatalf("insert base issues: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'base: P and S')"); err != nil {
		t.Fatalf("commit base: %v", err)
	}

	remoteBranch := currentBranch + "_depr"
	if _, err := db.ExecContext(ctx, "CALL DOLT_BRANCH(?, 'HEAD')", remoteBranch); err != nil {
		t.Fatalf("create remote branch: %v", err)
	}
	defer func() {
		db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", currentBranch)
		db.ExecContext(ctx, "CALL DOLT_BRANCH('-D', ?)", remoteBranch)
	}()

	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", remoteBranch); err != nil {
		t.Fatalf("checkout remote: %v", err)
	}
	// S depends on P (depends_on_issue_id = P). depends_on_id is generated.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO dependencies (issue_id, depends_on_issue_id, type, created_by) VALUES ('fk-dep-s', 'fk-dep-p', 'blocks', 'tester')"); err != nil {
		t.Fatalf("insert dependency on remote branch: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'theirs: S depends on P')"); err != nil {
		t.Fatalf("commit theirs: %v", err)
	}

	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", currentBranch); err != nil {
		t.Fatalf("checkout current: %v", err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM issues WHERE id = 'fk-dep-p'"); err != nil {
		t.Fatalf("delete P on current: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'ours: delete P')"); err != nil {
		t.Fatalf("commit ours: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "SET @@dolt_allow_commit_conflicts = 1"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("set allow_commit_conflicts: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "SET @@dolt_force_transaction_commit = 1"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("set force_transaction_commit: %v", err)
	}
	_, mergeErr := tx.ExecContext(ctx, "CALL DOLT_MERGE(?)", remoteBranch)
	t.Logf("merge result: %v", mergeErr)
	var nViol int
	_ = tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(num_violations),0) FROM dolt_constraint_violations").Scan(&nViol)
	t.Logf("constraint violations after merge: %d", nViol)
	if nViol == 0 {
		_ = tx.Rollback()
		t.Skip("merge produced no FK constraint violations on this Dolt version")
	}

	resolved, resolveErr := store.tryAutoResolveFKConstraintViolationsWithRef(ctx, tx, remoteBranch)
	if resolveErr != nil {
		_ = tx.Rollback()
		t.Fatalf("resolve error: %v (mergeErr: %v)", resolveErr, mergeErr)
	}
	if !resolved {
		_ = tx.Rollback()
		t.Fatalf("dependencies FK violation not auto-resolved (mergeErr: %v)", mergeErr)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit after resolve: %v", err)
	}

	// P must be re-inserted, and the dependency row must survive.
	var pCount, depCount int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM issues WHERE id = 'fk-dep-p'").Scan(&pCount); err != nil {
		t.Fatalf("count P: %v", err)
	}
	if pCount != 1 {
		t.Errorf("expected P re-inserted (1), got %d", pCount)
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dependencies WHERE issue_id = 'fk-dep-s' AND depends_on_issue_id = 'fk-dep-p'").Scan(&depCount); err != nil {
		t.Fatalf("count dependency: %v", err)
	}
	if depCount != 1 {
		t.Errorf("expected dependency to survive (1), got %d", depCount)
	}
}

// TestPullAutoResolveSkipsNonMetadataConflicts verifies that conflicts on
// tables other than metadata are NOT auto-resolved.
func TestPullAutoResolveSkipsNonMetadataConflicts(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	db := store.db

	var currentBranch string
	if err := db.QueryRowContext(ctx, "SELECT active_branch()").Scan(&currentBranch); err != nil {
		t.Fatalf("failed to get current branch: %v", err)
	}

	// Create an issue on the current branch.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type) VALUES ('conflict-test', 'Local Title', '', '', '', '', 'open', 2, 'task')"); err != nil {
		t.Fatalf("failed to insert issue on current branch: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'local issue')"); err != nil {
		t.Fatalf("failed to commit on current branch: %v", err)
	}

	// Create divergent branch from parent.
	remoteBranch := currentBranch + "_remote2"
	if _, err := db.ExecContext(ctx, "CALL DOLT_BRANCH(?, 'HEAD~1')", remoteBranch); err != nil {
		t.Fatalf("failed to create remote branch: %v", err)
	}
	defer func() {
		db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", currentBranch)
		db.ExecContext(ctx, "CALL DOLT_BRANCH('-D', ?)", remoteBranch)
	}()

	// Insert conflicting issue on remote branch (same PK, different title).
	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", remoteBranch); err != nil {
		t.Fatalf("failed to checkout remote branch: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type) VALUES ('conflict-test', 'Remote Title', '', '', '', '', 'open', 2, 'task')"); err != nil {
		t.Fatalf("failed to insert issue on remote branch: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'remote issue')"); err != nil {
		t.Fatalf("failed to commit on remote branch: %v", err)
	}

	// Switch back and merge.
	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", currentBranch); err != nil {
		t.Fatalf("failed to checkout current branch: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}

	if _, err := tx.ExecContext(ctx, "SET @@dolt_allow_commit_conflicts = 1"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("failed to set dolt_allow_commit_conflicts: %v", err)
	}

	_, mergeErr := tx.ExecContext(ctx, "CALL DOLT_MERGE(?)", remoteBranch)

	// Issues table conflict should NOT be auto-resolved.
	resolved, resolveErr := store.tryAutoResolveMetadataConflicts(ctx, tx)
	_ = tx.Rollback()

	if mergeErr == nil && resolveErr == nil && !resolved {
		// Clean merge — Dolt auto-merged the issue changes.
		t.Skip("merge succeeded without conflicts — cannot test non-metadata conflict path")
		return
	}

	if resolveErr != nil {
		// Error checking conflicts is acceptable for some Dolt versions.
		t.Logf("tryAutoResolveMetadataConflicts returned error: %v", resolveErr)
		return
	}

	if resolved {
		t.Error("expected non-metadata conflicts NOT to be auto-resolved")
	}
}

// seedIssuesConflict creates a real issues-table data conflict in the working
// set using the divergent-branch pattern (same PK inserted with different titles
// on two branches, then merged with dolt_allow_commit_conflicts). It returns an
// open transaction holding the conflict; the caller must Rollback or Commit it.
// The conflict is visible via SELECT ... FROM dolt_conflicts on the returned tx.
func seedIssuesConflict(t *testing.T, store *DoltStore, ctx context.Context, pk string) *sql.Tx {
	t.Helper()
	db := store.db

	var currentBranch string
	if err := db.QueryRowContext(ctx, "SELECT active_branch()").Scan(&currentBranch); err != nil {
		t.Fatalf("failed to get current branch: %v", err)
	}

	if _, err := db.ExecContext(ctx,
		"INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type) VALUES (?, 'Local Title', '', '', '', '', 'open', 2, 'task')", pk); err != nil {
		t.Fatalf("failed to insert local issue: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'local issue')"); err != nil {
		t.Fatalf("failed to commit local issue: %v", err)
	}

	remoteBranch := currentBranch + "_remote_" + pk
	if _, err := db.ExecContext(ctx, "CALL DOLT_BRANCH(?, 'HEAD~1')", remoteBranch); err != nil {
		t.Fatalf("failed to create remote branch: %v", err)
	}
	t.Cleanup(func() {
		db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", currentBranch)
		db.ExecContext(ctx, "CALL DOLT_BRANCH('-D', ?)", remoteBranch)
	})

	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", remoteBranch); err != nil {
		t.Fatalf("failed to checkout remote branch: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type) VALUES (?, 'Remote Title', '', '', '', '', 'open', 2, 'task')", pk); err != nil {
		t.Fatalf("failed to insert remote issue: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'remote issue')"); err != nil {
		t.Fatalf("failed to commit remote issue: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", currentBranch); err != nil {
		t.Fatalf("failed to checkout current branch: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin tx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "SET @@dolt_allow_commit_conflicts = 1"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("failed to set allow_commit_conflicts: %v", err)
	}
	if _, mergeErr := tx.ExecContext(ctx, "CALL DOLT_MERGE(?)", remoteBranch); mergeErr != nil {
		// Some Dolt versions surface conflicts as an error here; the conflict is
		// still left in the working set, which is what we want.
		t.Logf("DOLT_MERGE returned (expected on conflict): %v", mergeErr)
	}
	return tx
}

// TestGetConflicts_ReportsCount verifies that GetConflicts reports the per-table
// conflict count from dolt_conflicts.num_conflicts, not just the table name.
func TestGetConflicts_ReportsCount(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	tx := seedIssuesConflict(t, store, ctx, "conflict-count")
	defer tx.Rollback()

	conflicts, err := versioncontrolops.GetConflicts(ctx, tx)
	if err != nil {
		t.Fatalf("GetConflicts error: %v", err)
	}
	if len(conflicts) == 0 {
		t.Skip("Dolt auto-merged — no conflict materialized to count")
	}

	var found bool
	for _, c := range conflicts {
		if c.Field == "issues" {
			found = true
			if c.Count < 1 {
				t.Errorf("expected issues conflict Count >= 1, got %d", c.Count)
			}
		}
	}
	if !found {
		t.Errorf("expected an 'issues' table conflict, got %+v", conflicts)
	}
}

// TestPullWithAutoResolve_SurfacesUnresolvedConflicts verifies that an
// issues-table data conflict surviving metadata/FK auto-resolution is surfaced as
// a typed *ConflictsRemainError naming the table with count >= 1, instead of being
// silently committed (the bug: pullWithAutoResolve returned nil). It exercises
// detectRemainingConflicts on the pull transaction — the same tx-helper idiom as
// TestPullAutoResolveMetadataConflicts uses for tryAutoResolveMetadataConflicts —
// because pullWithAutoResolve opens its own connection (a fresh session on the
// default branch), which the per-test isolated branch harness cannot drive.
func TestPullWithAutoResolve_SurfacesUnresolvedConflicts(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	tx := seedIssuesConflict(t, store, ctx, "pull-surface")
	defer tx.Rollback()

	// Metadata auto-resolve must decline an issues conflict (not metadata-only).
	resolved, err := store.tryAutoResolveMetadataConflicts(ctx, tx)
	if err != nil {
		t.Fatalf("tryAutoResolveMetadataConflicts: %v", err)
	}
	if resolved {
		t.Skip("Dolt auto-merged the issues conflict — nothing left to surface")
	}

	// The new detection path must surface it as a typed error with the count.
	remainErr, derr := store.detectRemainingConflicts(ctx, tx)
	if derr != nil {
		t.Fatalf("detectRemainingConflicts: %v", derr)
	}
	if remainErr == nil {
		t.Skip("no conflict materialized to surface")
	}

	var cre *ConflictsRemainError
	if !errors.As(error(remainErr), &cre) {
		t.Fatalf("expected *ConflictsRemainError, got %T: %v", remainErr, remainErr)
	}
	if cre.Counts["issues"] < 1 {
		t.Errorf("expected issues conflict count >= 1, got %+v", cre.Counts)
	}
}

// TestConflictsResolveAndCommit verifies that resolving an issues-table conflict
// with "theirs" and then committing (what `bd dolt conflicts resolve --theirs
// issues` does) clears the conflict from the working set — i.e. ResolveConflicts
// stages-and-commits cleanly via store.Commit (GH#2455 stages dirty tables), so
// GetConflicts is empty afterwards and the store is no longer wedged.
func TestConflictsResolveAndCommit(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	tx := seedIssuesConflict(t, store, ctx, "resolve-commit")

	// Confirm a conflict materialized, then PERSIST the conflicted working set
	// (allow-commit-conflicts is set on this tx) and release the single pooled
	// connection so the store methods below can acquire it.
	conflicts, err := versioncontrolops.GetConflicts(ctx, tx)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("GetConflicts(tx): %v", err)
	}
	if len(conflicts) == 0 {
		_ = tx.Rollback()
		t.Skip("Dolt auto-merged the issues conflict — nothing to resolve")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit merge-with-conflicts: %v", err)
	}

	// Resolve + commit through the store API (the resolve command's core).
	if err := store.ResolveConflicts(ctx, "issues", "theirs"); err != nil {
		t.Fatalf("ResolveConflicts: %v", err)
	}
	if err := store.Commit(ctx, "resolve issues conflicts (theirs)"); err != nil {
		t.Fatalf("Commit after resolve: %v", err)
	}

	remaining, err := store.GetConflicts(ctx)
	if err != nil {
		t.Fatalf("GetConflicts after resolve: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("expected no conflicts after resolve+commit, got %+v", remaining)
	}
}
