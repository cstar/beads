package issueops

import (
	"context"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestReadyWorkBriefProjection asserts that the work-probe projection builder,
// driven with brief=true, emits a SELECT that omits the 7 heavy free-text/blob
// body columns (the measured 7–12x cost driver on the high-frequency poll path)
// while still projecting the columns the probe consumes — crucially i.metadata
// (carries gc.routed_to for pool routing) and i.title (be-yvci).
func TestReadyWorkBriefProjection(t *testing.T) {
	t.Parallel()

	var captured string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(
		func(_ string, actual string) error {
			captured = actual
			return nil // capture-and-match-all; assertions run on `captured`
		},
	)))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Empty result set → the scan loop never runs, so we only exercise the
	// projection the builder emits.
	mock.ExpectQuery("").WillReturnRows(sqlmock.NewRows([]string{"id"}))

	if _, err := runSearchQueryInTx(
		context.Background(), tx, IssuesFilterTables,
		"", "", "", nil,
		false /*includeWispReverseDeps*/, false /*skipLabels*/, true, /*brief*/
	); err != nil {
		t.Fatalf("runSearchQueryInTx(brief): %v", err)
	}

	for _, dropped := range bodyColumnsOmittedInBrief {
		if strings.Contains(captured, "i."+dropped) {
			t.Errorf("brief work-probe SELECT must not project i.%s\nSQL:\n%s", dropped, captured)
		}
	}
	for _, kept := range []string{"i.metadata", "i.title", "i.id", "i.status"} {
		if !strings.Contains(captured, kept) {
			t.Errorf("brief work-probe SELECT must project %s\nSQL:\n%s", kept, captured)
		}
	}
}
