package cli

import (
	"errors"
	"flag"
	"io"
	"reflect"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/runtrace"
)

func testExactTraceOptions(path string) observationOptions {
	var options observationOptions
	options.trace.selectTarget(observationTraceExact, path)
	return options
}

func testDirectoryTraceOptions(path string) observationOptions {
	var options observationOptions
	options.trace.selectTarget(observationTraceDirectory, path)
	return options
}

func TestObservationOptionsAliasesAndInterleavedFlags(t *testing.T) {
	for _, alias := range []string{"-v", "--verbose"} {
		t.Run(alias, func(t *testing.T) {
			flags := flag.NewFlagSet("get", flag.ContinueOnError)
			flags.SetOutput(io.Discard)
			var options observationOptions
			if err := bindObservationOptions(flags, &options); err != nil {
				t.Fatal(err)
			}
			positionals, parse := parseInterleaved(flags, []string{
				"capability", alias, "--trace", "trace.ndjson", "selection",
			})
			if parse != flagParseReady {
				t.Fatalf("parse=%d", parse)
			}
			target, targetErr := options.traceTarget()
			expected, _ := runtrace.ExactFile("trace.ndjson")
			if !options.verbose || targetErr != nil || target != expected ||
				!reflect.DeepEqual(positionals, []string{"capability", "selection"}) {
				t.Fatalf("options=%+v positionals=%q", options, positionals)
			}
		})
	}
	if err := bindObservationOptions(nil, &observationOptions{}); err != errObservationOptionsUnavailable {
		t.Fatalf("nil flag set error=%v", err)
	}
}

func TestObservationOptionsRejectTraceStandardOutput(t *testing.T) {
	options := testExactTraceOptions("-")
	if err := options.validate(); err != errTraceStandardOutput {
		t.Fatalf("validate error=%v", err)
	}
	if (observationOptions{}).traceEnabled() {
		t.Fatal("empty trace path unexpectedly enabled tracing")
	}
}

func TestObservationOptionsModelOneMutuallyExclusiveTraceTarget(t *testing.T) {
	for _, options := range []observationOptions{
		testExactTraceOptions("-"),
		testDirectoryTraceOptions("-"),
	} {
		if err := options.validate(); !errors.Is(err, errTraceStandardOutput) {
			t.Fatalf("standard stream target %+v error = %v", options, err)
		}
	}
	conflict := testExactTraceOptions("one.ndjson")
	conflict.trace.selectTarget(observationTraceDirectory, "traces")
	conflict.trace.selectTarget(observationTraceExact, "replacement.ndjson")
	if err := conflict.validate(); !errors.Is(err, errTraceTargetConflict) {
		t.Fatalf("conflict error = %v", err)
	}
	if !conflict.traceEnabled() {
		t.Fatal("conflicting explicit targets were not recognized before validation")
	}

	flags := flag.NewFlagSet("share", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var directory observationOptions
	if err := bindObservationOptions(flags, &directory); err != nil {
		t.Fatal(err)
	}
	positionals, parse := parseInterleaved(flags, []string{"source", "--trace-dir", "traces", "second"})
	target, targetErr := directory.traceTarget()
	expected, _ := runtrace.RunDirectory("traces")
	if parse != flagParseReady || targetErr != nil || target != expected ||
		!reflect.DeepEqual(positionals, []string{"source", "second"}) {
		t.Fatalf("parse=%d options=%+v positionals=%q", parse, directory, positionals)
	}
}
