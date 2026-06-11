package db

import (
	"context"
	"fmt"
	"strings"
)

// blockedStateBatchSize bounds the IN-clause size for is_blocked recompute
// passes, mirroring issueops.queryBatchSize.
const blockedStateBatchSize = 200

// waitsForGateBlockedSQL is the shared waits-for gate predicate, evaluated
// against a dependency row aliased as d. Duplicated from
// internal/storage/issueops/blocked_state.go to keep the domain layer
// independent (see adaptive.go for the same convention).
const waitsForGateBlockedSQL = `
		(
		  EXISTS (
		    SELECT 1 FROM dependencies cd JOIN issues child ON child.id = cd.issue_id
		    WHERE cd.type = 'parent-child'
		      AND ((d.depends_on_issue_id IS NOT NULL AND cd.depends_on_issue_id = d.depends_on_issue_id)
		        OR (d.depends_on_wisp_id IS NOT NULL AND cd.depends_on_wisp_id = d.depends_on_wisp_id))
		      AND child.status <> 'closed' AND child.status <> 'pinned'
		  )
		  OR EXISTS (
		    SELECT 1 FROM wisp_dependencies cd JOIN wisps child ON child.id = cd.issue_id
		    WHERE cd.type = 'parent-child'
		      AND ((d.depends_on_issue_id IS NOT NULL AND cd.depends_on_issue_id = d.depends_on_issue_id)
		        OR (d.depends_on_wisp_id IS NOT NULL AND cd.depends_on_wisp_id = d.depends_on_wisp_id))
		      AND child.status <> 'closed' AND child.status <> 'pinned'
		  )
		)
		AND NOT (
		  JSON_UNQUOTE(JSON_EXTRACT(d.metadata, '$.gate')) = 'any-children'
		  AND (
		    EXISTS (
		      SELECT 1 FROM dependencies cd JOIN issues child ON child.id = cd.issue_id
		      WHERE cd.type = 'parent-child'
		        AND ((d.depends_on_issue_id IS NOT NULL AND cd.depends_on_issue_id = d.depends_on_issue_id)
		          OR (d.depends_on_wisp_id IS NOT NULL AND cd.depends_on_wisp_id = d.depends_on_wisp_id))
		        AND child.status = 'closed'
		    )
		    OR EXISTS (
		      SELECT 1 FROM wisp_dependencies cd JOIN wisps child ON child.id = cd.issue_id
		      WHERE cd.type = 'parent-child'
		        AND ((d.depends_on_issue_id IS NOT NULL AND cd.depends_on_issue_id = d.depends_on_issue_id)
		          OR (d.depends_on_wisp_id IS NOT NULL AND cd.depends_on_wisp_id = d.depends_on_wisp_id))
		        AND child.status = 'closed'
		    )
		  )
		)
`

// RecomputeIsBlocked re-derives the denormalized is_blocked flag for the given
// issue and wisp IDs, iterating to a fixpoint so chains within the id set
// (e.g. parent blocked -> child blocked) settle. It marks and unmarks, so it
// is safe for non-monotonic transitions. IDs that do not exist in the targeted
// table are ignored.
//
// Duplicated in spirit from issueops.RecomputeIsBlockedInTx; the domain layer
// stays independent of the legacy store packages by convention.
func (r *dependencySQLRepositoryImpl) RecomputeIsBlocked(ctx context.Context, issueIDs, wispIDs []string) error {
	if len(issueIDs) == 0 && len(wispIDs) == 0 {
		return nil
	}
	for {
		var changed int64

		n, err := runBlockedMarkUnmarkBatched(ctx, r.runner, markBlockedTemplateForIssues(), unmarkBlockedTemplateForIssues(), issueIDs)
		if err != nil {
			return err
		}
		changed += n

		n, err = runBlockedMarkUnmarkBatched(ctx, r.runner, markBlockedTemplateForWisps(), unmarkBlockedTemplateForWisps(), wispIDs)
		if err != nil {
			return err
		}
		changed += n

		if changed == 0 {
			return nil
		}
	}
}

func markBlockedTemplateForIssues() string {
	return fmt.Sprintf(`
		UPDATE issues i SET i.is_blocked = 1
		WHERE i.id IN (%%s)
		  AND i.is_blocked = 0
		  AND i.status <> 'closed' AND i.status <> 'pinned'
		  AND (
		    EXISTS (
		      SELECT 1 FROM dependencies d
		      JOIN issues t ON t.id = d.depends_on_issue_id
		      WHERE d.issue_id = i.id
		        AND (d.type = 'blocks' OR d.type = 'conditional-blocks')
		        AND t.status <> 'closed' AND t.status <> 'pinned'
		    )
		    OR EXISTS (
		      SELECT 1 FROM dependencies d
		      JOIN wisps t ON t.id = d.depends_on_wisp_id
		      WHERE d.issue_id = i.id
		        AND (d.type = 'blocks' OR d.type = 'conditional-blocks')
		        AND t.status <> 'closed' AND t.status <> 'pinned'
		    )
		    OR EXISTS (
		      SELECT 1 FROM dependencies d
		      JOIN issues p ON p.id = d.depends_on_issue_id
		      WHERE d.issue_id = i.id
		        AND d.type = 'parent-child'
		        AND p.is_blocked = 1
		    )
		    OR EXISTS (
		      SELECT 1 FROM dependencies d
		      JOIN wisps p ON p.id = d.depends_on_wisp_id
		      WHERE d.issue_id = i.id
		        AND d.type = 'parent-child'
		        AND p.is_blocked = 1
		    )
		    OR EXISTS (
		      SELECT 1 FROM dependencies d
		      WHERE d.issue_id = i.id AND d.type = 'waits-for'
		        AND (%s)
		    )
		  )
	`, waitsForGateBlockedSQL)
}

func unmarkBlockedTemplateForIssues() string {
	return fmt.Sprintf(`
		UPDATE issues i SET i.is_blocked = 0
		WHERE i.id IN (%%s)
		  AND i.is_blocked = 1
		  AND (
		    i.status = 'closed' OR i.status = 'pinned'
		    OR (
		      NOT EXISTS (
		        SELECT 1 FROM dependencies d
		        JOIN issues t ON t.id = d.depends_on_issue_id
		        WHERE d.issue_id = i.id
		          AND (d.type = 'blocks' OR d.type = 'conditional-blocks')
		          AND t.status <> 'closed' AND t.status <> 'pinned'
		      )
		      AND NOT EXISTS (
		        SELECT 1 FROM dependencies d
		        JOIN wisps t ON t.id = d.depends_on_wisp_id
		        WHERE d.issue_id = i.id
		          AND (d.type = 'blocks' OR d.type = 'conditional-blocks')
		          AND t.status <> 'closed' AND t.status <> 'pinned'
		      )
		      AND NOT EXISTS (
		        SELECT 1 FROM dependencies d
		        JOIN issues p ON p.id = d.depends_on_issue_id
		        WHERE d.issue_id = i.id
		          AND d.type = 'parent-child'
		          AND p.is_blocked = 1
		      )
		      AND NOT EXISTS (
		        SELECT 1 FROM dependencies d
		        JOIN wisps p ON p.id = d.depends_on_wisp_id
		        WHERE d.issue_id = i.id
		          AND d.type = 'parent-child'
		          AND p.is_blocked = 1
		      )
		      AND NOT EXISTS (
		        SELECT 1 FROM dependencies d
		        WHERE d.issue_id = i.id AND d.type = 'waits-for'
		          AND (%s)
		      )
		    )
		  )
	`, waitsForGateBlockedSQL)
}

func markBlockedTemplateForWisps() string {
	return fmt.Sprintf(`
		UPDATE wisps w SET w.is_blocked = 1
		WHERE w.id IN (%%s)
		  AND w.is_blocked = 0
		  AND w.status <> 'closed' AND w.status <> 'pinned'
		  AND (
		    EXISTS (
		      SELECT 1 FROM wisp_dependencies d
		      JOIN issues t ON t.id = d.depends_on_issue_id
		      WHERE d.issue_id = w.id
		        AND (d.type = 'blocks' OR d.type = 'conditional-blocks')
		        AND t.status <> 'closed' AND t.status <> 'pinned'
		    )
		    OR EXISTS (
		      SELECT 1 FROM wisp_dependencies d
		      JOIN wisps t ON t.id = d.depends_on_wisp_id
		      WHERE d.issue_id = w.id
		        AND (d.type = 'blocks' OR d.type = 'conditional-blocks')
		        AND t.status <> 'closed' AND t.status <> 'pinned'
		    )
		    OR EXISTS (
		      SELECT 1 FROM wisp_dependencies d
		      JOIN issues p ON p.id = d.depends_on_issue_id
		      WHERE d.issue_id = w.id
		        AND d.type = 'parent-child'
		        AND p.is_blocked = 1
		    )
		    OR EXISTS (
		      SELECT 1 FROM wisp_dependencies d
		      JOIN wisps p ON p.id = d.depends_on_wisp_id
		      WHERE d.issue_id = w.id
		        AND d.type = 'parent-child'
		        AND p.is_blocked = 1
		    )
		    OR EXISTS (
		      SELECT 1 FROM wisp_dependencies d
		      WHERE d.issue_id = w.id AND d.type = 'waits-for'
		        AND (%s)
		    )
		  )
	`, waitsForGateBlockedSQL)
}

func unmarkBlockedTemplateForWisps() string {
	return fmt.Sprintf(`
		UPDATE wisps w SET w.is_blocked = 0
		WHERE w.id IN (%%s)
		  AND w.is_blocked = 1
		  AND (
		    w.status = 'closed' OR w.status = 'pinned'
		    OR (
		      NOT EXISTS (
		        SELECT 1 FROM wisp_dependencies d
		        JOIN issues t ON t.id = d.depends_on_issue_id
		        WHERE d.issue_id = w.id
		          AND (d.type = 'blocks' OR d.type = 'conditional-blocks')
		          AND t.status <> 'closed' AND t.status <> 'pinned'
		      )
		      AND NOT EXISTS (
		        SELECT 1 FROM wisp_dependencies d
		        JOIN wisps t ON t.id = d.depends_on_wisp_id
		        WHERE d.issue_id = w.id
		          AND (d.type = 'blocks' OR d.type = 'conditional-blocks')
		          AND t.status <> 'closed' AND t.status <> 'pinned'
		      )
		      AND NOT EXISTS (
		        SELECT 1 FROM wisp_dependencies d
		        JOIN issues p ON p.id = d.depends_on_issue_id
		        WHERE d.issue_id = w.id
		          AND d.type = 'parent-child'
		          AND p.is_blocked = 1
		      )
		      AND NOT EXISTS (
		        SELECT 1 FROM wisp_dependencies d
		        JOIN wisps p ON p.id = d.depends_on_wisp_id
		        WHERE d.issue_id = w.id
		          AND d.type = 'parent-child'
		          AND p.is_blocked = 1
		      )
		      AND NOT EXISTS (
		        SELECT 1 FROM wisp_dependencies d
		        WHERE d.issue_id = w.id AND d.type = 'waits-for'
		          AND (%s)
		      )
		    )
		  )
	`, waitsForGateBlockedSQL)
}

//nolint:gosec // G201: callers pass constant templates; only IN-clause placeholders are formatted in.
func runBlockedMarkUnmarkBatched(ctx context.Context, r Runner, markTmpl, unmarkTmpl string, ids []string) (int64, error) {
	var changed int64
	for start := 0; start < len(ids); start += blockedStateBatchSize {
		end := start + blockedStateBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		placeholders, args := blockedStateInClause(ids[start:end])

		res, err := r.ExecContext(ctx, fmt.Sprintf(markTmpl, placeholders), args...)
		if err != nil {
			return changed, fmt.Errorf("recompute is_blocked (mark): %w", err)
		}
		n, _ := res.RowsAffected()
		changed += n

		res, err = r.ExecContext(ctx, fmt.Sprintf(unmarkTmpl, placeholders), args...)
		if err != nil {
			return changed, fmt.Errorf("recompute is_blocked (unmark): %w", err)
		}
		n, _ = res.RowsAffected()
		changed += n
	}
	return changed, nil
}

func blockedStateInClause(ids []string) (string, []any) {
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	return strings.Join(placeholders, ", "), args
}
