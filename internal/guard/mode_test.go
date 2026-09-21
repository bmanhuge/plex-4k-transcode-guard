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

	// Invalid content never escalates: an enforce baseline drops to dry-run,
	// a dry-run baseline stays dry-run, and a warning explains why.
	if err := os.WriteFile(path, []byte("kill everything"), 0o644); err != nil {
		t.Fatal(err)
	}
	if m, src, warn := ResolveMode(false, path); m != ModeDryRun || src != "fallback" || !strings.Contains(warn, "does not contain") {
		t.Fatalf("invalid with enforce baseline: got %v %q %q", m, src, warn)
	}
	if m, src, warn := ResolveMode(true, path); m != ModeDryRun || src != "fallback" || !strings.Contains(warn, "does not contain") {
		t.Fatalf("invalid with dry-run baseline: got %v %q %q", m, src, warn)
	}
	// An off baseline cannot exist from the environment, but the rule is
	// generic: the less permissive of baseline and dry-run wins.
	if got := min(ModeOff, ModeDryRun); got != ModeOff {
		t.Fatalf("mode ordering broken: %v", got)
	}
}

func TestModeString(t *testing.T) {
	if ModeEnforce.String() != "enforce" || ModeDryRun.String() != "dry-run" || ModeOff.String() != "off" {
		t.Fatal("unexpected mode names")
	}
	if m, ok := ParseMode("nonsense"); ok || m == ModeEnforce {
		t.Fatal("nonsense must not parse, and must never yield enforce")
	}
	var zero Mode
	if zero != ModeOff {
		t.Fatal("the zero value of Mode must be off")
	}
}
