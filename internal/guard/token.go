package guard

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// ErrNoToken is returned when Preferences.xml exists but carries no
// PlexOnlineToken attribute (an unclaimed server).
var ErrNoToken = errors.New("no PlexOnlineToken in Plex preferences (server not claimed?)")

// maxPreferencesBytes bounds how much of Preferences.xml is read.
const maxPreferencesBytes = 1 << 20

// TokenSource reads the Plex server token from Preferences.xml. It caches
// the token and transparently re-reads the file whenever its size or
// modification time changes, or after Invalidate is called (for example on
// an HTTP 401). The token value itself is never logged or formatted into
// errors.
type TokenSource struct {
	path     string
	onChange func(token string)

	mu      sync.Mutex
	token   string
	loaded  bool
	modTime time.Time
	size    int64
}

// NewTokenSource creates a TokenSource for path. onChange, when non-nil, is
// invoked synchronously with every newly loaded token before it is returned
// to callers, so a Redactor can be updated first.
func NewTokenSource(path string, onChange func(token string)) *TokenSource {
	return &TokenSource{path: path, onChange: onChange}
}

// Token returns the current token, reloading it from disk if the file
// changed. Errors never contain the token.
func (t *TokenSource) Token() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	info, err := os.Stat(t.path)
	if err != nil {
		t.loaded = false
		t.token = ""
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("preferences file not found: %w", err)
		}
		return "", fmt.Errorf("stat preferences file: %w", err)
	}
	if t.loaded && info.ModTime().Equal(t.modTime) && info.Size() == t.size {
		return t.token, nil
	}

	f, err := os.Open(t.path)
	if err != nil {
		t.loaded = false
		t.token = ""
		return "", fmt.Errorf("open preferences file: %w", err)
	}
	defer f.Close()
	token, err := ParsePreferencesToken(io.LimitReader(f, maxPreferencesBytes))
	if err != nil {
		t.loaded = false
		t.token = ""
		return "", err
	}
	changed := token != t.token
	t.token = token
	t.loaded = true
	t.modTime = info.ModTime()
	t.size = info.Size()
	if changed && t.onChange != nil {
		t.onChange(token)
	}
	return token, nil
}

// Invalidate forces the next Token call to re-read the file.
func (t *TokenSource) Invalidate() {
	t.mu.Lock()
	t.loaded = false
	t.mu.Unlock()
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
