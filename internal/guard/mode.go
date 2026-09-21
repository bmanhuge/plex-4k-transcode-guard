package guard

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Mode is the effective operating mode for a poll.
type Mode int

// The zero value is ModeOff so an unset or unparsed Mode can never enforce.
const (
	// ModeOff skips polling entirely until the mode changes.
	ModeOff Mode = iota
	// ModeDryRun polls and classifies but never calls the terminate endpoint.
	ModeDryRun
	// ModeEnforce polls, classifies and terminates matching sessions.
	ModeEnforce
)

// String returns the canonical spelling used in logs and in the mode file.
func (m Mode) String() string {
	switch m {
	case ModeEnforce:
		return "enforce"
	case ModeDryRun:
		return "dry-run"
	case ModeOff:
		return "off"
	}
	return fmt.Sprintf("mode(%d)", int(m))
}

// ParseMode accepts the canonical spellings plus a few obvious aliases.
func ParseMode(s string) (Mode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "enforce", "enforced", "on":
		return ModeEnforce, true
	case "dry-run", "dryrun", "dry_run", "dry":
		return ModeDryRun, true
	case "off", "disabled", "disable", "pause", "paused":
		return ModeOff, true
	}
	return ModeOff, false
}

// ResolveMode computes the effective mode. The environment (dryRun) is the
// baseline. When modeFile is non-empty and the file exists with a valid
// value, the file wins; a missing file means "use the baseline". An
// unreadable or invalid file never escalates: the result is the baseline
// or dry-run, whichever is less permissive, and the problem is reported
// through warning. This keeps a mistyped "dry-run" from silently leaving
// enforcement on.
func ResolveMode(dryRun bool, modeFile string) (mode Mode, source string, warning string) {
	mode = ModeEnforce
	if dryRun {
		mode = ModeDryRun
	}
	source = "env"
	if modeFile == "" {
		return mode, source, ""
	}
	data, err := readSmallFile(modeFile, 4096)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return mode, source, ""
		}
		safe := min(mode, ModeDryRun)
		return safe, "fallback", fmt.Sprintf("mode file %s cannot be read (%v); running in %s until it is fixed or removed", modeFile, err, safe)
	}
	fileMode, ok := ParseMode(string(data))
	if !ok {
		safe := min(mode, ModeDryRun)
		return safe, "fallback", fmt.Sprintf("mode file %s does not contain enforce, dry-run or off; running in %s until it is fixed or removed", modeFile, safe)
	}
	return fileMode, "file", ""
}
