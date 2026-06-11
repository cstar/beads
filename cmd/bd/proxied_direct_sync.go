package main

import (
	"context"
	"fmt"
	"os"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/dolt"
)

// Raw-Dolt remote sync (dolt push / dolt pull) in proxied-server mode.
//
// The S5 guard (proxied_unsupported.go) exists because the multiplexed proxy
// cannot service CALL DOLT_PUSH / DOLT_PULL safely, and silently dead-dropping
// them (empty-JSON exit 0) stranded syncs. But a gc-managed proxied scope
// records the REAL dolt endpoint — the same server the proxy itself fronts —
// in proxied_server_client_info.json. For these capabilities we can do what
// direct ServerMode would do: open a direct connection to that endpoint for
// the duration of the command. The guard remains the loud fallback whenever
// no external endpoint is recorded, preserving the dead-drop contract.

// directSyncDoltConfig builds the direct ServerMode config for raw-Dolt sync
// from a proxied scope's recorded external endpoint. Returns nil when the
// endpoint is unusable (nil external or missing port) — callers must then
// fall back to the unsupported-capability guard. Pure function for tests.
func directSyncDoltConfig(beadsDir, database string, ext *configfile.ExternalDoltConfig) *dolt.Config {
	if ext == nil || ext.Port == 0 {
		return nil
	}
	host := ext.Host
	if host == "" {
		host = "127.0.0.1"
	}
	cfg := &dolt.Config{
		Path:       beadsDir,
		BeadsDir:   beadsDir,
		Database:   database,
		ServerMode: true,
		ServerHost: host,
		ServerPort: ext.Port,
		ServerUser: ext.ResolvedUser(),
		// AutoStart stays false (zero value): gc owns the managed server
		// lifecycle; raw sync must never spawn a competing dolt.
		//
		// RoutedThroughProxy stays false: unlike the routed store (which
		// listens on the proxy's ephemeral port), this endpoint IS the
		// canonical managed server — persisting its port is identical to
		// what gc publishes, so the portfile-clobber suppression does not
		// apply.
	}
	if pw := os.Getenv("BEADS_PROXIED_SERVER_EXTERNAL_PASSWORD"); pw != "" {
		cfg.ServerPassword = pw
	}
	return cfg
}

// storeForRawDoltSync returns the store a raw-Dolt remote-sync command should
// operate on. Outside proxied-server mode it is the normal store (getStore),
// byte-for-byte the existing behavior. In proxied-server mode it opens a
// direct ServerMode store at the scope's recorded external endpoint; when no
// usable endpoint is recorded it exits with the typed unsupported error,
// exactly like the previous unconditional guard (never silent).
func storeForRawDoltSync(ctx context.Context, c Capability) storage.DoltStorage {
	if !proxiedServerMode {
		return getStore()
	}
	dir := resolveBeadsDirForDBPath(getDBPath())
	if dir == "" {
		guardUnsupportedInProxiedMode(c)
		return nil // unreachable: guard exits in proxied mode
	}
	info, err := configfile.LoadProxiedServerClientInfo(dir)
	if err != nil || info == nil {
		guardUnsupportedInProxiedMode(c)
		return nil // unreachable: guard exits in proxied mode
	}
	database := configfile.DefaultDoltDatabase
	if persisted, _ := configfile.Load(dir); persisted != nil {
		database = persisted.GetDoltDatabase()
	}
	cfg := directSyncDoltConfig(dir, database, info.External)
	if cfg == nil {
		guardUnsupportedInProxiedMode(c)
		return nil // unreachable: guard exits in proxied mode
	}
	st, err := dolt.New(ctx, cfg)
	if err != nil {
		FatalErrorRespectJSON("%s: opening direct dolt endpoint %s:%d: %v", c, cfg.ServerHost, cfg.ServerPort, err)
	}
	fmt.Fprintf(os.Stderr, "proxied scope: using direct dolt endpoint %s:%d for %s\n", cfg.ServerHost, cfg.ServerPort, c)
	return st
}
