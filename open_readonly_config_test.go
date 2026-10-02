package beads

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/beads/internal/configfile"
)

func TestReadOnlyConfigCentralModeBeforeRouting(t *testing.T) {
	for _, mode := range []string{"server", "proxied-server"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, ".beads")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(`{"backend":"dolt","dolt_database":"project"}`), 0600); err != nil {
				t.Fatal(err)
			}
			central := filepath.Join(root, "server.json")
			if err := os.WriteFile(central, []byte(`{"dolt_mode":"`+mode+`","dolt_server_host":"central.example","dolt_database":"must_not_inherit"}`), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("BEADS_CENTRAL_CONFIG", central)
			cfg, err := loadReadOnlyConfig(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := resolveOpenBackend(cfg); got != openBackendServer {
				t.Fatalf("central %s routed to backend %v", mode, got)
			}
			if cfg.DoltServerHost != "central.example" || cfg.DoltDatabase != "project" {
				t.Fatalf("incorrect central merge: %+v", cfg)
			}
		})
	}
}

func TestReadOnlyServerOptionsPreserveProxyMode(t *testing.T) {
	for _, mode := range []string{"server", "proxied-server"} {
		cfg := &configfile.Config{Backend: "dolt", DoltMode: mode}
		options := readOnlyServerOptions(cfg)
		if !options.ReadOnly || !options.DisableAutoStart || options.CreateIfMissing {
			t.Fatalf("unsafe readonly options: %+v", options)
		}
		if options.ProxiedServer != (mode == "proxied-server") {
			t.Errorf("mode %s lost proxy provenance", mode)
		}
	}
}
