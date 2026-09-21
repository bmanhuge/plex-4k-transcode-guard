package guard

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// ErrNoToken is returned when Preferences.xml exists but carries no
// PlexOnlineToken attribute (an unclaimed server).
var ErrNoToken = errors.New("no PlexOnlineToken in Plex preferences (server not claimed?)")

// maxPreferencesBytes bounds how much of Preferences.xml is read.
const maxPreferencesBytes = 1 << 20

// TokenSource reads the Plex server token from Preferences.xml. The file is
// small and is re-read on every call, so a rotated or newly claimed token is
// picked up on the next poll without any invalidation protocol. The token
// value itself is never logged or formatted into errors.
type TokenSource struct {
	path     string
	onChange func(token string)

	mu   sync.Mutex
	last string
}

// NewTokenSource creates a TokenSource for path. onChange, when non-nil, is
// invoked synchronously whenever a different token is loaded, before it is
// returned to callers, so a Redactor can be updated first.
func NewTokenSource(path string, onChange func(token string)) *TokenSource {
	return &TokenSource{path: path, onChange: onChange}
}

// Token returns the current token. Errors never contain the token.
func (t *TokenSource) Token() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	token, err := t.read()
	if err != nil {
		t.last = ""
		return "", err
	}
	if token != t.last {
		t.last = token
		if t.onChange != nil {
			t.onChange(token)
		}
	}
	return token, nil
}

func (t *TokenSource) read() (string, error) {
	info, err := os.Stat(t.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("preferences file not found: %w", err)
		}
		return "", fmt.Errorf("stat preferences file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("preferences file %s is not a regular file", t.path)
	}
	f, err := os.Open(t.path)
	if err != nil {
		return "", fmt.Errorf("open preferences file: %w", err)
	}
	defer f.Close()
	return ParsePreferencesToken(io.LimitReader(f, maxPreferencesBytes))
}

// ParsePreferencesToken extracts PlexOnlineToken from a Preferences.xml
// document. The document is expected to be a single <Preferences .../>
// element with attributes.
func ParsePreferencesToken(r io.Reader) (string, error) {
	dec := xml.NewDecoder(r)
	// Plex writes encoding="utf-8"; accept any declared charset as-is rather
	// than failing on the declaration.
	dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return "", ErrNoToken
		}
		if err != nil {
			return "", fmt.Errorf("parsing preferences xml: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if se.Name.Local != "Preferences" {
			return "", fmt.Errorf("parsing preferences xml: unexpected root element %q", se.Name.Local)
		}
		for _, a := range se.Attr {
			if a.Name.Local != "PlexOnlineToken" {
				continue
			}
			v := strings.TrimSpace(a.Value)
			if v == "" {
				return "", ErrNoToken
			}
			if strings.ContainsAny(v, " \t\r\n\"'<>&") {
				return "", errors.New("parsing preferences xml: PlexOnlineToken has an unexpected format")
			}
			return v, nil
		}
		return "", ErrNoToken
	}
}
