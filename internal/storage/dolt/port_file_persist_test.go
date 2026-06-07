package dolt

import (
	"os"
	"testing"
)

// TestShouldPersistPortFileForConfig guards the proxied-server port-file clobber
// fix. A proxied routed store (RoutedThroughProxy) connects to an ephemeral
// db-proxy listener; its port must never be persisted into
// .beads/dolt-server.port, which is the canonical-server discovery hint. Before
// the fix, newServerMode wrote the proxy's listener port there, so the file went
// stale the moment the proxy respawned on a new port and any reader that trusted
// it targeted a dead endpoint.
func TestShouldPersistPortFileForConfig(t *testing.T) {
	// The decision keys off BEADS_DOLT_SERVER_PORT / BEADS_DOLT_PORT; clear both
	// so the env-override branch does not mask the RoutedThroughProxy logic.
	for _, k := range []string{"BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_PORT"} {
		if orig, ok := os.LookupEnv(k); ok {
			t.Cleanup(func() { _ = os.Setenv(k, orig) })
			_ = os.Unsetenv(k)
		}
	}

	tests := []struct {
		name string
		cfg  *Config
		want bool
	}{
		{
			name: "canonical local server persists",
			cfg:  &Config{ServerHost: "127.0.0.1", ServerPort: 48770},
			want: true,
		},
		{
			name: "proxied routed store does not persist",
			cfg:  &Config{ServerHost: "127.0.0.1", ServerPort: 50534, RoutedThroughProxy: true},
			want: false,
		},
		{
			name: "remote host never persists",
			cfg:  &Config{ServerHost: "db.example.com", ServerPort: 3306},
			want: false,
		},
		{
			name: "nil config is safe",
			cfg:  nil,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldPersistPortFileForConfig(tt.cfg); got != tt.want {
				t.Fatalf("shouldPersistPortFileForConfig() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestShouldPersistPortFileForConfig_EnvOverrideSuppresses confirms an explicit
// port override still suppresses the persist even for a canonical local server,
// preserving the pre-existing shouldPersistResolvedPortFile contract.
func TestShouldPersistPortFileForConfig_EnvOverrideSuppresses(t *testing.T) {
	if orig, ok := os.LookupEnv("BEADS_DOLT_PORT"); ok {
		t.Cleanup(func() { _ = os.Setenv("BEADS_DOLT_PORT", orig) })
	} else {
		t.Cleanup(func() { _ = os.Unsetenv("BEADS_DOLT_PORT") })
	}
	_ = os.Setenv("BEADS_DOLT_PORT", "48770")

	cfg := &Config{ServerHost: "127.0.0.1", ServerPort: 48770}
	if shouldPersistPortFileForConfig(cfg) {
		t.Fatal("expected env port override to suppress port-file persist")
	}
}
