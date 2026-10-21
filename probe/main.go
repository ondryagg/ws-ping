// Command probe is the Ondrya latency probe: a WebSocket echo service whose
// only job is to let a browser measure round-trip time to this node.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/ondryadev/ws-ping/probe/internal/app"
	"github.com/ondryadev/ws-ping/probe/internal/config"
)

// Set at build time: -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = ""
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func buildCommit() string {
	if commit != "" {
		return commit
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
	}
	return "unknown"
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	sub := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	if sub == "version" {
		_, _ = fmt.Fprintf(stdout, "ondrya-probe %s (%s)\n", version, buildCommit())
		return 0
	}
	if sub != "serve" && sub != "healthcheck" {
		_, _ = fmt.Fprintf(stderr, "usage: probe [serve|healthcheck|version] [flags]\n")
		return 2
	}

	var apply func(*app.Options)
	cfg, err := config.Load(args, getenv, func(fs *flag.FlagSet) { apply = fixtureSetup(fs) })
	var help *config.HelpError
	if errors.As(err, &help) {
		_, _ = fmt.Fprint(stdout, help.Text)
		return 0
	}
	if err != nil {
		slog.New(slog.NewJSONHandler(stderr, nil)).Error("invalid configuration, refusing to start", "err", err)
		return 2
	}

	if sub == "healthcheck" {
		if err := app.Healthcheck(cfg); err != nil {
			_, _ = fmt.Fprintf(stderr, "unhealthy: %v\n", err)
			return 1
		}
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	opts := app.Options{Cfg: cfg, Version: version, Commit: buildCommit(), LogOut: stdout, Stderr: stderr}
	if apply != nil {
		apply(&opts)
	}
	return app.Run(ctx, opts)
}
