package cli

import (
	"flag"
	"io"
	"reflect"
	"testing"
)

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
			if !options.verbose || options.tracePath != "trace.ndjson" ||
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
	options := observationOptions{tracePath: "-"}
	if err := options.validate(); err != errTraceStandardOutput {
		t.Fatalf("validate error=%v", err)
	}
	if (observationOptions{}).traceEnabled() {
		t.Fatal("empty trace path unexpectedly enabled tracing")
	}
}
