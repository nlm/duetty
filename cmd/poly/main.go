// Command poly is a two-language live transcript/translation terminal app.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/nlm/poly/internal/app"
	"github.com/nlm/poly/internal/config"
)

// version is the poly release version.
const version = "0.1.0-dev"

func main() {
	cfg, err := config.Load(os.Args[1:], os.Getenv)
	var uerr *config.UsageError
	switch {
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprintf(os.Stdout, "poly %s: live two-language transcript and translation\n\n", version)
		config.Usage(os.Stdout)
		os.Exit(0)
	case errors.As(err, &uerr):
		fmt.Fprintf(os.Stderr, "poly %s: %v\n\n", version, err)
		config.Usage(os.Stderr)
		os.Exit(2)
	case err != nil:
		fmt.Fprintf(os.Stderr, "poly %s: %v\n", version, err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if err := app.Run(ctx, cfg, app.Deps{}); err != nil {
		stop()
		fmt.Fprintf(os.Stderr, "poly: %v\n", err)
		os.Exit(1)
	}
}
