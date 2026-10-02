package beads_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/backends"
)

func TestOpenReadyReaderUsesRegisteredReadOnlyFactory(t *testing.T) {
	const name = "sdk-readonly-fixture"
	writable, readonly := 0, 0
	sentinel := errors.New("readonly factory reached")
	backends.Register(name, backends.Backend{
		Open: func(context.Context, string) (storage.DoltStorage, error) {
			writable++
			return nil, errors.New("writable factory reached")
		},
		OpenReadOnly: func(context.Context, string) (storage.DoltStorage, error) { readonly++; return nil, sentinel },
	})
	t.Cleanup(func() { backends.Deregister(name) })
	dir := writeBackendMetadata(t, name)
	s, err := beads.OpenReadyReader(context.Background(), dir)
	if s != nil {
		_ = s.Close()
		t.Fatal("unexpected store")
	}
	if !errors.Is(err, sentinel) || readonly != 1 || writable != 0 {
		t.Fatalf("readonly=%d writable=%d err=%v; SDK must select the non-mutating factory", readonly, writable, err)
	}
}

func TestOpenReadyReaderDoesNotProvisionMissingStorage(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".beads")
	s, err := beads.OpenReadyReader(context.Background(), dir)
	if s != nil {
		_ = s.Close()
		t.Fatal("missing storage returned a store")
	}
	if err == nil {
		t.Fatal("missing storage must fail")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("readonly open provisioned %d entries", len(entries))
	}
}

func TestOpenReadyReaderRejectsUnsupportedBackend(t *testing.T) {
	for _, name := range []string{"sqlite", "mysql", "postgres", "unknown-readonly-backend"} {
		t.Run(name, func(t *testing.T) {
			dir := writeBackendMetadata(t, name)
			s, err := beads.OpenReadyReader(context.Background(), dir)
			if s != nil {
				_ = s.Close()
				t.Fatal("unsupported backend returned a store")
			}
			if err == nil {
				t.Fatal("unsupported backend must fail")
			}
			for _, path := range []string{"embeddeddolt", "dolt", "beads.db"} {
				if _, statErr := os.Stat(filepath.Join(dir, path)); !os.IsNotExist(statErr) {
					t.Fatalf("readonly open provisioned %s: %v", path, statErr)
				}
			}
		})
	}
}

func TestOpenReadyReaderRejectsCorruptMetadata(t *testing.T) {
	dir := writeBackendMetadata(t, "dolt")
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte("{invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := beads.OpenReadyReader(context.Background(), dir)
	if s != nil {
		_ = s.Close()
		t.Fatal("corrupt metadata returned a store")
	}
	if err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("error=%v; corrupt metadata must fail before backend open", err)
	}
}

func TestOpenReadyReaderServerDoesNotMutate(t *testing.T) {
	skipIfNoDoltServer(t)
	dir := writeBackendMetadata(t, "dolt")
	database := fmt.Sprintf("sdk_readonly_%x", time.Now().UnixNano())
	metadata := fmt.Sprintf(`{"backend":"dolt","dolt_mode":"server","dolt_database":%q,"dolt_server_host":"127.0.0.1","dolt_server_port":%d}`, database, testServerPort)
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(metadata), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	writer, err := beads.OpenFromConfig(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	if err := writer.SetConfig(ctx, "issue_prefix", "sdkro"); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	issue := &beads.Issue{ID: "sdkro-probe", Title: "expired defer", Status: beads.StatusDeferred, IssueType: beads.TypeTask, Priority: 2, DeferUntil: &past}
	if err := writer.CreateIssue(ctx, issue, "test"); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%d)/%s", testServerPort, database))
	if err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	type snapshot struct {
		status              string
		deferUntil, updated sql.NullString
		events              int
		prefix              string
	}
	inspect := func() snapshot {
		t.Helper()
		var state snapshot
		if err := db.QueryRowContext(ctx, "SELECT status,defer_until,updated_at FROM issues WHERE id='sdkro-probe'").Scan(&state.status, &state.deferUntil, &state.updated); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM events WHERE issue_id='sdkro-probe'").Scan(&state.events); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(ctx, "SELECT value FROM config WHERE `key`='issue_prefix'").Scan(&state.prefix); err != nil {
			t.Fatal(err)
		}
		return state
	}
	before := inspect()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := beads.OpenReadyReader(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, ok := any(reader).(storage.Storage); ok {
		t.Fatal("ready reader exposes writable storage")
	}
	if _, err := reader.GetReadyWork(ctx, beads.WorkFilter{}); err != nil {
		t.Fatal(err)
	}
	if after := inspect(); before != after {
		t.Fatalf("readonly readiness mutated state: before=%+v after=%+v", before, after)
	}
	var version int
	if err := db.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO schema_migrations (version) VALUES (?)", version+1); err != nil {
		t.Fatal(err)
	}
	rejected, err := beads.OpenReadyReader(ctx, dir)
	if rejected != nil {
		_ = rejected.Close()
		t.Fatal("future schema returned reader")
	}
	if err == nil {
		t.Fatal("future schema accepted")
	}
	var afterVersion int
	if err := db.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&afterVersion); err != nil {
		t.Fatal(err)
	}
	if afterVersion != version+1 {
		t.Fatalf("readonly migrated future schema: %d", afterVersion)
	}
}

// A read consumer must not receive write capabilities through a type assertion,
// even when a backend's readonly open still returns its full concrete store.
func TestOpenReadyReaderDoesNotExposeWriteCapabilities(t *testing.T) {
	const name = "sdk-readonly-capability-fixture"
	fixture := &readOnlyCapabilityFixture{}
	backends.Register(name, backends.Backend{
		Open: func(context.Context, string) (storage.DoltStorage, error) {
			t.Fatal("writable factory reached")
			return nil, nil
		},
		OpenReadOnly: func(context.Context, string) (storage.DoltStorage, error) { return fixture, nil },
	})
	t.Cleanup(func() { backends.Deregister(name) })
	reader, err := beads.OpenReadyReader(t.Context(), writeBackendMetadata(t, name))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := any(reader).(interface {
		SetConfig(context.Context, string, string) error
	}); ok {
		t.Error("readonly API exposes SetConfig")
	}
	if _, ok := any(reader).(storage.Storage); ok {
		t.Error("readonly API exposes complete writable Storage")
	}
	issues, err := reader.GetReadyWork(t.Context(), beads.WorkFilter{IncludeEphemeral: true})
	if err != nil || len(issues) != 1 || issues[0].ID != "sdk-ro" || !fixture.filter.IncludeEphemeral {
		t.Fatalf("read forwarding: issues=%v err=%v filter=%+v", issues, err, fixture.filter)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if fixture.closes != 1 {
		t.Fatalf("closes=%d", fixture.closes)
	}
}

type readOnlyCapabilityFixture struct {
	storage.DoltStorage
	filter beads.WorkFilter
	closes int
}

func (f *readOnlyCapabilityFixture) GetReadyWork(_ context.Context, filter beads.WorkFilter) ([]*beads.Issue, error) {
	f.filter = filter
	return []*beads.Issue{{ID: "sdk-ro"}}, nil
}
func (f *readOnlyCapabilityFixture) Close() error { f.closes++; return nil }
