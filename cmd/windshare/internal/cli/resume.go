package cli

import (
	"context"

	"github.com/windshare/windshare/cmd/windshare/internal/resumecommand"
)

func (a *App) runResume(ctx context.Context, args []string) int {
	serializedTerminal := a.stderrWriter()
	runner := resumecommand.NewFilesystemRunner(resumecommand.FilesystemConfig{
		Input:                    a.Stdin,
		Output:                   a.Stdout,
		RawTerminalOutput:        a.Stderr,
		SerializedTerminalOutput: serializedTerminal,
		Logf:                     a.logf,
	})
	switch runner.Run(ctx, args) {
	case resumecommand.ResultOK:
		return ExitOK
	case resumecommand.ResultUsage:
		return ExitUsage
	case resumecommand.ResultFailure:
		return ExitFailure
	default:
		a.logf("resume: command returned an invalid result")
		return ExitFailure
	}
}
