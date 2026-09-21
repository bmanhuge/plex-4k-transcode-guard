package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveModeFromEnv(t *testing.T) {
	if m, src, warn := ResolveMode(false, ""); m != ModeEnforce || src != "env" || warn != "" {
		t.Fatalf("got %v %q %q", m, src, warn)
	}
	if m, src, warn := ResolveMode(true, ""); m != ModeDryRun || src != "env" || warn != "" {
		t.Fatalf("got %v %q %q", m, src, warn)
	}
}

func TestResolveModeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mode")

	// Missing file: environment applies.
	if m, src, warn := ResolveMode(true, path); m != ModeDryRun || src != "env" || warn != "" {
		t.Fatalf("missing file: got %v %q %q", m, src, warn)
	}
	cases := []struct {
		content string
		env     bool
		want    Mode
	}{
		{"enforce\n", true, ModeEnforce},
		{"  DRY-RUN  ", false, ModeDryRun},
		{"off", false, ModeOff},
		{"paused", true, ModeOff},
	}
	for _, tc := range cases {
		if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
			t.Fatal(err)
		}
		m, src, warn := ResolveMode(tc.env, path)
		if m != tc.want || src != "file" || warn != "" {
			t.Fatalf("%q: got %v %q %q", tc.content, m, src, warn)
		}
	}

	// Invalid content: environment applies and a warning explains why.
	if err := os.WriteFile(path, []byte("kill everything"), 0o644); err != nil {
		t.Fatal(err)
	}
	if m, src, warn := ResolveMode(true, path); m != ModeDryRun || src != "env" || !strings.Contains(warn, "does not contain") {
		t.Fatalf("invalid: got %v %q %q", m, src, warn)
	}
}

func TestModeString(t *testing.T) {
	if ModeEnforce.String() != "enforce" || ModeDryRun.String() != "dry-run" || ModeOff.String() != "off" {
		t.Fatal("unexpected mode names")
	}
	if _, ok := ParseMode("nonsense"); ok {
		t.Fatal("nonsense must not parse")
	}
}
