package resumecommand

import (
	"context"
	"io"

	"github.com/windshare/windshare/adapters/filesystem"
	"github.com/windshare/windshare/engine"
)

type recoveryInspector interface {
	InspectRecovery(context.Context, engine.RecoveryAuthority) (*engine.RecoveryInventory, error)
}

// LineOutput commits complete diagnostic records before returning, including prompts.
type LineOutput interface {
	WriteLines(...string)
}

// FilesystemConfig keeps raw terminal detection separate from diagnostic records;
// the CLI's stderr lock must not hide the underlying terminal file descriptor.
type FilesystemConfig struct {
	Recovery          recoveryInspector
	Input             io.Reader
	Output            io.Writer
	RawTerminalOutput io.Writer
	DiagnosticOutput  LineOutput
	Logf              func(string, ...any)
}

func NewFilesystemRunner(config FilesystemConfig) Runner {
	logger := logFunc(config.Logf)
	return newRunner(resumeDependencies{
		inventories: filesystemResumeStateInventoryOpener{recovery: config.Recovery},
		confirmation: newStdioResumeConfirmationTerminal(
			config.Input, config.RawTerminalOutput, config.DiagnosticOutput,
		),
		parser:   flagRequestParser{logger: logger},
		renderer: textRenderer{},
		output:   streamResumeOutput{result: config.Output, usage: config.DiagnosticOutput},
		logger:   logger,
	})
}

type filesystemResumeStateInventoryOpener struct {
	recovery recoveryInspector
}

func (opener filesystemResumeStateInventoryOpener) OpenResumeStateInventory(
	ctx context.Context,
	rootPath string,
) (resumeStateInventory, error) {
	if opener.recovery == nil {
		return nil, engine.RecoveryDestinationFailure(engine.ErrRecoveryContract)
	}
	return opener.recovery.InspectRecovery(ctx, filesystem.Recovery(rootPath))
}
