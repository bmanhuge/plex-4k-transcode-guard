package guard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// metadataCacheTTL bounds how long a library item's media list is reused.
const metadataCacheTTL = 10 * time.Minute

// metadataCacheMax bounds the number of cached items.
const metadataCacheMax = 512

// noTokenWarnInterval rate-limits the "no token" warning.
const noTokenWarnInterval = 5 * time.Minute

// maxBackoff caps the delay after consecutive poll failures.
const maxBackoff = 5 * time.Minute

// Runner is the polling loop.
type Runner struct {
	cfg      Config
	log      *slog.Logger
	client   PlexAPI
	tokens   *TokenSource
	messages *MessageSource
	cooldown *Cooldown
	version  string

	// sleep and now are injectable for tests.
	sleep func(ctx context.Context, d time.Duration) error
	now   func() time.Time

	mu              sync.Mutex
	failures        int
	lastMode        Mode
	lastModeSet     bool
	lastMessage     string
	lastMsgSource   string
	lastWarnings    map[string]string
	lastNoTokenWarn time.Time
	tokenLoaded     bool
	cache           map[string]cacheEntry
}

type cacheEntry struct {
	sources []SourceMedia
	expires time.Time
}

// PollResult summarises one poll for logging and tests.
type PollResult struct {
	Mode           Mode
	Sessions       int
	Videos         int
	Transcoding    int
	Candidates     int
	WouldTerminate int
	Terminated     int
	Failed         int
	Skipped        int
	Err            error
}

// NewRunner wires the runner from config. The redactor is updated with the
// token before any request is made so it can never appear in the log.
func NewRunner(cfg Config, log *slog.Logger, redactor *Redactor, version string) (*Runner, error) {
	if log == nil {
		return nil, errors.New("logger is required")
	}
	if redactor == nil {
		redactor = &Redactor{}
	}
	tokens := NewTokenSource(cfg.PreferencesFile, func(tok string) { redactor.SetSecrets(tok) })
	client, err := NewClient(cfg.PlexURL, cfg.HTTPTimeout, tokens, version)
	if err != nil {
		return nil, err
	}
	return newRunner(cfg, log, client, tokens, version), nil
}

// newRunner is the test seam that accepts a fake PlexAPI.
func newRunner(cfg Config, log *slog.Logger, client PlexAPI, tokens *TokenSource, version string) *Runner {
	return &Runner{
		cfg:          cfg,
		log:          log,
		client:       client,
		tokens:       tokens,
		messages:     NewMessageSource(cfg.MessageFile),
		cooldown:     NewCooldown(cfg.Cooldown),
		version:      version,
		sleep:        sleepCtx,
		now:          time.Now,
		lastWarnings: map[string]string{},
		cache:        map[string]cacheEntry{},
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Run blocks until ctx is cancelled. It waits for Plex, then polls at the
// configured interval with bounded exponential backoff after failures.
// A cancelled context is a normal shutdown and returns nil.
func (r *Runner) Run(ctx context.Context) error {
	mode, _, _ := ResolveMode(r.cfg.DryRun, r.cfg.ModeFile)
	r.log.Info("starting",
		"version", r.version,
		"mode", mode.String(),
		"dry_run_env", r.cfg.DryRun,
		"poll_interval", r.cfg.PollInterval,
		"http_timeout", r.cfg.HTTPTimeout,
		"cooldown", r.cfg.Cooldown,
		"plex_url", r.cfg.PlexURL,
		"preferences_file", r.cfg.PreferencesFile,
		"message_file", r.cfg.MessageFile,
		"mode_file", nonEmpty(r.cfg.ModeFile, "(disabled)"),
	)
	if err := r.waitReady(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	for {
		res := r.PollOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		delay := r.nextDelay(res.Err)
		if err := r.sleep(ctx, delay); err != nil {
			return nil
		}
	}
}

func (r *Runner) waitReady(ctx context.Context) error {
	attempt := 0
	for {
		err := r.client.Ready(ctx)
		if err == nil {
			r.log.Info("plex is ready", "attempts", attempt+1)
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		attempt++
		delay := min(2*time.Second<<uint(min(attempt-1, 4)), 30*time.Second)
		if attempt <= 3 || attempt%10 == 0 {
			r.log.Info("waiting for plex", "attempt", attempt, "retry_in", delay, "err", err)
		}
		if err := r.sleep(ctx, delay); err != nil {
			return err
		}
	}
}

// nextDelay returns the delay before the next poll: the poll interval on
// success, or a bounded exponential backoff after consecutive failures.
func (r *Runner) nextDelay(err error) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		r.failures = 0
		return r.cfg.PollInterval
	}
	r.failures++
	shift := min(r.failures, 5)
	delay := r.cfg.PollInterval << uint(shift)
	return min(delay, max(maxBackoff, r.cfg.PollInterval))
}

// PollOnce performs a single poll and acts on the result. It never panics
// on malformed input and never terminates anything in dry-run mode.
func (r *Runner) PollOnce(ctx context.Context) PollResult {
	mode, modeSource, warn := ResolveMode(r.cfg.DryRun, r.cfg.ModeFile)
	r.warnChanged("mode-file", warn)
	r.noteMode(mode, modeSource)
	res := PollResult{Mode: mode}
	if mode == ModeOff {
		return res
	}

	if _, err := r.tokens.Token(); err != nil {
		r.noteTokenMissing(err)
		res.Err = err
		return res
	}
	r.noteTokenLoaded()

	data, err := r.client.Sessions(ctx)
	if err != nil {
		if ctx.Err() != nil {
			res.Err = err
			return res
		}
		if errors.Is(err, ErrUnauthorized) {
			r.tokens.Invalidate()
			r.log.Warn("plex rejected the token; it will be re-read from the preferences file", "err", err)
		} else {
			r.log.Warn("polling sessions failed", "err", err)
		}
		res.Err = err
		return res
	}
	sessions, err := ParseSessions(data)
	if err != nil {
		r.log.Warn("could not parse sessions", "err", err)
		res.Err = err
		return res
	}
	res.Sessions = len(sessions.Items)
	res.Videos = sessions.Videos

	message, msgSource, msgWarn := r.messages.Message()
	r.warnChanged("message-file", msgWarn)
	r.noteMessage(message, msgSource)

	for _, s := range sessions.Items {
		if s.IsVideoTranscode() {
			res.Transcoding++
		}
		v := Screen(s)
		if !v.Candidate {
			r.log.Debug("skipping session", "reason", v.Reason, "key", s.SessionKey, "kind", s.Kind, "user", s.User, "title", s.DisplayTitle(), "detail", v.Detail)
			continue
		}
		sources, err := r.sources(ctx, s.RatingKey, v.MediaID)
		if err != nil {
			if ctx.Err() != nil {
				res.Err = err
				return res
			}
			if errors.Is(err, ErrUnauthorized) {
				r.tokens.Invalidate()
			}
			r.log.Warn("cannot resolve source media; leaving session alone", "key", s.SessionKey, "rating_key", s.RatingKey, "user", s.User, "title", s.DisplayTitle(), "err", err)
			res.Failed++
			continue
		}
		v = Judge(v, sources)
		if !v.Terminate {
			r.log.Debug("video transcode is not a 4K source", "reason", v.Reason, "key", s.SessionKey, "user", s.User, "title", s.DisplayTitle(), "evidence", Evidence(s, v), "detail", v.Detail)
			continue
		}
		res.Candidates++
		r.log.Info("4K video transcode detected", "key", s.SessionKey, "session", s.SessionID, "user", s.User, "title", s.DisplayTitle(), "evidence", Evidence(s, v), "detail", v.Detail)
		if s.SessionID == "" {
			r.log.Warn("cannot terminate: session has no Session id", "key", s.SessionKey, "user", s.User, "title", s.DisplayTitle())
			res.Skipped++
			continue
		}
		allowed, remaining := r.cooldown.Allow(s.SessionID)
		if !allowed {
			r.log.Debug("cooldown active; not acting again yet", "session", s.SessionID, "remaining", remaining.Round(time.Second))
			res.Skipped++
			continue
		}
		if mode == ModeDryRun {
			r.log.Info("would terminate (dry-run)", "session", s.SessionID, "key", s.SessionKey, "user", s.User, "title", s.DisplayTitle(), "reason", message, "reason_source", msgSource)
			res.WouldTerminate++
			continue
		}
		if err := r.client.Terminate(ctx, s.SessionID, message); err != nil {
			if errors.Is(err, ErrUnauthorized) {
				r.tokens.Invalidate()
			}
			r.log.Error("terminate failed", "session", s.SessionID, "key", s.SessionKey, "user", s.User, "title", s.DisplayTitle(), "err", err)
			res.Failed++
			continue
		}
		r.log.Info("terminated session", "session", s.SessionID, "key", s.SessionKey, "user", s.User, "title", s.DisplayTitle(), "reason", message, "reason_source", msgSource)
		res.Terminated++
	}
	r.cooldown.Prune()
	r.log.Debug("poll complete", "mode", mode.String(), "sessions", res.Sessions, "videos", res.Videos, "video_transcodes", res.Transcoding, "uhd_transcodes", res.Candidates, "would_terminate", res.WouldTerminate, "terminated", res.Terminated, "failed", res.Failed, "skipped", res.Skipped)
	return res
}

// sources returns the library media versions for ratingKey, cached for a
// short time. If a cached entry does not contain mediaID the item is
// fetched again once, in case the library changed.
func (r *Runner) sources(ctx context.Context, ratingKey, mediaID string) ([]SourceMedia, error) {
	if ratingKey == "" {
		return nil, errors.New("session has no ratingKey")
	}
	now := r.now()
	r.mu.Lock()
	entry, ok := r.cache[ratingKey]
	r.mu.Unlock()
	if ok && now.Before(entry.expires) && (mediaID == "" || containsMedia(entry.sources, mediaID)) {
		return entry.sources, nil
	}
	data, err := r.client.Metadata(ctx, ratingKey)
	if err != nil {
		return nil, err
	}
	sources, err := ParseMetadataMedia(data)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if len(r.cache) >= metadataCacheMax {
		r.cache = map[string]cacheEntry{}
	}
	r.cache[ratingKey] = cacheEntry{sources: sources, expires: now.Add(metadataCacheTTL)}
	r.mu.Unlock()
	return sources, nil
}

func containsMedia(sources []SourceMedia, id string) bool {
	for _, s := range sources {
		if s.ID == id {
			return true
		}
	}
	return false
}

// warnChanged logs a warning only when its text changes, so a persistent
// condition (blank message file, invalid mode file) is reported once rather
// than at every poll. An empty msg clears the condition.
func (r *Runner) warnChanged(key, msg string) {
	r.mu.Lock()
	prev := r.lastWarnings[key]
	if msg == "" {
		delete(r.lastWarnings, key)
	} else {
		r.lastWarnings[key] = msg
	}
	r.mu.Unlock()
	if msg != "" && msg != prev {
		r.log.Warn(msg)
	} else if msg == "" && prev != "" {
		r.log.Info("condition cleared", "about", key)
	}
}

func (r *Runner) noteMode(mode Mode, source string) {
	r.mu.Lock()
	changed := !r.lastModeSet || r.lastMode != mode
	r.lastMode = mode
	r.lastModeSet = true
	r.mu.Unlock()
	if changed {
		r.log.Info("effective mode", "mode", mode.String(), "source", source)
	}
}

func (r *Runner) noteMessage(message, source string) {
	r.mu.Lock()
	changed := r.lastMessage != message || r.lastMsgSource != source
	r.lastMessage = message
	r.lastMsgSource = source
	r.mu.Unlock()
	if changed {
		r.log.Info("stop message loaded", "source", source, "file", r.cfg.MessageFile, "reason", message)
	}
}

func (r *Runner) noteTokenLoaded() {
	r.mu.Lock()
	first := !r.tokenLoaded
	r.tokenLoaded = true
	r.lastNoTokenWarn = time.Time{}
	r.mu.Unlock()
	if first {
		r.log.Info("plex token loaded", "file", r.cfg.PreferencesFile)
	}
}

func (r *Runner) noteTokenMissing(err error) {
	now := r.now()
	r.mu.Lock()
	r.tokenLoaded = false
	due := r.lastNoTokenWarn.IsZero() || now.Sub(r.lastNoTokenWarn) >= noTokenWarnInterval
	if due {
		r.lastNoTokenWarn = now
	}
	r.mu.Unlock()
	if due {
		r.log.Warn("plex token unavailable; not polling until it can be read", "file", r.cfg.PreferencesFile, "err", err)
	}
}

// String renders a PollResult for tests and debugging.
func (p PollResult) String() string {
	return fmt.Sprintf("mode=%s sessions=%d videos=%d transcoding=%d uhd=%d would_terminate=%d terminated=%d failed=%d skipped=%d err=%v",
		p.Mode, p.Sessions, p.Videos, p.Transcoding, p.Candidates, p.WouldTerminate, p.Terminated, p.Failed, p.Skipped, p.Err)
}
