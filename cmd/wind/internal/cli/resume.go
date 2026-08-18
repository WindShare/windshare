package cli

import (
	"context"

	"github.com/windshare/windshare/cmd/wind/internal/resumecommand"
)

func (a *App) runResume(ctx context.Context, args []string) int {
	terminal := a.terminalOutput()
	serializedTerminal := completeLineCanvasWriter{canvas: terminal.canvas}
	runner := resumecommand.NewFilesystemRunner(resumecommand.FilesystemConfig{
		Input:  a.Stdin,
		Output: a.Stdout,
		// Resume still needs the raw descriptor for interactivity detection, but
		// every byte of human output is coordinated by TerminalCanvas.
		RawTerminalOutput:        a.Stderr,
		SerializedTerminalOutput: serializedTerminal,
		Logf:                     a.writeCompleteLine,
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
