package beads

import (
	"context"
	"fmt"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storage/backends"
	"github.com/steveyegge/beads/internal/storage/dolt"
)

// ReadyReader exposes readiness queries without any writable storage capability.
// Close releases the underlying storage handle.
type ReadyReader interface {
	GetReadyWork(context.Context, WorkFilter) ([]*Issue, error)
	Close() error
}

// OpenReadyReader opens existing storage using the configured backend's
// non-mutating open path. It does not provision storage, migrate schemas or
// implicitly start a Dolt server. Embedded storage requires CGO.
//
// The returned reader exposes raw backend readiness and filtering semantics,
// without write methods or an accessor to the underlying storage. Close it
// before changing the workspace's backend or connection configuration.
func OpenReadyReader(ctx context.Context, beadsDir string) (ReadyReader, error) {
	cfg, err := loadReadOnlyConfig(beadsDir)
	if err != nil {
		return nil, err
	}
	if !configfile.IsSupportedBackend(cfg.Backend) {
		return nil, configuredBackendUnavailable(cfg.Backend)
	}
	if backend, ok := backends.Lookup(cfg.GetBackend()); ok {
		store, err := backend.OpenReadOnly(ctx, beadsDir)
		if err != nil {
			return nil, err
		}
		return &readyReader{store: store}, nil
	}
	if resolveOpenBackend(cfg) == openBackendServer {
		store, err := dolt.NewFromConfigWithOptions(ctx, beadsDir, readOnlyServerOptions(cfg))
		if err != nil {
			return nil, err
		}
		return &readyReader{store: store}, nil
	}
	return openEmbeddedReadyReader(ctx, beadsDir, cfg.GetDoltDatabase())
}

func loadReadOnlyConfig(beadsDir string) (*configfile.Config, error) {
	cfg, err := configfile.Load(beadsDir)
	if err != nil {
		return nil, fmt.Errorf("loading storage metadata: %w", err)
	}
	if cfg == nil {
		cfg = configfile.DefaultConfig()
	}
	dolt.ApplyCentralConfigDefaults(cfg)
	return cfg, nil
}

func readOnlyServerOptions(cfg *configfile.Config) *dolt.Config {
	return &dolt.Config{ReadOnly: true, DisableAutoStart: true, ProxiedServer: cfg.IsDoltProxiedServerMode()}
}

// A named private field deliberately prevents promotion of write methods and
// role accessors from the concrete store through a runtime type assertion.
type readyReader struct{ store ReadyReader }

func (r *readyReader) GetReadyWork(ctx context.Context, filter WorkFilter) ([]*Issue, error) {
	return r.store.GetReadyWork(ctx, filter)
}
func (r *readyReader) Close() error { return r.store.Close() }
