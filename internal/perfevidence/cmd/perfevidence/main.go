package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/windshare/windshare/internal/perfevidence"
	"github.com/windshare/windshare/internal/perfevidence/mutationdomain"
	"github.com/windshare/windshare/internal/perfevidence/processrun"
)

func main() {
	os.Exit(run())
}

func run() int {
	if handled, code := processrun.MaybeRunHelper(os.Args[1:], os.Stdin); handled {
		return code
	}
	if handled, code := mutationdomain.MaybeRunHelper(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); handled {
		return code
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return perfevidence.MainWithMutationDomains(
		ctx, os.Args[1:], os.Stdout, os.Stderr, mutationdomain.NewFactory(),
	)
}
