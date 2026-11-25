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
	request, exit, ready := resumecommand.Parse(args, a.stderrLines())
	if !ready {
		return exit.Int()
	}
	closeApp, err := a.ensureApplication(diagnostics.Sink{})
	if err != nil {
		return ExitFailure
	}
	defer a.closeCommandApplication(closeApp, &code)
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
	return runner.Execute(ctx, request).Int()
}
