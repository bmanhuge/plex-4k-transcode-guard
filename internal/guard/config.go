// Package guard implements the Plex 4K transcode guard: a small supervised
// service that watches a local Plex Media Server, finds video sessions that
// are actively transcoding a 4K/UHD source, and terminates only those
// sessions with an operator-provided message.
package guard

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Environment variable names understood by the service.
const (
	EnvDryRun          = "PLEX_4K_GUARD_DRY_RUN"
	EnvPollInterval    = "PLEX_4K_GUARD_POLL_INTERVAL"
	EnvHTTPTimeout     = "PLEX_4K_GUARD_HTTP_TIMEOUT"
	EnvCooldown        = "PLEX_4K_GUARD_COOLDOWN"
	EnvPlexURL         = "PLEX_4K_GUARD_PLEX_URL"
	EnvPreferencesFile = "PLEX_4K_GUARD_PREFERENCES_FILE"
	EnvMessageFile     = "PLEX_4K_GUARD_MESSAGE_FILE"
	EnvModeFile        = "PLEX_4K_GUARD_MODE_FILE"
	EnvLogLevel        = "PLEX_4K_GUARD_LOG_LEVEL"
)

// Defaults and validation bounds.
const (
	DefaultPollInterval    = 10 * time.Second
	DefaultHTTPTimeout     = 5 * time.Second
	DefaultCooldown        = 60 * time.Second
	DefaultPlexURL         = "http://127.0.0.1:32400"
	DefaultPreferencesFile = "/config/Library/Application Support/Plex Media Server/Preferences.xml"
	DefaultMessageFile     = "/config/4k-stop-message.txt"
	DefaultLogLevel        = "info"

	MinPollInterval = 2 * time.Second
	MaxPollInterval = 5 * time.Minute
	MinHTTPTimeout  = 1 * time.Second
	MaxHTTPTimeout  = 60 * time.Second
	MaxCooldown     = time.Hour
)

// Config is the validated runtime configuration.
type Config struct {
	// DryRun disables the termination call. Everything else (polling,
	// classification, logging) behaves identically.
	DryRun bool
	// PollInterval is the delay between two /status/sessions polls.
	PollInterval time.Duration
	// HTTPTimeout bounds every request to Plex (dial, headers and body).
	HTTPTimeout time.Duration
	// Cooldown is the minimum time between two termination attempts for the
	// same Plex session id. Zero disables the cooldown.
	Cooldown time.Duration
	// PlexURL is the base URL of the local Plex Media Server.
	PlexURL string
	// PreferencesFile is the Plex Preferences.xml holding PlexOnlineToken.
	PreferencesFile string
	// MessageFile holds the termination reason shown to the user.
	MessageFile string
	// ModeFile, when non-empty, is an optional runtime override file
	// containing "enforce", "dry-run" or "off". Empty disables the override.
	ModeFile string
	// LogLevel is "debug", "info", "warn" or "error".
	LogLevel string
}

// DefaultConfig returns the built-in defaults.
func DefaultConfig() Config {
	return Config{
		DryRun:          false,
		PollInterval:    DefaultPollInterval,
		HTTPTimeout:     DefaultHTTPTimeout,
		Cooldown:        DefaultCooldown,
		PlexURL:         DefaultPlexURL,
		PreferencesFile: DefaultPreferencesFile,
		MessageFile:     DefaultMessageFile,
		ModeFile:        "",
		LogLevel:        DefaultLogLevel,
	}
}

// LoadConfig builds a Config from the environment (getenv is normally
// os.Getenv). Every variable is validated strictly; all problems are
// reported together in the returned error. Unset or blank variables keep
// their defaults.
func LoadConfig(getenv func(string) string) (Config, error) {
	cfg := DefaultConfig()
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	if raw := strings.TrimSpace(getenv(EnvDryRun)); raw != "" {
		v, err := parseBool(EnvDryRun, raw)
		add(err)
		if err == nil {
			cfg.DryRun = v
		}
	}
	if raw := strings.TrimSpace(getenv(EnvPollInterval)); raw != "" {
		v, err := parseDuration(EnvPollInterval, raw, MinPollInterval, MaxPollInterval)
		add(err)
		if err == nil {
			cfg.PollInterval = v
		}
	}
	if raw := strings.TrimSpace(getenv(EnvHTTPTimeout)); raw != "" {
		v, err := parseDuration(EnvHTTPTimeout, raw, MinHTTPTimeout, MaxHTTPTimeout)
		add(err)
		if err == nil {
			cfg.HTTPTimeout = v
		}
	}
	if raw := strings.TrimSpace(getenv(EnvCooldown)); raw != "" {
		v, err := parseDuration(EnvCooldown, raw, 0, MaxCooldown)
		add(err)
		if err == nil {
			cfg.Cooldown = v
		}
	}
	if raw := strings.TrimSpace(getenv(EnvPlexURL)); raw != "" {
		v, err := parseBaseURL(EnvPlexURL, raw)
		add(err)
		if err == nil {
			cfg.PlexURL = v
		}
	}
	if raw := strings.TrimSpace(getenv(EnvPreferencesFile)); raw != "" {
		add(validateAbsPath(EnvPreferencesFile, raw))
		cfg.PreferencesFile = raw
	}
	if raw := strings.TrimSpace(getenv(EnvMessageFile)); raw != "" {
		add(validateAbsPath(EnvMessageFile, raw))
		cfg.MessageFile = raw
	}
	if raw := strings.TrimSpace(getenv(EnvModeFile)); raw != "" {
		add(validateAbsPath(EnvModeFile, raw))
		cfg.ModeFile = raw
	}
	if raw := strings.ToLower(strings.TrimSpace(getenv(EnvLogLevel))); raw != "" {
		switch raw {
		case "debug", "info", "warn", "warning", "error":
			if raw == "warning" {
				raw = "warn"
			}
			cfg.LogLevel = raw
		default:
			add(fmt.Errorf("%s: %q is not one of debug, info, warn, error", EnvLogLevel, raw))
		}
	}

	if len(errs) > 0 {
		return cfg, errors.Join(errs...)
	}
	return cfg, nil
}

func parseBool(name, raw string) (bool, error) {
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("%s: %q is not a boolean (use true/false)", name, raw)
}

// parseDuration accepts a Go duration ("15s", "1m30s") or a bare number of
// seconds ("15"), then range-checks it.
func parseDuration(name, raw string, minimum, maximum time.Duration) (time.Duration, error) {
	var d time.Duration
	if secs, err := strconv.ParseFloat(raw, 64); err == nil {
		if secs < 0 || secs > float64(maximum/time.Second)+1 {
			return 0, fmt.Errorf("%s: %q is out of range (%s to %s)", name, raw, minimum, maximum)
		}
		d = time.Duration(secs * float64(time.Second))
	} else {
		parsed, perr := time.ParseDuration(raw)
		if perr != nil {
			return 0, fmt.Errorf("%s: %q is not a duration (examples: 10s, 1m, 15)", name, raw)
		}
		d = parsed
	}
	if d < minimum || d > maximum {
		return 0, fmt.Errorf("%s: %s is out of range (%s to %s)", name, d, minimum, maximum)
	}
	return d, nil
}

func parseBaseURL(name, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%s: %q is not a valid URL", name, raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%s: scheme must be http or https", name)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%s: host is required", name)
	}
	if u.User != nil {
		return "", fmt.Errorf("%s: credentials in the URL are not allowed", name)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%s: query strings and fragments are not allowed", name)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), nil
}

func validateAbsPath(name, raw string) error {
	if !filepath.IsAbs(raw) {
		return fmt.Errorf("%s: %q must be an absolute path", name, raw)
	}
	return nil
}
