// Package configcommand imports persistent frontend and relay choices.
package configcommand

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"

	"github.com/windshare/windshare/cmd/wind/internal/command"
	"github.com/windshare/windshare/cmd/wind/internal/commandinput"
	"github.com/windshare/windshare/engine/relayconfig"
)

// Synopsis is the single source of truth for config command lines shared
// between configcommand.Usage and top-level wind usage.
const Synopsis = command.Name + " config import <file|-> [--front-url <url>] [-v|--verbose]"

const Usage = "Usage: " + Synopsis + "\n" +
	"  Replace sharing defaults; --front-url saves the website address; '-' reads JSON from stdin.\n"

// Runner keeps the destination authority injected so embedded callers cannot
// accidentally modify the developer's personal configuration.
type Runner struct {
	Input        io.Reader
	Output       command.LineOutput
	TraceHandler slog.Handler
	Save         func(relayconfig.Config) error
}

func (runner Runner) Run(ctx context.Context, args []string) command.ExitCode {
	request, outcome, err := parse(args)
	if outcome == command.ParseHelp {
		runner.writeLine("%s", Usage)
		return command.ExitOK
	}
	if outcome != command.ParseReady {
		runner.writeLine("config import: %s", outcome.Diagnostic())
		return outcome.ExitCode()
	}
	if err != nil {
		if errors.Is(err, relayconfig.ErrFrontURL) {
			runner.writeLine("config import: %s", err)
		} else {
			runner.writeLine("%s", Usage)
		}
		return command.ExitUsage
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
		return command.ExitFailure
	}
	config, err := commandinput.Read(ctx, func() (relayconfig.Config, error) {
		return runner.read(request.source)
	})
	if ctx.Err() != nil {
		log.Info("failed", "stage", "read", "reason", "cancelled")
		runner.writeLine("config import: cancelled; saved configuration was not changed")
		return command.ExitFailure
	}
	if err != nil {
		if errors.Is(err, relayconfig.ErrFrontURL) {
			log.Info("failed", "stage", "validate", "reason", "invalid_frontend", "error", err)
			runner.writeLine("config import: %s", err)
			return command.ExitUsage
		}
		// Name the member: common JSON checkers accept repetitions silently.
		if errors.Is(err, relayconfig.ErrRepeatedMember) {
			log.Info("failed", "stage", "validate", "reason", "repeated_member", "error", err)
			runner.writeLine("config import: %s", err)
			return command.ExitUsage
		}
		if errors.Is(err, relayconfig.ErrRelayURL) {
			log.Info("failed", "stage", "validate", "reason", "invalid_relay", "error", err)
			runner.writeLine("config import: %s", err)
			return command.ExitUsage
		}
		if errors.Is(err, relayconfig.ErrConfig) {
			log.Info("failed", "stage", "validate", "reason", "invalid_configuration")
			runner.writeLine("config import: invalid sharing configuration. Provide a profiles array with up to %d entries. Optional fields include active_relays selected from those profiles, an HTTP(S) front_url, and up to %d turn_services.", relayconfig.MaximumProfiles, relayconfig.MaxTURNServices)
			return command.ExitUsage
		}
		log.Info("failed", "stage", "read", "reason", "source_unreadable")
		runner.writeLine("config import: cannot read the input file or stdin; saved configuration was not changed")
		return command.ExitFailure
	}
	if request.frontURL != "" {
		config.FrontURL = request.frontURL
	}
	log.Info("validated", "candidate_count", len(config.Profiles), "front_url", config.FrontURL, "frontend_override", request.frontURL != "")
	if ctx.Err() != nil {
		log.Info("failed", "stage", "save", "reason", "cancelled")
		runner.writeLine("config import: cancelled; saved configuration was not changed")
		return command.ExitFailure
	}
	if runner.Save == nil {
		log.Info("failed", "stage", "save", "reason", "destination_unavailable")
		runner.writeLine("config import: configuration storage is unavailable")
		return command.ExitFailure
	}
	if err := runner.Save(config); err != nil {
		log.Info("failed", "stage", "save", "reason", "write_failed")
		runner.writeLine("config import: cannot save configuration; check the user config directory or WINDSHARE_CONFIG")
		return command.ExitFailure
	}
	log.Info("committed", "candidate_count", len(config.Profiles), "frontend_configured", config.FrontURL != "")
	runner.writeLine("Saved sharing configuration with %d relay candidate(s).", len(config.Profiles))
	if config.FrontURL == "" {
		runner.writeLine("Sharing uses the built-in receiver website.")
	} else {
		runner.writeLine("Frontend: %s. Future shares use these defaults.", config.FrontURL)
	}
	return command.ExitOK
}

func (runner Runner) writeLine(format string, args ...any) {
	command.WriteLinef(runner.Output, format, args...)
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

func parse(args []string) (importRequest, command.ParseOutcome, error) {
	if len(args) > 0 && command.IsHelpToken(args[0]) {
		return importRequest{}, command.ParseHelp, nil
	}
	if len(args) == 0 || args[0] != "import" {
		return importRequest{}, command.ParseReady, errors.New("expected import")
	}
	var request importRequest
	flags := command.NewFlagSet("config import")
	flags.BoolVar(&request.verbose, "v", false, "emit structured import diagnostics")
	flags.BoolVar(&request.verbose, "verbose", false, "emit structured import diagnostics")
	flags.StringVar(&request.frontURL, "front-url", "", "save this frontend base URL for future shares")
	paths, outcome := command.ParseInterleaved(flags, args[1:])
	if outcome != command.ParseReady {
		return importRequest{}, outcome, nil
	}
	if len(paths) != 1 || paths[0] == "" {
		return importRequest{}, command.ParseReady, errors.New("expected one input file or stdin")
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
		return importRequest{}, command.ParseReady, frontErr
	}
	request.source = paths[0]
	return request, command.ParseReady, nil
}
