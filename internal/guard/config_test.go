package guard

import (
	"strings"
	"testing"
	"time"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig(envMap(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != DefaultConfig() {
		t.Fatalf("defaults mismatch: %+v", cfg)
	}
	if cfg.DryRun {
		t.Fatal("dry-run must default to false")
	}
	if cfg.PollInterval != 10*time.Second || cfg.HTTPTimeout != 5*time.Second || cfg.Cooldown != 60*time.Second {
		t.Fatalf("unexpected timing defaults: %+v", cfg)
	}
	if cfg.ModeFile != "" {
		t.Fatal("mode file override must be disabled by default")
	}
}

func TestLoadConfigValid(t *testing.T) {
	cfg, err := LoadConfig(envMap(map[string]string{
		EnvDryRun:          "TRUE",
		EnvPollInterval:    "15",
		EnvHTTPTimeout:     "2s",
		EnvCooldown:        "1m30s",
		EnvPlexURL:         "http://127.0.0.1:32400/",
		EnvPreferencesFile: "/config/Prefs.xml",
		EnvMessageFile:     "/config/msg.txt",
		EnvModeFile:        "/config/4k-guard-mode",
		EnvLogLevel:        "DEBUG",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Config{
		DryRun: true, PollInterval: 15 * time.Second, HTTPTimeout: 2 * time.Second, Cooldown: 90 * time.Second,
		PlexURL: "http://127.0.0.1:32400", PreferencesFile: "/config/Prefs.xml", MessageFile: "/config/msg.txt",
		ModeFile: "/config/4k-guard-mode", LogLevel: "debug",
	}
	if cfg != want {
		t.Fatalf("got %+v\nwant %+v", cfg, want)
	}
}

func TestLoadConfigBooleans(t *testing.T) {
	for _, raw := range []string{"1", "yes", "on", "True"} {
		cfg, err := LoadConfig(envMap(map[string]string{EnvDryRun: raw}))
		if err != nil || !cfg.DryRun {
			t.Fatalf("%q: want dry-run true, got %v err=%v", raw, cfg.DryRun, err)
		}
	}
	for _, raw := range []string{"0", "no", "off", "False"} {
		cfg, err := LoadConfig(envMap(map[string]string{EnvDryRun: raw}))
		if err != nil || cfg.DryRun {
			t.Fatalf("%q: want dry-run false, got %v err=%v", raw, cfg.DryRun, err)
		}
	}
}

func TestLoadConfigInvalid(t *testing.T) {
	cases := []struct {
		name, key, value string
	}{
		{"dry-run word", EnvDryRun, "maybe"},
		{"poll below min", EnvPollInterval, "1s"},
		{"poll above max", EnvPollInterval, "10m"},
		{"poll garbage", EnvPollInterval, "soon"},
		{"poll negative", EnvPollInterval, "-5"},
		{"timeout zero", EnvHTTPTimeout, "0"},
		{"timeout above max", EnvHTTPTimeout, "61s"},
		{"cooldown above max", EnvCooldown, "2h"},
		{"cooldown negative", EnvCooldown, "-1s"},
		{"url scheme", EnvPlexURL, "ftp://127.0.0.1"},
		{"url credentials", EnvPlexURL, "http://user:pw@127.0.0.1:32400"},
		{"url query", EnvPlexURL, "http://127.0.0.1:32400/?x=1"},
		{"url no scheme", EnvPlexURL, "127.0.0.1:32400"},
		{"prefs relative", EnvPreferencesFile, "Preferences.xml"},
		{"message relative", EnvMessageFile, "msg.txt"},
		{"mode relative", EnvModeFile, "mode"},
		{"log level", EnvLogLevel, "verbose"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(envMap(map[string]string{tc.key: tc.value}))
			if err == nil {
				t.Fatalf("expected error for %s=%q", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("error should name %s: %v", tc.key, err)
			}
		})
	}
}

func TestLoadConfigReportsAllErrors(t *testing.T) {
	_, err := LoadConfig(envMap(map[string]string{EnvDryRun: "x", EnvPollInterval: "0", EnvLogLevel: "loud"}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, key := range []string{EnvDryRun, EnvPollInterval, EnvLogLevel} {
		if !strings.Contains(err.Error(), key) {
			t.Fatalf("joined error should mention %s: %v", key, err)
		}
	}
}
