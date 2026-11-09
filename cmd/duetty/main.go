// Command duetty is a two-language live transcript/translation terminal app.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/nlm/duetty/internal/app"
	"github.com/nlm/duetty/internal/config"
)

// version is the duetty release version.
var version = "0.1.0-dev" // set at release build time: -ldflags "-X main.version=..."

func main() {
	cfg, err := config.Load(os.Args[1:], os.Getenv)
	var uerr *config.UsageError
	switch {
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprintf(os.Stdout, "duetty %s: live two-language transcript and translation\n\n", version)
		config.Usage(os.Stdout)
		os.Exit(0)
	case errors.As(err, &uerr):
		fmt.Fprintf(os.Stderr, "duetty %s: %v\n\n", version, err)
		config.Usage(os.Stderr)
		os.Exit(2)
	case err != nil:
		fmt.Fprintf(os.Stderr, "duetty %s: %v\n", version, err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if cfg.PrintWhisperHint {
		if err := app.PrintWhisperHints(ctx, cfg, os.Stdout); err != nil {
			stop()
			fmt.Fprintf(os.Stderr, "duetty: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := app.Run(ctx, cfg, app.Deps{}); err != nil {
		stop()
		fmt.Fprintf(os.Stderr, "duetty: %v\n", err)
		os.Exit(1)
	}
}
