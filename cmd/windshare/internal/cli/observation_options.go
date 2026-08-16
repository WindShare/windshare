package cli

import (
	"errors"
	"flag"
)

var (
	errObservationOptionsUnavailable = errors.New("observation options are unavailable")
	errTraceStandardOutput           = errors.New("trace output must be a file")
)

// observationOptions belongs to the parsed command request. Keeping these
// values out of App prevents one invocation's verbose or trace policy from
// leaking into another invocation in in-process tests.
type observationOptions struct {
	verbose   bool
	tracePath string
}

func bindObservationOptions(flags *flag.FlagSet, options *observationOptions) error {
	if flags == nil || options == nil {
		return errObservationOptionsUnavailable
	}
	flags.BoolVar(&options.verbose, "v", false, "show diagnostic milestones")
	flags.BoolVar(&options.verbose, "verbose", false, "show diagnostic milestones")
	flags.StringVar(&options.tracePath, "trace", "", "write privacy-safe NDJSON trace to file")
	return nil
}

func (options observationOptions) validate() error {
	if options.tracePath == "-" {
		return errTraceStandardOutput
	}
	return nil
}

func (options observationOptions) traceEnabled() bool { return options.tracePath != "" }
