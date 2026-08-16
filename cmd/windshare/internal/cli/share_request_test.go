package cli

import (
	"io"
	"path/filepath"
	"testing"
)

func TestShareRequestOwnsVerboseAndTraceOptionsAcrossInterleavedArguments(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "share.ndjson")
	app := &App{Stderr: io.Discard}
	t.Cleanup(app.closeTerminalOutput)
	request, parse := app.parseShareRequest([]string{
		"selected-one", "-v", "selected-two", "--trace", tracePath,
	})
	if parse != requestParseReady || !request.observation.verbose || request.observation.tracePath != tracePath ||
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
}
