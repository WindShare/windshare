package cli

import (
	"context"

	"github.com/windshare/windshare/cmd/wind/internal/resumecommand"
	"github.com/windshare/windshare/core/diagnostics"
)

func (a *App) runResume(ctx context.Context, args []string) (code int) {
	if len(args) > 0 && args[0] == "retry" {
		return a.runReceive(ctx, receiveResumeRetry, args[1:])
	}
	closeApp, err := a.ensureApplication(diagnostics.Sink{})
	if err != nil {
		return ExitFailure
	}
	defer closeApp(&code)
	runner := resumecommand.NewFilesystemRunner(resumecommand.FilesystemConfig{
		Recovery: a.application,
		Input:    a.Stdin,
		Output:   a.Stdout,
		// Resume still needs the raw descriptor for interactivity detection, but
		// every byte of human output is coordinated by TerminalCanvas.
		RawTerminalOutput: a.Stderr,
		DiagnosticOutput:  a.stderrLines(),
		Logf:              a.writeCompleteLine,
	})
	switch runner.Run(ctx, args) {
	case resumecommand.ResultOK:
		return ExitOK
	case resumecommand.ResultUsage:
		return ExitUsage
	case resumecommand.ResultFailure:
		return ExitFailure
	default:
		a.writeCompleteLine("resume: command returned an invalid result")
		return ExitFailure
	}
}
