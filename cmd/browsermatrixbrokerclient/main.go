package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/windshare/windshare/internal/browsermatrixbroker"
)

func main() {
	os.Exit(command())
}

func command() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return execute(ctx, os.Args, os.Getwd, os.Stdin, os.Stdout, os.Stderr)
}

func execute(
	ctx context.Context,
	arguments []string,
	workingDirectory func() (string, error),
	input io.Reader,
	output io.Writer,
	errorOutput io.Writer,
) int {
	if len(arguments) != 1 {
		_, _ = io.WriteString(errorOutput, "credential broker client arguments rejected\n")
		return 2
	}
	directory, err := workingDirectory()
	if err != nil {
		_, _ = io.WriteString(errorOutput, "credential broker client configuration rejected\n")
		return 2
	}
	config, err := browsermatrixbroker.LoadClientConfig(
		filepath.Join(directory, browsermatrixbroker.ClientConfigFileName),
	)
	if err != nil {
		_, _ = io.WriteString(errorOutput, "credential broker client configuration rejected\n")
		return 2
	}
	if err := run(ctx, config, input, output); err != nil {
		_, _ = io.WriteString(errorOutput, "credential broker client exchange failed\n")
		return 1
	}
	return 0
}

func run(
	ctx context.Context,
	config browsermatrixbroker.ClientConfig,
	input io.Reader,
	output io.Writer,
) error {
	client, err := browsermatrixbroker.NewClient(config)
	if err != nil {
		return errors.New("credential broker client construction failed")
	}
	defer client.Close()
	return client.Exchange(ctx, input, output)
}
