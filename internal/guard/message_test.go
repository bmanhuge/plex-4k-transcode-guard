package guard

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMessageCreatesDefaultFileAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "4k-stop-message.txt")
	ms := NewMessageSource(path)

	msg, source, warn := ms.Message()
	if msg != DefaultStopMessage || source != "file" || warn != "" {
		t.Fatalf("got msg=%q source=%q warn=%q", msg, source, warn)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != DefaultStopMessage+"\n" {
		t.Fatalf("file content %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != MessageFileMode {
		t.Fatalf("mode %o, want %o", info.Mode().Perm(), MessageFileMode)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestMessageDefaultTextIsExact(t *testing.T) {
	const want = "You are not allowed to transcode 4K content, please play the normal resolution version."
	if DefaultStopMessage != want {
		t.Fatalf("default message drifted: %q", DefaultStopMessage)
	}
}

func TestMessageCustomFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "msg.txt")
	if err := os.WriteFile(path, []byte("  Custom stop reason \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	msg, source, warn := NewMessageSource(path).Message()
	if msg != "Custom stop reason" || source != "file" || warn != "" {
		t.Fatalf("got msg=%q source=%q warn=%q", msg, source, warn)
	}
}

func TestMessageMultilineCollapsed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "msg.txt")
	if err := os.WriteFile(path, []byte("line one\r\nline   two\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	msg, _, _ := NewMessageSource(path).Message()
	if msg != "line one line two" {
		t.Fatalf("got %q", msg)
	}
}

func TestMessageBlankFileUsesDefaultWithoutTouchingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "msg.txt")
	if err := os.WriteFile(path, []byte("  \n\t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	msg, source, warn := NewMessageSource(path).Message()
	if msg != DefaultStopMessage || source != "default" || !strings.Contains(warn, "blank") {
		t.Fatalf("got msg=%q source=%q warn=%q", msg, source, warn)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "  \n\t\n" {
		t.Fatalf("blank file must not be rewritten, got %q", data)
	}
}

func TestMessageUnreadableFileUsesDefault(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	path := filepath.Join(t.TempDir(), "msg.txt")
	if err := os.WriteFile(path, []byte("secret"), 0o000); err != nil {
		t.Fatal(err)
	}
	msg, source, warn := NewMessageSource(path).Message()
	if msg != DefaultStopMessage || source != "default" || !strings.Contains(warn, "cannot be read") {
		t.Fatalf("got msg=%q source=%q warn=%q", msg, source, warn)
	}
}

func TestMessageMissingDirectoryUsesDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "msg.txt")
	ms := NewMessageSource(path)
	msg, source, warn := ms.Message()
	if msg != DefaultStopMessage || source != "default" || !strings.Contains(warn, "could not be created") {
		t.Fatalf("got msg=%q source=%q warn=%q", msg, source, warn)
	}
	// The warning text must be stable across retries so it is logged once.
	if _, _, again := ms.Message(); again != warn {
		t.Fatalf("warning text changed between calls:\n%s\n%s", warn, again)
	}
	if strings.Contains(warn, ".tmp") {
		t.Fatalf("warning leaks the random temp name: %s", warn)
	}
}

func TestMessageHugeFileIsTruncatedNotReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "msg.txt")
	if err := os.WriteFile(path, []byte("Custom "+strings.Repeat("x", 100<<10)), 0o644); err != nil {
		t.Fatal(err)
	}
	msg, source, warn := NewMessageSource(path).Message()
	if source != "file" || !strings.HasPrefix(msg, "Custom x") || len([]rune(msg)) != MaxMessageLength || !strings.Contains(warn, "truncated") {
		t.Fatalf("got source=%q len=%d warn=%q", source, len([]rune(msg)), warn)
	}
}

func TestMessageTooLongIsTruncated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "msg.txt")
	long := strings.Repeat("é", MaxMessageLength+50)
	if err := os.WriteFile(path, []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	msg, source, warn := NewMessageSource(path).Message()
	if source != "file" || !strings.Contains(warn, "truncated") {
		t.Fatalf("got source=%q warn=%q", source, warn)
	}
	if got := len([]rune(msg)); got != MaxMessageLength {
		t.Fatalf("length %d", got)
	}
}

func TestMessageReloadsWhenFileChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "msg.txt")
	if err := os.WriteFile(path, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	ms := NewMessageSource(path)
	if msg, _, _ := ms.Message(); msg != "first" {
		t.Fatalf("got %q", msg)
	}
	if err := os.WriteFile(path, []byte("second message"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if msg, _, _ := ms.Message(); msg != "second message" {
		t.Fatalf("got %q", msg)
	}
}

func TestEnsureMessageFileDoesNotOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "msg.txt")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	created, err := EnsureMessageFile(path, DefaultStopMessage)
	if err != nil || created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "keep" {
		t.Fatalf("existing file modified: %q", data)
	}
}

func TestMessageRejectsNonRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan struct{})
	var msg, source, warn string
	go func() { msg, source, warn = NewMessageSource(path).Message(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reading a FIFO must not block")
	}
	if msg != DefaultStopMessage || source != "default" || !strings.Contains(warn, "not a regular file") {
		t.Fatalf("got msg=%q source=%q warn=%q", msg, source, warn)
	}
}

func TestNormalizeMessage(t *testing.T) {
	if got := NormalizeMessage("\n  a\x00b \t c\r\n"); got != "ab c" {
		t.Fatalf("got %q", got)
	}
}
