package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/windshare/windshare/internal/perfevidence"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return perfevidence.Main(ctx, os.Args[1:], os.Stdout, os.Stderr)
}
