package guard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"
)

// metadataCacheTTL bounds how long a library item's media list is reused.
const metadataCacheTTL = 10 * time.Minute

// metadataNegativeTTL bounds how long a failed metadata lookup is remembered
// before it is retried, so a deleted item or a stalled endpoint does not cost
// one request and one warning per poll.
const metadataNegativeTTL = time.Minute

// metadataCacheMax bounds the number of cached items.
const metadataCacheMax = 512

// noTokenWarnInterval rate-limits the "no token" warning.
const noTokenWarnInterval = 5 * time.Minute

// maxBackoff caps the delay after consecutive poll failures. It equals the
// largest allowed poll interval, so backoff never shortens a poll.
const maxBackoff = MaxPollInterval

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
	lastMode        string
	lastMessage     string
	lastMsgSource   string
	lastWarnings    map[string]string
	lastNoTokenWarn time.Time
	cache           map[string]cacheEntry
}

type cacheEntry struct {
	sources []SourceMedia
	expires time.Time
	// missing records session media ids that were looked up and not found
	// in sources, so the item is not fetched again on every poll.
	missing map[string]struct{}
	// err is set for a negative entry (lookup failed).
	err error
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
	tokens := tokenSource(cfg, log, redactor)
	client, err := NewClient(cfg.PlexURL, cfg.HTTPTimeout, tokens, version)
	if err != nil {
		return nil, err
	}
	return newRunner(cfg, log, client, tokens, version), nil
}

// tokenSource wires the token file to the redactor (updated first) and to a
// log line for every load, so both the initial load and a later rotation
// are visible in the container log without ever printing the value.
func tokenSource(cfg Config, log *slog.Logger, redactor *Redactor) *TokenSource {
	return NewTokenSource(cfg.PreferencesFile, func(tok string) {
		redactor.SetSecrets(tok)
		log.Info("plex token loaded", "file", cfg.PreferencesFile)
	})
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
	r.log.Info("starting",
		"version", r.version,
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
	return min(r.cfg.PollInterval<<uint(shift), maxBackoff)
}

// PollOnce performs a single poll and acts on the result. It never panics
// on malformed input and never terminates anything in dry-run mode. All
// requests of one poll share a deadline so a stalled endpoint cannot make a
// poll run longer than one interval plus one request timeout; ctx itself
// signals shutdown.
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
	r.clearTokenWarning()

	pollCtx, cancel := context.WithTimeout(ctx, r.cfg.PollInterval+r.cfg.HTTPTimeout)
	defer cancel()
	debug := r.log.Enabled(ctx, slog.LevelDebug)

	data, err := r.client.Sessions(pollCtx)
	if err != nil {
		if ctx.Err() != nil {
			res.Err = err
			return res
		}
		if errors.Is(err, ErrUnauthorized) {
			r.log.Warn("plex rejected the token; the preferences file is re-read on the next poll", "err", err)
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
			if debug {
				r.log.Debug("skipping session", "reason", v.Reason, "key", s.SessionKey, "kind", s.Kind, "user", s.User, "title", s.DisplayTitle(), "detail", v.Detail)
			}
			continue
		}
		sources, err := r.sources(pollCtx, s.RatingKey, v.MediaID)
		if err != nil {
			if ctx.Err() != nil {
				res.Err = err
				return res
			}
			r.warnChanged("metadata:"+s.RatingKey, fmt.Sprintf("cannot resolve source media for %q (rating key %s); leaving its sessions alone: %v", s.DisplayTitle(), s.RatingKey, err))
			res.Failed++
			continue
		}
		r.warnChanged("metadata:"+s.RatingKey, "")
		v = Judge(v, sources)
		if !v.Terminate {
			if debug {
				r.log.Debug("video transcode is not a 4K source", "reason", v.Reason, "key", s.SessionKey, "user", s.User, "title", s.DisplayTitle(), "evidence", Evidence(s, v), "detail", v.Detail)
			}
			continue
		}
		res.Candidates++
		// The cooldown is keyed by mode so that sessions reported during a
		// dry-run are acted on immediately once enforcement is switched on.
		// Sessions without a Session id fall back to the sessionKey so their
		// warning is not repeated every poll.
		cooldownKey := mode.String() + ":" + s.SessionID
		if s.SessionID == "" {
			cooldownKey = mode.String() + ":key:" + s.SessionKey
		}
		allowed, remaining := r.cooldown.Allow(cooldownKey)
		if !allowed {
			if debug {
				r.log.Debug("cooldown active; not acting again yet", "session", s.SessionID, "key", s.SessionKey, "remaining", remaining.Round(time.Second))
			}
			res.Skipped++
			continue
		}
		r.log.Info("4K video transcode detected", "key", s.SessionKey, "session", s.SessionID, "user", s.User, "title", s.DisplayTitle(), "evidence", Evidence(s, v), "detail", v.Detail)
		if s.SessionID == "" {
			r.log.Warn("cannot terminate: session has no Session id", "key", s.SessionKey, "user", s.User, "title", s.DisplayTitle())
			res.Skipped++
			continue
		}
		if mode == ModeDryRun {
			r.log.Info("would terminate (dry-run)", "session", s.SessionID, "key", s.SessionKey, "user", s.User, "title", s.DisplayTitle(), "reason", message, "reason_source", msgSource)
			res.WouldTerminate++
			continue
		}
		if err := r.client.Terminate(pollCtx, s.SessionID, message); err != nil {
			if ctx.Err() != nil {
				res.Err = err
				return res
			}
			r.log.Error("terminate failed", "session", s.SessionID, "key", s.SessionKey, "user", s.User, "title", s.DisplayTitle(), "err", err)
			res.Failed++
			continue
		}
		r.log.Info("terminated session", "session", s.SessionID, "key", s.SessionKey, "user", s.User, "title", s.DisplayTitle(), "reason", message, "reason_source", msgSource)
		res.Terminated++
	}
	r.cooldown.Prune()
	r.pruneCache()
	r.log.Debug("poll complete", "mode", mode.String(), "sessions", res.Sessions, "videos", res.Videos, "video_transcodes", res.Transcoding, "uhd_transcodes", res.Candidates, "would_terminate", res.WouldTerminate, "terminated", res.Terminated, "failed", res.Failed, "skipped", res.Skipped)
	return res
}

// sources returns the library media versions for ratingKey, cached for a
// short time. A cached entry that does not contain mediaID is refreshed
// once (the library may have changed); if the id is still absent, that fact
// is cached too. Failed lookups are cached for metadataNegativeTTL.
func (r *Runner) sources(ctx context.Context, ratingKey, mediaID string) ([]SourceMedia, error) {
	if ratingKey == "" {
		return nil, errors.New("session has no ratingKey")
	}
	now := r.now()
	r.mu.Lock()
	entry, ok := r.cache[ratingKey]
	r.mu.Unlock()
	if ok && now.Before(entry.expires) {
		if entry.err != nil {
			return nil, entry.err
		}
		_, known := entry.missing[mediaID]
		if mediaID == "" || known || slices.ContainsFunc(entry.sources, func(s SourceMedia) bool { return s.ID == mediaID }) {
			return entry.sources, nil
		}
	}
	data, err := r.client.Metadata(ctx, ratingKey)
	var sources []SourceMedia
	if err == nil {
		sources, err = ParseMetadataMedia(data)
	}
	if err != nil {
		if ctx.Err() == nil {
			r.store(ratingKey, cacheEntry{err: err, expires: now.Add(metadataNegativeTTL)})
		}
		return nil, err
	}
	fresh := cacheEntry{sources: sources, expires: now.Add(metadataCacheTTL), missing: map[string]struct{}{}}
	if mediaID != "" && !slices.ContainsFunc(sources, func(s SourceMedia) bool { return s.ID == mediaID }) {
		fresh.missing[mediaID] = struct{}{}
	}
	r.store(ratingKey, fresh)
	return sources, nil
}

func (r *Runner) store(ratingKey string, entry cacheEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cache) >= metadataCacheMax {
		r.sweepCacheLocked()
		if len(r.cache) >= metadataCacheMax {
			r.cache = map[string]cacheEntry{}
		}
	}
	r.cache[ratingKey] = entry
}

// pruneCache drops expired entries.
func (r *Runner) pruneCache() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepCacheLocked()
}

func (r *Runner) sweepCacheLocked() {
	now := r.now()
	for key, entry := range r.cache {
		if !now.Before(entry.expires) {
			delete(r.cache, key)
		}
	}
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
	state := mode.String() + " " + source
	r.mu.Lock()
	changed := r.lastMode != state
	r.lastMode = state
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

func (r *Runner) clearTokenWarning() {
	r.mu.Lock()
	r.lastNoTokenWarn = time.Time{}
	r.mu.Unlock()
}

func (r *Runner) noteTokenMissing(err error) {
	now := r.now()
	r.mu.Lock()
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
