package guard

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestRedactorRedactsSecretsAndEncodedForms(t *testing.T) {
	var r Redactor
	r.SetSecrets("tok3n/with+chars", "ab")
	in := "url=?x=tok3n/with+chars raw=tok3n%2Fwith%2Bchars ab abc"
	got := r.Redact(in)
	if strings.Contains(got, "tok3n") {
		t.Fatalf("secret leaked: %q", got)
	}
	if !strings.Contains(got, " ab abc") {
		t.Fatalf("short secret must be ignored, got %q", got)
	}
	if got != "url=?x=[REDACTED] raw=[REDACTED] ab abc" {
		t.Fatalf("unexpected redaction: %q", got)
	}
}

func TestRedactorEmptyIsNoop(t *testing.T) {
	var r Redactor
	if got := r.Redact("plain text"); got != "plain text" {
		t.Fatalf("got %q", got)
	}
}

func TestLoggerRedactsAndFormats(t *testing.T) {
	var buf bytes.Buffer
	var r Redactor
	log := NewLogger(&buf, slog.LevelInfo, &r)
	r.SetSecrets("SECRET-TOKEN-123")

	log.Debug("hidden")
	log.Info("hello world", "err", errors.New("failed with SECRET-TOKEN-123 in header"), "n", 3, "d", 5*time.Second, "empty", "", "q", `a "b"`)
	out := buf.String()
	if strings.Contains(out, "hidden") {
		t.Fatalf("debug record must be filtered at info level: %q", out)
	}
	if strings.Contains(out, "SECRET-TOKEN-123") {
		t.Fatalf("token leaked: %q", out)
	}
	for _, want := range []string{LogPrefix + " ", " INFO hello world", `err="failed with [REDACTED] in header"`, " n=3", " d=5s", ` empty=""`, ` q="a \"b\""`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
	if !strings.HasSuffix(out, "\n") || strings.Count(out, "\n") != 1 {
		t.Fatalf("expected exactly one line: %q", out)
	}
}

func TestLoggerWithAttrsAndGroup(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelDebug, nil).With("component", "test").WithGroup("g")
	log.Debug("msg", "k", "v")
	out := buf.String()
	if !strings.Contains(out, " component=test") || !strings.Contains(out, " g.k=v") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestLoggerInlinesEmptyKeyGroup(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelDebug, nil).WithGroup("g")
	log.Info("msg", slog.Group("", slog.String("a", "1")), slog.Group("inner", slog.Int("n", 2)))
	out := buf.String()
	if !strings.Contains(out, " g.a=1") || !strings.Contains(out, " g.inner.n=2") || strings.Contains(out, "g..") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestParseLogLevel(t *testing.T) {
	cases := map[string]slog.Level{"debug": slog.LevelDebug, "INFO": slog.LevelInfo, "warn": slog.LevelWarn, "warning": slog.LevelWarn, "error": slog.LevelError, "": slog.LevelInfo, "junk": slog.LevelInfo}
	for in, want := range cases {
		if got := ParseLogLevel(in); got != want {
			t.Fatalf("%q: got %v want %v", in, got, want)
		}
	}
}
