package guard

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fixtureToken = "fixtureTOKENvalue1234567"

func TestParsePreferencesTokenFixture(t *testing.T) {
	f, err := os.Open("testdata/preferences.xml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tok, err := ParsePreferencesToken(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != fixtureToken {
		t.Fatalf("got %q", tok)
	}
}

func TestParsePreferencesTokenUnclaimed(t *testing.T) {
	f, err := os.Open("testdata/preferences_unclaimed.xml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, err = ParsePreferencesToken(f)
	if !errors.Is(err, ErrNoToken) {
		t.Fatalf("want ErrNoToken, got %v", err)
	}
}

func TestParsePreferencesTokenErrorsNeverContainToken(t *testing.T) {
	cases := map[string]string{
		"truncated":   `<?xml version="1.0"?><Preferences PlexOnlineToken="` + fixtureToken + `"`,
		"blank":       `<Preferences PlexOnlineToken="   "/>`,
		"wrong root":  `<Other PlexOnlineToken="` + fixtureToken + `"/>`,
		"bad format":  `<Preferences PlexOnlineToken="` + fixtureToken + ` with space"/>`,
		"empty input": ``,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			tok, err := ParsePreferencesToken(strings.NewReader(doc))
			if err == nil {
				t.Fatalf("expected error, got token %q", tok)
			}
			if tok != "" {
				t.Fatalf("token must be empty on error, got %q", tok)
			}
			if strings.Contains(err.Error(), fixtureToken) {
				t.Fatalf("error leaks token: %v", err)
			}
		})
	}
}

func writePrefs(t *testing.T, path, token string) {
	t.Helper()
	doc := `<?xml version="1.0" encoding="utf-8"?>` + "\n" + `<Preferences MachineIdentifier="m" PlexOnlineToken="` + token + `" FriendlyName="x"/>`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTokenSourceReloadsOnChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Preferences.xml")
	writePrefs(t, path, "firstTOKEN0001")
	var seen []string
	ts := NewTokenSource(path, func(tok string) { seen = append(seen, tok) })

	tok, err := ts.Token()
	if err != nil || tok != "firstTOKEN0001" {
		t.Fatalf("got %q %v", tok, err)
	}
	// Unchanged file: cached, no callback.
	if tok, err := ts.Token(); err != nil || tok != "firstTOKEN0001" {
		t.Fatalf("got %q %v", tok, err)
	}
	if len(seen) != 1 {
		t.Fatalf("onChange calls: %v", seen)
	}

	// Different content with a different size, and an explicit mtime bump so
	// coarse filesystem timestamps cannot hide the change.
	writePrefs(t, path, "secondTOKEN00002")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	tok, err = ts.Token()
	if err != nil || tok != "secondTOKEN00002" {
		t.Fatalf("got %q %v", tok, err)
	}
	if len(seen) != 2 || seen[1] != "secondTOKEN00002" {
		t.Fatalf("onChange calls: %v", seen)
	}

	// Invalidate forces a re-read; same content means no callback.
	ts.Invalidate()
	if tok, err := ts.Token(); err != nil || tok != "secondTOKEN00002" {
		t.Fatalf("got %q %v", tok, err)
	}
	if len(seen) != 2 {
		t.Fatalf("onChange must not fire for an unchanged token: %v", seen)
	}
}

func TestTokenSourceMissingFile(t *testing.T) {
	ts := NewTokenSource(filepath.Join(t.TempDir(), "missing.xml"), nil)
	tok, err := ts.Token()
	if err == nil || tok != "" {
		t.Fatalf("expected error, got %q %v", tok, err)
	}
	if errors.Is(err, ErrNoToken) {
		t.Fatal("a missing file is not the same as an unclaimed server")
	}
}

func TestTokenSourceUnclaimedThenClaimed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Preferences.xml")
	if err := os.WriteFile(path, []byte(`<Preferences FriendlyName="x"/>`), 0o600); err != nil {
		t.Fatal(err)
	}
	ts := NewTokenSource(path, nil)
	if _, err := ts.Token(); !errors.Is(err, ErrNoToken) {
		t.Fatalf("want ErrNoToken, got %v", err)
	}
	writePrefs(t, path, "claimedTOKEN0003")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if tok, err := ts.Token(); err != nil || tok != "claimedTOKEN0003" {
		t.Fatalf("got %q %v", tok, err)
	}
}
