package cli

import (
	"errors"
	"flag"

	"github.com/windshare/windshare/cmd/wind/internal/runtrace"
)

var (
	errObservationOptionsUnavailable = errors.New("observation options are unavailable")
	errTraceStandardOutput           = errors.New("trace output must be a file")
	errTraceTargetConflict           = errors.New("trace file and directory targets conflict")
)

// observationOptions belongs to the parsed command request. Keeping these
// values out of App prevents one invocation's verbose or trace policy from
// leaking into another invocation in in-process tests.
type observationOptions struct {
	verbose bool
	trace   observationTraceSelection
}

type observationTraceMode uint8

const (
	observationTraceUnset observationTraceMode = iota
	observationTraceExact
	observationTraceDirectory
)

// observationTraceSelection preserves one closed target from parsing through
// opening. Conflict is sticky so later repeated flags cannot erase the fact
// that both mutually exclusive spellings were supplied.
type observationTraceSelection struct {
	mode       observationTraceMode
	target     runtrace.Target
	targetErr  error
	conflicted bool
}

type observationTraceFlag struct {
	selection *observationTraceSelection
	mode      observationTraceMode
}

func (value observationTraceFlag) String() string { return "" }

func (value observationTraceFlag) Set(path string) error {
	if value.selection == nil {
		return errObservationOptionsUnavailable
	}
	value.selection.selectTarget(value.mode, path)
	return nil
}

func (selection *observationTraceSelection) selectTarget(mode observationTraceMode, path string) {
	if selection == nil {
		return
	}
	if selection.mode != observationTraceUnset && selection.mode != mode {
		selection.conflicted = true
		return
	}
	selection.mode = mode
	if path == "-" {
		selection.target = runtrace.Target{}
		selection.targetErr = errTraceStandardOutput
		return
	}
	var (
		target runtrace.Target
		err    error
	)
	switch mode {
	case observationTraceExact:
		target, err = runtrace.ExactFile(path)
	case observationTraceDirectory:
		target, err = runtrace.RunDirectory(path)
	default:
		err = runtrace.ErrInvalidTarget
	}
	selection.target = target
	selection.targetErr = err
}

func bindObservationOptions(flags *flag.FlagSet, options *observationOptions) error {
	if flags == nil || options == nil {
		return errObservationOptionsUnavailable
	}
	flags.BoolVar(&options.verbose, "v", false, "show diagnostic milestones")
	flags.BoolVar(&options.verbose, "verbose", false, "show diagnostic milestones")
	flags.Var(observationTraceFlag{selection: &options.trace, mode: observationTraceExact}, "trace", "write privacy-safe NDJSON trace to an exact new file")
	flags.Var(observationTraceFlag{selection: &options.trace, mode: observationTraceDirectory}, "trace-dir", "write privacy-safe NDJSON trace to a generated file in this directory")
	return nil
}

func (options observationOptions) validate() error {
	if options.trace.conflicted {
		return errTraceTargetConflict
	}
	return options.trace.targetErr
}

func (options observationOptions) traceEnabled() bool {
	return options.trace.mode != observationTraceUnset
}

func (options observationOptions) traceTarget() (runtrace.Target, error) {
	if err := options.validate(); err != nil {
		return runtrace.Target{}, err
	}
	if !options.traceEnabled() {
		return runtrace.Target{}, runtrace.ErrInvalidTarget
	}
	return options.trace.target, nil
}

func (options observationOptions) traceDirectoryMode() bool {
	return options.trace.mode == observationTraceDirectory
}

func observationOptionDiagnostic(err error) string {
	switch {
	case errors.Is(err, errTraceTargetConflict):
		return "--trace and --trace-dir cannot be used together"
	case errors.Is(err, errTraceStandardOutput):
		return "trace output must be a file"
	default:
		return "trace target is invalid"
	}
}
