package resumecommand

import (
	"context"
	"io"

	"github.com/windshare/windshare/engine"
)

type recoveryInspector interface {
	InspectRecovery(context.Context, engine.RecoveryAuthority) (*engine.RecoveryInventory, error)
}

// FilesystemConfig keeps raw terminal detection separate from serialized writes;
// the CLI's stderr lock must not hide the underlying terminal file descriptor.
type FilesystemConfig struct {
	Recovery                 recoveryInspector
	Input                    io.Reader
	Output                   io.Writer
	RawTerminalOutput        io.Writer
	SerializedTerminalOutput io.Writer
	Logf                     func(string, ...any)
}

func NewFilesystemRunner(config FilesystemConfig) Runner {
	logger := logFunc(config.Logf)
	return newRunner(resumeDependencies{
		inventories: filesystemResumeStateInventoryOpener{recovery: config.Recovery},
		confirmation: newStdioResumeConfirmationTerminal(
			config.Input, config.RawTerminalOutput, config.SerializedTerminalOutput,
		),
		parser:   flagRequestParser{logger: logger},
		renderer: textRenderer{},
		output:   streamResumeOutput{result: config.Output, usage: config.SerializedTerminalOutput},
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
	return opener.recovery.InspectRecovery(ctx, engine.FilesystemRecovery(rootPath))
}
