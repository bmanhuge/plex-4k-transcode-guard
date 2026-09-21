package guard

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type terminateCall struct {
	sessionID, reason string
}

type fakePlex struct {
	mu            sync.Mutex
	readyErrs     []error
	readyCalls    int
	sessions      []byte
	sessionsErr   error
	sessionsCalls int
	metadata      map[string][]byte
	metadataErr   map[string]error
	metadataCalls map[string]int
	terminateErr  error
	terminated    []terminateCall
}

func (f *fakePlex) Ready(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readyCalls++
	if len(f.readyErrs) > 0 {
		err := f.readyErrs[0]
		f.readyErrs = f.readyErrs[1:]
		return err
	}
	return nil
}

func (f *fakePlex) Sessions(context.Context) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessionsCalls++
	if f.sessionsErr != nil {
		return nil, f.sessionsErr
	}
	return f.sessions, nil
}

func (f *fakePlex) Metadata(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.metadataCalls == nil {
		f.metadataCalls = map[string]int{}
	}
	f.metadataCalls[key]++
	if err := f.metadataErr[key]; err != nil {
		return nil, err
	}
	data, ok := f.metadata[key]
	if !ok {
		return nil, &HTTPError{Op: "metadata", Status: 404}
	}
	return data, nil
}

func (f *fakePlex) Terminate(_ context.Context, id, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.terminateErr != nil {
		return f.terminateErr
	}
	f.terminated = append(f.terminated, terminateCall{id, reason})
	return nil
}

func (f *fakePlex) calls() (int, []terminateCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessionsCalls, append([]terminateCall{}, f.terminated...)
}

func mixedFake(t *testing.T) *fakePlex {
	t.Helper()
	return &fakePlex{
		sessions: fixture(t, "sessions_mixed.xml"),
		metadata: map[string][]byte{
			"769612": fixture(t, "metadata_769612.xml"),
			"786405": fixture(t, "metadata_786405.xml"),
			"700001": fixture(t, "metadata_700001.xml"),
			"800001": fixture(t, "metadata_800001.xml"),
			"900001": fixture(t, "metadata_900001.xml"),
		},
	}
}

type testEnv struct {
	runner *Runner
	logs   *bytes.Buffer
	dir    string
	prefs  string
	fake   *fakePlex
}

func (e *testEnv) logText() string { return e.logs.String() }

func newTestRunner(t *testing.T, fake *fakePlex, mutate func(*Config)) *testEnv {
	t.Helper()
	dir := t.TempDir()
	prefs := filepath.Join(dir, "Preferences.xml")
	if err := os.WriteFile(prefs, fixture(t, "preferences.xml"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.PreferencesFile = prefs
	cfg.MessageFile = filepath.Join(dir, "4k-stop-message.txt")
	cfg.LogLevel = "debug"
	if mutate != nil {
		mutate(&cfg)
	}
	var logs bytes.Buffer
	redactor := &Redactor{}
	log := NewLogger(&logs, slog.LevelDebug, redactor)
	r := newRunner(cfg, log, fake, tokenSource(cfg, log, redactor), "test")
	return &testEnv{runner: r, logs: &logs, dir: dir, prefs: prefs, fake: fake}
}

func assertNoTokenInLogs(t *testing.T, env *testEnv) {
	t.Helper()
	if strings.Contains(env.logText(), fixtureToken) {
		t.Fatalf("token leaked into logs:\n%s", env.logText())
	}
}

func TestPollDryRunNeverTerminates(t *testing.T) {
	fake := mixedFake(t)
	env := newTestRunner(t, fake, func(c *Config) { c.DryRun = true })
	res := env.runner.PollOnce(context.Background())
	if res.Err != nil {
		t.Fatalf("poll error: %v", res.Err)
	}
	if res.Mode != ModeDryRun || res.Sessions != 9 || res.Videos != 8 || res.Transcoding != 6 || res.Candidates != 3 || res.WouldTerminate != 2 || res.Terminated != 0 || res.Skipped != 1 || res.Failed != 0 {
		t.Fatalf("unexpected result: %s", res)
	}
	if _, calls := fake.calls(); len(calls) != 0 {
		t.Fatalf("dry-run must never terminate: %+v", calls)
	}
	logs := env.logText()
	for _, want := range []string{
		"would terminate (dry-run)",
		"session=e6gmj1bjf7jlbz7hu5cqcags",
		"session=unselected-session-id",
		`reason="` + DefaultStopMessage + `"`,
		"4K video transcode detected",
		"source=3840x2160/4k",
		"cannot terminate: session has no Session id",
		"stop message loaded",
		"effective mode",
		"mode=dry-run",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q\n%s", want, logs)
		}
	}
	for _, leak := range []string{"10.0.0.", "203.0.113", "aaaa-bbbb"} {
		if strings.Contains(logs, leak) {
			t.Errorf("logs leak %q", leak)
		}
	}
	assertNoTokenInLogs(t, env)
	if data, err := os.ReadFile(env.runner.cfg.MessageFile); err != nil || string(data) != DefaultStopMessage+"\n" {
		t.Fatalf("message file not created with the default text: %q %v", data, err)
	}
}

func TestPollEnforceTerminatesOnlyUHDTranscodes(t *testing.T) {
	fake := mixedFake(t)
	env := newTestRunner(t, fake, nil)
	res := env.runner.PollOnce(context.Background())
	if res.Err != nil || res.Terminated != 2 || res.WouldTerminate != 0 {
		t.Fatalf("unexpected result: %s", res)
	}
	_, calls := fake.calls()
	want := []terminateCall{
		{"e6gmj1bjf7jlbz7hu5cqcags", DefaultStopMessage},
		{"unselected-session-id", DefaultStopMessage},
	}
	if len(calls) != len(want) {
		t.Fatalf("calls: %+v", calls)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("call %d: got %+v want %+v", i, calls[i], want[i])
		}
	}
	for _, id := range []string{"85355d4a0003bbd6-com-plexapp-android", "8ba94897f066b1ef-com-plexapp-android", "direct-stream-session-id", "music-session-id", "upscale-session-id", "two-version-session-id"} {
		for _, c := range calls {
			if c.sessionID == id {
				t.Fatalf("session %s must never be terminated", id)
			}
		}
	}
	if !strings.Contains(env.logText(), "terminated session") {
		t.Fatal("expected terminated log line")
	}
	assertNoTokenInLogs(t, env)
}

func TestPollUsesCustomMessageFile(t *testing.T) {
	fake := mixedFake(t)
	env := newTestRunner(t, fake, nil)
	if err := os.WriteFile(env.runner.cfg.MessageFile, []byte("Custom stop reason\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env.runner.PollOnce(context.Background())
	_, calls := fake.calls()
	if len(calls) == 0 || calls[0].reason != "Custom stop reason" {
		t.Fatalf("calls: %+v", calls)
	}
}

func TestPollBlankMessageFileFallsBackToDefault(t *testing.T) {
	fake := mixedFake(t)
	env := newTestRunner(t, fake, nil)
	if err := os.WriteFile(env.runner.cfg.MessageFile, []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env.runner.PollOnce(context.Background())
	env.runner.PollOnce(context.Background())
	_, calls := fake.calls()
	if len(calls) == 0 || calls[0].reason != DefaultStopMessage {
		t.Fatalf("calls: %+v", calls)
	}
	if n := strings.Count(env.logText(), "is blank"); n != 1 {
		t.Fatalf("blank warning logged %d times, want once", n)
	}
}

func TestPollCooldownPreventsRepeat(t *testing.T) {
	fake := mixedFake(t)
	env := newTestRunner(t, fake, nil)
	now := time.Date(2026, 9, 21, 20, 0, 0, 0, time.UTC)
	env.runner.cooldown.now = func() time.Time { return now }

	env.runner.PollOnce(context.Background())
	res := env.runner.PollOnce(context.Background())
	if _, calls := fake.calls(); len(calls) != 2 {
		t.Fatalf("second poll inside cooldown must not re-terminate: %+v", calls)
	}
	if res.Skipped < 2 || !strings.Contains(env.logText(), "cooldown active") {
		t.Fatalf("cooldown skip not reported: %s", res)
	}
	now = now.Add(61 * time.Second)
	env.runner.PollOnce(context.Background())
	if _, calls := fake.calls(); len(calls) != 4 {
		t.Fatalf("after cooldown the session is acted on again: %+v", calls)
	}
}

func TestPollCooldownZeroRepeatsEveryPoll(t *testing.T) {
	fake := mixedFake(t)
	env := newTestRunner(t, fake, func(c *Config) { c.Cooldown = 0 })
	env.runner.PollOnce(context.Background())
	env.runner.PollOnce(context.Background())
	if _, calls := fake.calls(); len(calls) != 4 {
		t.Fatalf("calls: %+v", calls)
	}
}

func TestPollUnmatchedSourceFallsBackSafely(t *testing.T) {
	fake := mixedFake(t)
	// Serve a 1080p-only item for the 4K session's rating key: the session's
	// media id is not found, the sole version is not 4K, nothing happens.
	fake.metadata["769612"] = fixture(t, "metadata_786405.xml")
	fake.metadata["900001"] = fixture(t, "metadata_900001_mixed.xml")
	env := newTestRunner(t, fake, nil)
	for i := 0; i < 3; i++ {
		res := env.runner.PollOnce(context.Background())
		if res.Err != nil || res.Terminated != 0 || res.Candidates != 0 {
			t.Fatalf("poll %d: unexpected result: %s", i, res)
		}
	}
	if !strings.Contains(env.logText(), "media-not-found") || !strings.Contains(env.logText(), "ambiguous-source") {
		t.Fatal("unmatched and ambiguous sources should be logged")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	// Two sessions share rating key 769612 and neither id matches: the item
	// is fetched once, and the miss is remembered instead of refetching.
	if fake.metadataCalls["769612"] != 1 || fake.metadataCalls["900001"] != 1 {
		t.Fatalf("unmatched ids must not refetch every poll: %v", fake.metadataCalls)
	}
}

func TestDryRunDoesNotConsumeEnforceCooldown(t *testing.T) {
	fake := mixedFake(t)
	modeFile := filepath.Join(t.TempDir(), "mode")
	env := newTestRunner(t, fake, func(c *Config) { c.DryRun = true; c.ModeFile = modeFile })
	if res := env.runner.PollOnce(context.Background()); res.WouldTerminate != 2 {
		t.Fatalf("dry-run: %s", res)
	}
	if err := os.WriteFile(modeFile, []byte("enforce"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := env.runner.PollOnce(context.Background())
	if res.Terminated != 2 || res.Skipped != 1 {
		t.Fatalf("sessions reported in dry-run must be acted on immediately after switching to enforce: %s", res)
	}
}

func TestPollMetadataFailureIsNegativelyCachedAndWarnedOnce(t *testing.T) {
	fake := mixedFake(t)
	fake.metadataErr = map[string]error{"769612": &HTTPError{Op: "metadata", Status: 404}}
	env := newTestRunner(t, fake, func(c *Config) { c.Cooldown = 0 })
	for i := 0; i < 3; i++ {
		env.runner.PollOnce(context.Background())
	}
	fake.mu.Lock()
	calls := fake.metadataCalls["769612"]
	fake.mu.Unlock()
	if calls != 1 {
		t.Fatalf("failed lookup must be cached, got %d calls", calls)
	}
	if n := strings.Count(env.logText(), "cannot resolve source media"); n != 1 {
		t.Fatalf("warning logged %d times, want once", n)
	}
	// After the negative TTL the lookup is retried and, on success, the
	// warning is cleared.
	fake.mu.Lock()
	delete(fake.metadataErr, "769612")
	fake.mu.Unlock()
	env.runner.now = func() time.Time { return time.Now().Add(2 * metadataNegativeTTL) }
	env.runner.cooldown.now = env.runner.now
	res := env.runner.PollOnce(context.Background())
	if res.Terminated != 2 || !strings.Contains(env.logText(), "condition cleared") {
		t.Fatalf("after negative TTL: %s", res)
	}
}

func TestCachePrunesExpiredEntries(t *testing.T) {
	fake := mixedFake(t)
	env := newTestRunner(t, fake, nil)
	env.runner.PollOnce(context.Background())
	env.runner.mu.Lock()
	before := len(env.runner.cache)
	env.runner.mu.Unlock()
	if before == 0 {
		t.Fatal("expected cached items")
	}
	env.runner.now = func() time.Time { return time.Now().Add(2 * metadataCacheTTL) }
	env.runner.pruneCache()
	env.runner.mu.Lock()
	defer env.runner.mu.Unlock()
	if len(env.runner.cache) != 0 {
		t.Fatalf("expired entries remain: %d", len(env.runner.cache))
	}
}

func TestPollNoTokenSkipsPolling(t *testing.T) {
	fake := mixedFake(t)
	env := newTestRunner(t, fake, nil)
	if err := os.WriteFile(env.prefs, fixture(t, "preferences_unclaimed.xml"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := env.runner.PollOnce(context.Background())
	if !errors.Is(res.Err, ErrNoToken) {
		t.Fatalf("want ErrNoToken, got %v", res.Err)
	}
	env.runner.PollOnce(context.Background())
	if calls, _ := fake.calls(); calls != 0 {
		t.Fatalf("sessions must not be polled without a token, got %d calls", calls)
	}
	if n := strings.Count(env.logText(), "token unavailable"); n != 1 {
		t.Fatalf("no-token warning logged %d times within the rate limit window", n)
	}
}

func TestPollMissingPreferencesFile(t *testing.T) {
	fake := mixedFake(t)
	env := newTestRunner(t, fake, func(c *Config) { c.PreferencesFile = filepath.Join(t.TempDir(), "nope.xml") })
	res := env.runner.PollOnce(context.Background())
	if res.Err == nil {
		t.Fatal("expected error")
	}
	if calls, _ := fake.calls(); calls != 0 {
		t.Fatal("no polling without a token")
	}
}

func TestPollUnauthorizedTriggersReload(t *testing.T) {
	fake := mixedFake(t)
	fake.sessionsErr = ErrUnauthorized
	env := newTestRunner(t, fake, nil)
	res := env.runner.PollOnce(context.Background())
	if !errors.Is(res.Err, ErrUnauthorized) {
		t.Fatalf("got %v", res.Err)
	}
	if !strings.Contains(env.logText(), "rejected the token") {
		t.Fatal("expected a warning about the rejected token")
	}
	// A rotated token is picked up on the next poll and its load is logged.
	writePrefs(t, env.prefs, "rotatedTOKEN000009")
	fake.mu.Lock()
	fake.sessionsErr = nil
	fake.mu.Unlock()
	res = env.runner.PollOnce(context.Background())
	if res.Err != nil || res.Terminated != 2 {
		t.Fatalf("after reload: %s", res)
	}
	if tok, _ := env.runner.tokens.Token(); tok != "rotatedTOKEN000009" {
		t.Fatalf("token not reloaded: %q", tok)
	}
	if n := strings.Count(env.logText(), "plex token loaded"); n != 2 {
		t.Fatalf("token loads logged %d times, want 2 (initial + rotation)", n)
	}
	if strings.Contains(env.logText(), "rotatedTOKEN000009") {
		t.Fatal("rotated token leaked into logs")
	}
}

func TestPollLeakyErrorIsRedacted(t *testing.T) {
	fake := mixedFake(t)
	fake.sessionsErr = errors.New("upstream said: bad token " + fixtureToken)
	env := newTestRunner(t, fake, nil)
	env.runner.PollOnce(context.Background())
	if !strings.Contains(env.logText(), "[REDACTED]") {
		t.Fatalf("expected redaction marker in logs:\n%s", env.logText())
	}
	assertNoTokenInLogs(t, env)
}

func TestPollModeFileOverride(t *testing.T) {
	fake := mixedFake(t)
	modeFile := filepath.Join(t.TempDir(), "mode")
	env := newTestRunner(t, fake, func(c *Config) { c.DryRun = true; c.ModeFile = modeFile })

	if err := os.WriteFile(modeFile, []byte("off\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := env.runner.PollOnce(context.Background())
	if res.Mode != ModeOff {
		t.Fatalf("mode %v", res.Mode)
	}
	if calls, _ := fake.calls(); calls != 0 {
		t.Fatal("off mode must not poll")
	}

	if err := os.WriteFile(modeFile, []byte("enforce"), 0o644); err != nil {
		t.Fatal(err)
	}
	res = env.runner.PollOnce(context.Background())
	if res.Mode != ModeEnforce || res.Terminated != 2 {
		t.Fatalf("enforce via file: %s", res)
	}

	if err := os.Remove(modeFile); err != nil {
		t.Fatal(err)
	}
	env.runner.cooldown = NewCooldown(0)
	res = env.runner.PollOnce(context.Background())
	if res.Mode != ModeDryRun || res.Terminated != 0 || res.WouldTerminate != 2 {
		t.Fatalf("back to env dry-run: %s", res)
	}
	if n := strings.Count(env.logText(), "effective mode"); n != 3 {
		t.Fatalf("mode change logged %d times, want 3", n)
	}
	// Writing the baseline mode into the file is still a visible change of
	// source, so an operator can confirm the file took effect.
	if err := os.WriteFile(modeFile, []byte("dry-run"), 0o644); err != nil {
		t.Fatal(err)
	}
	env.runner.PollOnce(context.Background())
	if !strings.Contains(env.logText(), "mode=dry-run source=file") {
		t.Fatal("mode source change must be logged")
	}
}

func TestPollInvalidModeFileWarnsOnceAndNeverEscalates(t *testing.T) {
	fake := mixedFake(t)
	modeFile := filepath.Join(t.TempDir(), "mode")
	if err := os.WriteFile(modeFile, []byte("nuke"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Enforce baseline plus a garbage override must run in dry-run.
	env := newTestRunner(t, fake, func(c *Config) { c.DryRun = false; c.ModeFile = modeFile })
	for i := 0; i < 3; i++ {
		if res := env.runner.PollOnce(context.Background()); res.Mode != ModeDryRun || res.Terminated != 0 {
			t.Fatalf("poll %d: %s", i, res)
		}
	}
	if n := strings.Count(env.logText(), "does not contain enforce"); n != 1 {
		t.Fatalf("warning logged %d times", n)
	}
}

func TestPollMetadataErrorLeavesSessionAlone(t *testing.T) {
	fake := mixedFake(t)
	fake.metadataErr = map[string]error{"769612": errors.New("boom")}
	env := newTestRunner(t, fake, nil)
	res := env.runner.PollOnce(context.Background())
	if res.Err != nil || res.Failed != 2 || res.Terminated != 1 {
		t.Fatalf("unexpected: %s", res)
	}
	_, calls := fake.calls()
	if len(calls) != 1 || calls[0].sessionID != "unselected-session-id" {
		t.Fatalf("calls: %+v", calls)
	}
	if !strings.Contains(env.logText(), "cannot resolve source media") {
		t.Fatal("expected warning")
	}
}

func TestPollMetadataIsCached(t *testing.T) {
	fake := mixedFake(t)
	env := newTestRunner(t, fake, func(c *Config) { c.Cooldown = 0 })
	env.runner.PollOnce(context.Background())
	env.runner.PollOnce(context.Background())
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.metadataCalls["769612"] != 1 || fake.metadataCalls["786405"] != 1 {
		t.Fatalf("metadata calls: %v", fake.metadataCalls)
	}
}

func TestPollParseErrorIsReported(t *testing.T) {
	fake := mixedFake(t)
	fake.sessions = []byte("<html>oops</html>")
	env := newTestRunner(t, fake, nil)
	res := env.runner.PollOnce(context.Background())
	if res.Err == nil || res.Terminated != 0 {
		t.Fatalf("unexpected: %s", res)
	}
}

func TestPollTerminateErrorIsLoggedAndCountedOnce(t *testing.T) {
	fake := mixedFake(t)
	fake.terminateErr = &HTTPError{Op: "terminate", Status: 500}
	env := newTestRunner(t, fake, nil)
	res := env.runner.PollOnce(context.Background())
	if res.Failed != 2 || res.Terminated != 0 {
		t.Fatalf("unexpected: %s", res)
	}
	res = env.runner.PollOnce(context.Background())
	if res.Failed != 0 || res.Skipped < 2 {
		t.Fatalf("failed attempts must respect the cooldown too: %s", res)
	}
	if !strings.Contains(env.logText(), "terminate failed") {
		t.Fatal("expected error log")
	}
}

func TestNextDelayBackoff(t *testing.T) {
	env := newTestRunner(t, mixedFake(t), nil)
	r := env.runner
	if d := r.nextDelay(nil); d != 10*time.Second {
		t.Fatalf("success delay %v", d)
	}
	want := []time.Duration{20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 300 * time.Second, 300 * time.Second}
	for i, w := range want {
		if d := r.nextDelay(errors.New("x")); d != w {
			t.Fatalf("failure %d: delay %v want %v", i+1, d, w)
		}
	}
	if d := r.nextDelay(nil); d != 10*time.Second {
		t.Fatalf("delay after recovery %v", d)
	}
}

func TestRunWaitsForReadyThenStopsOnCancel(t *testing.T) {
	fake := mixedFake(t)
	fake.readyErrs = []error{errors.New("refused"), errors.New("refused")}
	env := newTestRunner(t, fake, func(c *Config) { c.DryRun = true })
	ctx, cancel := context.WithCancel(context.Background())
	var slept []time.Duration
	env.runner.sleep = func(ctx context.Context, d time.Duration) error {
		slept = append(slept, d)
		if len(slept) == 3 {
			cancel()
		}
		return ctx.Err()
	}
	done := make(chan error, 1)
	go func() { done <- env.runner.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
	if fake.readyCalls != 3 {
		t.Fatalf("ready calls %d", fake.readyCalls)
	}
	if len(slept) != 3 || slept[0] != 2*time.Second || slept[1] != 4*time.Second || slept[2] != 10*time.Second {
		t.Fatalf("sleeps %v", slept)
	}
	logs := env.logText()
	for _, want := range []string{"starting", "waiting for plex", "plex is ready", "would terminate (dry-run)"} {
		if !strings.Contains(logs, want) {
			t.Errorf("missing %q in logs", want)
		}
	}
	if _, calls := fake.calls(); len(calls) != 0 {
		t.Fatal("dry-run run loop must not terminate")
	}
}

func TestRunRealSleepRespondsToCancel(t *testing.T) {
	fake := mixedFake(t)
	env := newTestRunner(t, fake, func(c *Config) { c.DryRun = true; c.PollInterval = 5 * time.Minute })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- env.runner.Run(ctx) }()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop promptly")
	}
	if time.Since(start) > time.Second {
		t.Fatal("shutdown was not prompt")
	}
}

func TestNewRunnerWiresRealClient(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PreferencesFile = filepath.Join(t.TempDir(), "Preferences.xml")
	r, err := NewRunner(cfg, NewLogger(&bytes.Buffer{}, slog.LevelInfo, nil), &Redactor{}, "test")
	if err != nil || r == nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if _, err := NewRunner(cfg, nil, nil, "test"); err == nil {
		t.Fatal("logger is required")
	}
}
