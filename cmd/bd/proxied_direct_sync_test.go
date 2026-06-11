package main

import (
	"testing"

	"github.com/steveyegge/beads/internal/configfile"
)

func TestDirectSyncDoltConfig(t *testing.T) {
	t.Run("external endpoint -> direct ServerMode config", func(t *testing.T) {
		t.Setenv("BEADS_PROXIED_SERVER_EXTERNAL_PASSWORD", "")
		cfg := directSyncDoltConfig("/scope/.beads", "vg", &configfile.ExternalDoltConfig{
			Host: "127.0.0.1",
			Port: 42188,
			User: "root",
		})
		if cfg == nil {
			t.Fatal("config = nil, want direct ServerMode config")
		}
		if !cfg.ServerMode {
			t.Fatal("ServerMode = false, want true")
		}
		if cfg.ServerHost != "127.0.0.1" || cfg.ServerPort != 42188 || cfg.ServerUser != "root" {
			t.Fatalf("endpoint = %s:%d user=%s, want 127.0.0.1:42188 root", cfg.ServerHost, cfg.ServerPort, cfg.ServerUser)
		}
		if cfg.Database != "vg" {
			t.Fatalf("Database = %q, want vg", cfg.Database)
		}
		if cfg.BeadsDir != "/scope/.beads" || cfg.Path != "/scope/.beads" {
			t.Fatalf("dirs = %q/%q, want /scope/.beads", cfg.BeadsDir, cfg.Path)
		}
		// gc owns the managed server: raw sync must never auto-start a dolt.
		if cfg.AutoStart {
			t.Fatal("AutoStart = true, want false")
		}
		// This IS the canonical endpoint — the portfile-clobber suppression
		// (RoutedThroughProxy) must NOT be set, unlike the routed store.
		if cfg.RoutedThroughProxy {
			t.Fatal("RoutedThroughProxy = true, want false (canonical endpoint)")
		}
	})

	t.Run("empty host defaults to loopback", func(t *testing.T) {
		cfg := directSyncDoltConfig("/scope/.beads", "db", &configfile.ExternalDoltConfig{Port: 40519})
		if cfg == nil || cfg.ServerHost != "127.0.0.1" {
			t.Fatalf("cfg = %+v, want host defaulted to 127.0.0.1", cfg)
		}
	})

	t.Run("external password env is forwarded", func(t *testing.T) {
		t.Setenv("BEADS_PROXIED_SERVER_EXTERNAL_PASSWORD", "sekrit")
		cfg := directSyncDoltConfig("/scope/.beads", "db", &configfile.ExternalDoltConfig{Port: 42188})
		if cfg == nil || cfg.ServerPassword != "sekrit" {
			t.Fatal("ServerPassword not forwarded from BEADS_PROXIED_SERVER_EXTERNAL_PASSWORD")
		}
	})

	t.Run("nil external -> nil (caller falls back to the loud guard)", func(t *testing.T) {
		if cfg := directSyncDoltConfig("/scope/.beads", "db", nil); cfg != nil {
			t.Fatalf("cfg = %+v, want nil", cfg)
		}
	})

	t.Run("missing port -> nil (caller falls back to the loud guard)", func(t *testing.T) {
		if cfg := directSyncDoltConfig("/scope/.beads", "db", &configfile.ExternalDoltConfig{Host: "127.0.0.1"}); cfg != nil {
			t.Fatalf("cfg = %+v, want nil", cfg)
		}
	})
}
