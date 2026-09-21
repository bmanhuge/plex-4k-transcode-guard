// Command plex-4k-guard watches a local Plex Media Server and terminates
// video sessions that transcode a 4K/UHD source. It is designed to run as an
// s6-overlay longrun inside a LinuxServer.io Plex container.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bmanhuge/plex-4k-transcode-guard/internal/guard"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// fatalConfigDelay keeps a misconfigured service from being restarted by
// s6 in a hot loop.
const fatalConfigDelay = 30 * time.Second

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-version", "version":
			fmt.Println("plex-4k-guard " + version)
			return
		case "--help", "-h", "help":
			usage()
			return
		}
	}
	os.Exit(run())
}

func usage() {
	fmt.Println("plex-4k-guard " + version)
	fmt.Println("Terminates Plex video sessions that transcode a 4K/UHD source.")
	fmt.Println()
	fmt.Println("Configuration is read from the environment:")
	fmt.Printf("  %-34s true|false (default false)\n", guard.EnvDryRun)
	fmt.Printf("  %-34s %s..%s (default %s)\n", guard.EnvPollInterval, guard.MinPollInterval, guard.MaxPollInterval, guard.DefaultPollInterval)
	fmt.Printf("  %-34s %s..%s (default %s)\n", guard.EnvHTTPTimeout, guard.MinHTTPTimeout, guard.MaxHTTPTimeout, guard.DefaultHTTPTimeout)
	fmt.Printf("  %-34s 0..%s (default %s)\n", guard.EnvCooldown, guard.MaxCooldown, guard.DefaultCooldown)
	fmt.Printf("  %-34s default %s\n", guard.EnvPlexURL, guard.DefaultPlexURL)
	fmt.Printf("  %-34s default %q\n", guard.EnvPreferencesFile, guard.DefaultPreferencesFile)
	fmt.Printf("  %-34s default %s\n", guard.EnvMessageFile, guard.DefaultMessageFile)
	fmt.Printf("  %-34s optional runtime override file (enforce|dry-run|off)\n", guard.EnvModeFile)
	fmt.Printf("  %-34s debug|info|warn|error (default %s)\n", guard.EnvLogLevel, guard.DefaultLogLevel)
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	redactor := &guard.Redactor{}
	cfg, cfgErr := guard.LoadConfig(os.Getenv)
	logger := guard.NewLogger(os.Stdout, guard.ParseLogLevel(cfg.LogLevel), redactor)
	if cfgErr != nil {
		logger.Error("invalid configuration; refusing to start", "err", cfgErr, "retry_in", fatalConfigDelay)
		select {
		case <-ctx.Done():
		case <-time.After(fatalConfigDelay):
		}
		return 1
	}

	runner, err := guard.NewRunner(cfg, logger, redactor, version)
	if err != nil {
		logger.Error("cannot initialise", "err", err, "retry_in", fatalConfigDelay)
		select {
		case <-ctx.Done():
		case <-time.After(fatalConfigDelay):
		}
		return 1
	}
	if err := runner.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("stopped with error", "err", err)
		return 1
	}
	logger.Info("stopped")
	return 0
}
