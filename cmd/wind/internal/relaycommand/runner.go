// Package relaycommand imports a sender's complete local relay selection.
package relaycommand

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/windshare/windshare/cmd/wind/internal/commandmeta"
	"github.com/windshare/windshare/cmd/wind/internal/relayconfig"
	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/senderrelay"
)

type Result uint8

const (
	ResultOK Result = iota
	ResultFailure
	ResultUsage
)

const Usage = "Usage: " + commandmeta.Name + " relay import <file|-> [-v|--verbose]\n" +
	"  Replace the saved relay candidate set; '-' reads JSON from stdin.\n"

// Runner keeps the destination authority injected so embedded callers cannot
// accidentally modify the developer's personal configuration.
type Runner struct {
	Input  io.Reader
	Output io.Writer
	Save   func([]senderrelay.Candidate) error
}

func (runner Runner) Run(ctx context.Context, args []string) Result {
	output := runner.Output
	if output == nil {
		output = io.Discard
	}
	request, err := parse(args)
	if errors.Is(err, flag.ErrHelp) {
		_, _ = io.WriteString(output, Usage)
		return ResultOK
	}
	if err != nil {
		_, _ = io.WriteString(output, Usage)
		return ResultUsage
	}
	traceOutput := io.Discard
	if request.verbose {
		traceOutput = output
	}
	sourceKind := "file"
	if request.source == "-" {
		sourceKind = "stdin"
	}
	log := slog.New(slog.NewJSONHandler(traceOutput, nil)).With(
		"operation", "relay_import", "operation_id", rand.Text(), "source", sourceKind)
	log.Info("started")
	if err := ctx.Err(); err != nil {
		log.Info("failed", "stage", "read", "reason", "cancelled")
		_, _ = fmt.Fprintln(output, "relay import: cancelled; saved configuration was not changed")
		return ResultFailure
	}
	candidates, err := runner.read(request.source)
	if err != nil {
		if errors.Is(err, relayconfig.ErrConfig) {
			log.Info("failed", "stage", "validate", "reason", "invalid_configuration")
			_, _ = fmt.Fprintf(output, "relay import: invalid relay configuration; expected 1–%d distinct relay URLs and valid optional registration tokens\n", relayset.MaximumEndpoints)
			return ResultUsage
		}
		log.Info("failed", "stage", "read", "reason", "source_unreadable")
		_, _ = fmt.Fprintln(output, "relay import: cannot read the input file or stdin; saved configuration was not changed")
		return ResultFailure
	}
	log.Info("validated", "candidate_count", len(candidates))
	if ctx.Err() != nil {
		log.Info("failed", "stage", "save", "reason", "cancelled")
		_, _ = fmt.Fprintln(output, "relay import: cancelled; saved configuration was not changed")
		return ResultFailure
	}
	if runner.Save == nil {
		log.Info("failed", "stage", "save", "reason", "destination_unavailable")
		_, _ = fmt.Fprintln(output, "relay import: configuration storage is unavailable")
		return ResultFailure
	}
	if err := runner.Save(candidates); err != nil {
		log.Info("failed", "stage", "save", "reason", "write_failed")
		_, _ = fmt.Fprintln(output, "relay import: cannot save configuration; check the user config directory or WINDSHARE_RELAY_CONFIG")
		return ResultFailure
	}
	log.Info("committed", "candidate_count", len(candidates))
	_, _ = fmt.Fprintf(output, "Saved %d relay candidate(s). Future shares use this complete set.\n", len(candidates))
	return ResultOK
}

func (runner Runner) read(source string) ([]senderrelay.Candidate, error) {
	if source == "-" {
		if runner.Input == nil {
			return nil, io.ErrUnexpectedEOF
		}
		return relayconfig.Decode(runner.Input)
	}
	file, err := os.Open(source)
	if err != nil {
		return nil, err
	}
	candidates, decodeErr := relayconfig.Decode(file)
	// Close the source before replacement so importing the saved file itself
	// also works on hosts that deny renaming an open destination.
	return candidates, errors.Join(decodeErr, file.Close())
}

type importRequest struct {
	source  string
	verbose bool
}

func parse(args []string) (importRequest, error) {
	if len(args) == 1 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		return importRequest{}, flag.ErrHelp
	}
	if len(args) == 0 || args[0] != "import" {
		return importRequest{}, errors.New("expected import")
	}
	var request importRequest
	flags := flag.NewFlagSet("relay import", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.BoolVar(&request.verbose, "v", false, "emit structured import diagnostics")
	flags.BoolVar(&request.verbose, "verbose", false, "emit structured import diagnostics")
	args = args[1:]
	var paths []string
	for {
		if err := flags.Parse(args); err != nil {
			return importRequest{}, err
		}
		rest := flags.Args()
		if len(rest) == 0 {
			break
		}
		paths = append(paths, rest[0])
		args = rest[1:]
	}
	if len(paths) != 1 || paths[0] == "" {
		return importRequest{}, errors.New("expected one input file or stdin")
	}
	request.source = paths[0]
	return request, nil
}
