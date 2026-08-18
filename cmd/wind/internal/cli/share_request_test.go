package cli

import (
	"io"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/runtrace"
)

func TestShareRequestOwnsVerboseAndTraceOptionsAcrossInterleavedArguments(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "share.ndjson")
	app := &App{Stderr: io.Discard}
	t.Cleanup(app.closeTerminalOutput)
	request, parse := app.parseShareRequest([]string{
		"selected-one", "-v", "selected-two", "--trace", tracePath,
	})
	target, targetErr := request.observation.traceTarget()
	expected, _ := runtrace.ExactFile(tracePath)
	if parse != requestParseReady || !request.observation.verbose || targetErr != nil || target != expected ||
		len(request.paths) != 2 || request.paths[0] != "selected-one" || request.paths[1] != "selected-two" {
		t.Fatalf("share request = %#v, parse = %d", request, parse)
	}
}

func TestShareRequestRejectsTraceOnCapabilityStream(t *testing.T) {
	app := &App{Stderr: io.Discard}
	t.Cleanup(app.closeTerminalOutput)
	if _, parse := app.parseShareRequest([]string{"selected", "--trace=-"}); parse != requestParseUsageFailure {
		t.Fatalf("trace stdout parse = %d, want %d", parse, requestParseUsageFailure)
	}
	if _, parse := app.parseShareRequest([]string{"selected", "--trace-dir=-"}); parse != requestParseUsageFailure {
		t.Fatalf("trace directory stdout parse = %d, want %d", parse, requestParseUsageFailure)
	}
	if _, parse := app.parseShareRequest([]string{
		"selected", "--trace", "one.ndjson", "--trace-dir", "traces",
	}); parse != requestParseUsageFailure {
		t.Fatalf("conflicting target parse = %d, want %d", parse, requestParseUsageFailure)
	}
}
