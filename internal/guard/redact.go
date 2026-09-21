package guard

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Redactor removes registered secrets from strings. It is used by the
// logger so that a token can never reach the container log, even through an
// error message produced by a third-party package.
type Redactor struct {
	mu      sync.RWMutex
	secrets []string
}

// SetSecrets replaces the set of secrets. Empty and very short values are
// ignored so that redaction cannot degrade into masking ordinary text.
func (r *Redactor) SetSecrets(secrets ...string) {
	var list []string
	for _, s := range secrets {
		s = strings.TrimSpace(s)
		if len(s) < 4 {
			continue
		}
		list = append(list, s)
		if esc := url.QueryEscape(s); esc != s {
			list = append(list, esc)
		}
	}
	r.mu.Lock()
	r.secrets = list
	r.mu.Unlock()
}

// Redact returns s with every registered secret replaced by "[REDACTED]".
func (r *Redactor) Redact(s string) string {
	r.mu.RLock()
	secrets := r.secrets
	r.mu.RUnlock()
	for _, secret := range secrets {
		s = strings.ReplaceAll(s, secret, "[REDACTED]")
	}
	return s
}

// LogPrefix is prepended to every log line so the guard is easy to grep in
// the combined container log.
const LogPrefix = "[plex-4k-guard]"

// lineHandler is a compact single-line slog handler with secret redaction.
type lineHandler struct {
	w        io.Writer
	mu       *sync.Mutex
	level    slog.Leveler
	redactor *Redactor
	attrs    []slog.Attr
	group    string
}

// NewLogger returns a logger writing one redacted line per record to w.
func NewLogger(w io.Writer, level slog.Level, redactor *Redactor) *slog.Logger {
	if redactor == nil {
		redactor = &Redactor{}
	}
	return slog.New(&lineHandler{w: w, mu: &sync.Mutex{}, level: level, redactor: redactor})
}

// ParseLogLevel converts a config level string to a slog.Level.
func ParseLogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func (h *lineHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *lineHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(LogPrefix)
	b.WriteByte(' ')
	ts := r.Time
	if ts.IsZero() {
		ts = time.Now()
	}
	b.WriteString(ts.UTC().Format(time.RFC3339))
	b.WriteByte(' ')
	b.WriteString(r.Level.String())
	b.WriteByte(' ')
	b.WriteString(r.Message)
	for _, a := range h.attrs {
		h.writeAttr(&b, "", a)
	}
	r.Attrs(func(a slog.Attr) bool {
		h.writeAttr(&b, h.group, a)
		return true
	})
	line := h.redactor.Redact(b.String()) + "\n"
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, line)
	return err
}

// writeAttr appends one attribute. prefix is the group path that applies
// to this attribute; attributes stored by WithAttrs are already qualified.
func (h *lineHandler) writeAttr(b *strings.Builder, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	key := a.Key
	if prefix != "" {
		key = prefix + "." + key
	}
	if a.Value.Kind() == slog.KindGroup {
		for _, ga := range a.Value.Group() {
			h.writeAttr(b, key, ga)
		}
		return
	}
	b.WriteByte(' ')
	b.WriteString(key)
	b.WriteByte('=')
	val := a.Value.String()
	if a.Value.Kind() == slog.KindDuration {
		val = a.Value.Duration().String()
	}
	if val == "" || strings.ContainsAny(val, " \t\r\n\"=") {
		b.WriteString(strconv.Quote(val))
	} else {
		b.WriteString(val)
	}
}

func (h *lineHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	nh := *h
	nh.attrs = append([]slog.Attr{}, h.attrs...)
	for _, a := range attrs {
		if h.group != "" {
			a.Key = h.group + "." + a.Key
		}
		nh.attrs = append(nh.attrs, a)
	}
	return &nh
}

func (h *lineHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	nh := *h
	if nh.group != "" {
		nh.group += "." + name
	} else {
		nh.group = name
	}
	return &nh
}
