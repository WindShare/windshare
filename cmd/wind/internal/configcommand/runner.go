// Package configcommand imports persistent frontend and relay choices.
package configcommand

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
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/turn"
)

type Result uint8

const (
	ResultOK Result = iota
	ResultFailure
	ResultUsage
)

const Usage = "Usage: " + commandmeta.Name + " config import <file|-> [--front-url <url>] [-v|--verbose]\n" +
	"  Replace sharing defaults; --front-url saves the website address; '-' reads JSON from stdin.\n"

// LineOutput commits complete diagnostic records immediately; it is not a byte stream.
type LineOutput interface {
	WriteLines(...string)
}

// Runner keeps the destination authority injected so embedded callers cannot
// accidentally modify the developer's personal configuration.
type Runner struct {
	Input        io.Reader
	Output       LineOutput
	TraceHandler slog.Handler
	Save         func(relayconfig.Config) error
}

func (runner Runner) Run(ctx context.Context, args []string) Result {
	request, err := parse(args)
	if errors.Is(err, flag.ErrHelp) {
		runner.writeLine("%s", Usage)
		return ResultOK
	}
	if err != nil {
		if errors.Is(err, relayconfig.ErrFrontURL) {
			runner.writeLine("config import: %s", err)
		} else {
			runner.writeLine("%s", Usage)
		}
		return ResultUsage
	}
	traceHandler := slog.DiscardHandler
	if request.verbose && runner.TraceHandler != nil {
		traceHandler = runner.TraceHandler
	}
	sourceKind := "file"
	if request.source == "-" {
		sourceKind = "stdin"
	}
	log := slog.New(traceHandler).With(
		"operation", "config_import", "operation_id", rand.Text(), "source", sourceKind)
	log.Info("started")
	if err := ctx.Err(); err != nil {
		log.Info("failed", "stage", "read", "reason", "cancelled")
		runner.writeLine("config import: cancelled; saved configuration was not changed")
		return ResultFailure
	}
	config, err := runner.read(request.source)
	if err != nil {
		if errors.Is(err, relayconfig.ErrFrontURL) {
			log.Info("failed", "stage", "validate", "reason", "invalid_frontend", "error", err)
			runner.writeLine("config import: %s", err)
			return ResultUsage
		}
		if errors.Is(err, relayconfig.ErrConfig) {
			log.Info("failed", "stage", "validate", "reason", "invalid_configuration")
			runner.writeLine("config import: invalid sharing configuration; expected at most %d profiles, active_relays selected from those profiles, and an optional HTTP(S) front_url", relayconfig.MaximumProfiles)
			return ResultUsage
		}
		log.Info("failed", "stage", "read", "reason", "source_unreadable")
		runner.writeLine("config import: cannot read the input file or stdin; saved configuration was not changed")
		return ResultFailure
	}
	if request.frontURL != "" {
		config.FrontURL = request.frontURL
	}
	log.Info("validated", "candidate_count", len(config.Profiles), "front_url", config.FrontURL, "frontend_override", request.frontURL != "")
	if ctx.Err() != nil {
		log.Info("failed", "stage", "save", "reason", "cancelled")
		runner.writeLine("config import: cancelled; saved configuration was not changed")
		return ResultFailure
	}
	if runner.Save == nil {
		log.Info("failed", "stage", "save", "reason", "destination_unavailable")
		runner.writeLine("config import: configuration storage is unavailable")
		return ResultFailure
	}
	if err := runner.Save(config); err != nil {
		log.Info("failed", "stage", "save", "reason", "write_failed")
		runner.writeLine("config import: cannot save configuration; check the user config directory or WINDSHARE_CONFIG")
		return ResultFailure
	}
	log.Info("committed", "candidate_count", len(config.Profiles), "frontend_configured", config.FrontURL != "")
	runner.writeLine("Saved sharing configuration with %d relay candidate(s).", len(config.Profiles))
	if len(config.TURN.Profiles) > 0 {
		runner.writeLine("%s", turn.DelegationNotice)
	}
	if config.FrontURL == "" {
		runner.writeLine("No frontend configured. Re-import with --front-url <url>, or pass --front-url when sharing.")
	} else {
		runner.writeLine("Frontend: %s. Future shares use these defaults.", config.FrontURL)
	}
	return ResultOK
}

func (runner Runner) writeLine(format string, args ...any) {
	if runner.Output != nil {
		runner.Output.WriteLines(fmt.Sprintf(format, args...))
	}
}

func (runner Runner) read(source string) (relayconfig.Config, error) {
	if source == "-" {
		if runner.Input == nil {
			return relayconfig.Config{}, io.ErrUnexpectedEOF
		}
		return relayconfig.Decode(runner.Input)
	}
	file, err := os.Open(source)
	if err != nil {
		return relayconfig.Config{}, err
	}
	candidates, decodeErr := relayconfig.Decode(file)
	// Close the source before replacement so importing the saved file itself
	// also works on hosts that deny renaming an open destination.
	return candidates, errors.Join(decodeErr, file.Close())
}

type importRequest struct {
	source   string
	verbose  bool
	frontURL string
}

func parse(args []string) (importRequest, error) {
	if len(args) == 1 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		return importRequest{}, flag.ErrHelp
	}
	if len(args) == 0 || args[0] != "import" {
		return importRequest{}, errors.New("expected import")
	}
	var request importRequest
	flags := flag.NewFlagSet("config import", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.BoolVar(&request.verbose, "v", false, "emit structured import diagnostics")
	flags.BoolVar(&request.verbose, "verbose", false, "emit structured import diagnostics")
	flags.StringVar(&request.frontURL, "front-url", "", "save this frontend base URL for future shares")
	args = args[1:]
	var paths []string
	for {
		if err := flags.Parse(args); err != nil {
			return importRequest{}, err
		}
		rest := flags.Args()
		// The flag package consumes a "--" terminator silently; once it has, every
		// remaining argument is a path, matching the share and get option boundary.
		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" {
			paths = append(paths, rest...)
			break
		}
		if len(rest) == 0 {
			break
		}
		paths = append(paths, rest[0])
		args = rest[1:]
	}
	if len(paths) != 1 || paths[0] == "" {
		return importRequest{}, errors.New("expected one input file or stdin")
	}
	// Validate after flag parsing so flag.Func cannot flatten the error category
	// into a message that also echoes the rejected value. Omission stays valid.
	var frontErr error
	flags.Visit(func(option *flag.Flag) {
		if option.Name == "front-url" {
			frontErr = relayconfig.ValidateFrontURL(request.frontURL)
		}
	})
	if frontErr != nil {
		return importRequest{}, frontErr
	}
	request.source = paths[0]
	return request, nil
}
