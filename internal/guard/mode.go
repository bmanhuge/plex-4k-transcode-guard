package guard

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Mode is the effective operating mode for a poll.
type Mode int

const (
	// ModeEnforce polls, classifies and terminates matching sessions.
	ModeEnforce Mode = iota
	// ModeDryRun polls and classifies but never calls the terminate endpoint.
	ModeDryRun
	// ModeOff skips polling entirely until the mode changes.
	ModeOff
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
	return ModeEnforce, false
}

// ResolveMode computes the effective mode. The environment (dryRun) is the
// baseline. When modeFile is non-empty and the file exists with a valid
// value, the file wins; a missing file means "use the baseline". An
// unreadable or invalid file is reported through warning and the baseline
// is used, which is the safer of the two when the baseline is dry-run.
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
		return mode, source, fmt.Sprintf("mode file %s cannot be read (%v); using %s from the environment", modeFile, err, mode)
	}
	fileMode, ok := ParseMode(string(data))
	if !ok {
		return mode, source, fmt.Sprintf("mode file %s does not contain enforce, dry-run or off; using %s from the environment", modeFile, mode)
	}
	return fileMode, "file", ""
}
