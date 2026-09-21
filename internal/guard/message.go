package guard

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// DefaultStopMessage is the reason shown to the viewer when the message
// file is missing, blank or unreadable. It is also the exact content written
// when the file is created.
const DefaultStopMessage = "You are not allowed to transcode 4K content, please play the normal resolution version."

// MaxMessageLength caps the reason sent to Plex (in runes).
const MaxMessageLength = 1000

// maxMessageFileBytes bounds how much of the message file is read. Anything
// beyond it is ignored (the text is truncated to MaxMessageLength anyway).
const maxMessageFileBytes = 64 << 10

// MessageFileMode is the permission applied to a freshly created file.
const MessageFileMode os.FileMode = 0o644

// MessageSource reads the termination reason from a file, creating the file
// with the default text when it does not exist. Edits are picked up on the
// next read without a restart.
type MessageSource struct {
	path string

	mu      sync.Mutex
	loaded  bool
	modTime time.Time
	size    int64
	message string
	source  string
	warning string
}

// NewMessageSource creates a MessageSource for path.
func NewMessageSource(path string) *MessageSource {
	return &MessageSource{path: path}
}

// Message returns the reason to send, where it came from ("file" or
// "default"), and a warning describing why the default is in use (empty when
// everything is fine).
func (m *MessageSource) Message() (message, source, warning string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	info, err := os.Stat(m.path)
	switch {
	case err == nil:
		if m.loaded && info.ModTime().Equal(m.modTime) && info.Size() == m.size {
			return m.message, m.source, m.warning
		}
	case errors.Is(err, os.ErrNotExist):
		created, cerr := EnsureMessageFile(m.path, DefaultStopMessage)
		if cerr != nil {
			return m.remember(DefaultStopMessage, "default", fmt.Sprintf("message file %s is missing and could not be created (%v); using the default message", m.path, stableError(cerr)), nil)
		}
		if created {
			return m.remember(DefaultStopMessage, "file", "", nil)
		}
		info, err = os.Stat(m.path)
		if err != nil {
			return m.remember(DefaultStopMessage, "default", fmt.Sprintf("message file %s cannot be read (%v); using the default message", m.path, err), nil)
		}
	default:
		return m.remember(DefaultStopMessage, "default", fmt.Sprintf("message file %s cannot be read (%v); using the default message", m.path, err), nil)
	}

	data, err := readPrefix(m.path, maxMessageFileBytes)
	if err != nil {
		return m.remember(DefaultStopMessage, "default", fmt.Sprintf("message file %s cannot be read (%v); using the default message", m.path, err), nil)
	}
	text := NormalizeMessage(string(data))
	if text == "" {
		return m.remember(DefaultStopMessage, "default", fmt.Sprintf("message file %s is blank; using the default message", m.path), info)
	}
	warn := ""
	if utf8.RuneCountInString(text) > MaxMessageLength {
		text = truncateRunes(text, MaxMessageLength)
		warn = fmt.Sprintf("message file %s is longer than %d characters; truncated", m.path, MaxMessageLength)
	}
	return m.remember(text, "file", warn, info)
}

func (m *MessageSource) remember(message, source, warning string, info os.FileInfo) (string, string, string) {
	m.message = message
	m.source = source
	m.warning = warning
	if info != nil {
		m.loaded = true
		m.modTime = info.ModTime()
		m.size = info.Size()
	} else {
		m.loaded = false
	}
	return message, source, warning
}

// stableError strips the path from a *fs.PathError so a retried operation
// that uses random temporary names yields the same text every time, which
// lets the runner report the condition once instead of on every poll.
func stableError(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %w", pe.Op, pe.Err)
	}
	return err
}

// NormalizeMessage trims the text and collapses internal whitespace
// (including line breaks) into single spaces so the reason is a single
// line, both in the Plex dialog and in the guard's own log.
func NormalizeMessage(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	return strings.Join(strings.Fields(s), " ")
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// EnsureMessageFile creates path with content (plus a trailing newline) if
// it does not exist. The write is atomic: the content is written to a
// temporary file in the same directory and renamed into place, so a reader
// never observes a partial file. An existing file is never modified.
func EnsureMessageFile(path, content string) (created bool, err error) {
	if _, err := os.Lstat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".4k-stop-message-*.tmp")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.WriteString(content + "\n"); err != nil {
		_ = tmp.Close()
		cleanup()
		return false, err
	}
	if err := tmp.Chmod(MessageFileMode); err != nil {
		_ = tmp.Close()
		cleanup()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return false, err
	}
	// Re-check right before the rename so a file that appeared meanwhile
	// (another writer, a restore) is kept rather than replaced.
	if _, err := os.Lstat(path); err == nil {
		cleanup()
		return false, nil
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return false, err
	}
	return true, nil
}

// readPrefix reads at most limit bytes from path. Only regular files are
// read: opening a FIFO or a device could block the poll loop forever.
func readPrefix(path string, limit int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit))
}

// readSmallFile reads path and fails if the file is larger than limit.
func readSmallFile(path string, limit int64) ([]byte, error) {
	data, err := readPrefix(path, limit+1)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file is larger than %d bytes", limit)
	}
	return data, nil
}
