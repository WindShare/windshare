package cli

import (
	"errors"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/commandprojection"
	"github.com/windshare/windshare/engine"
)

func reportShareResult(runtime *commandRuntime, result engine.TaskResult[engine.ShareResult], publicationErr error) int {
	if publicationErr != nil {
		code := shareExitCode(result.FailureClass)
		if code == ExitOK {
			code = ExitFailure
		}
		if publication, ok := errors.AsType[*engine.SharePublicationError](publicationErr); ok {
			switch publication.Stage {
			case engine.SharePublicationEncoding:
				emitShareKnownFailure(runtime, code, clievent.FailureCapabilityInvalid)
				return code
			case engine.SharePublicationOutput:
				emitShareKnownFailure(runtime, code, clievent.FailurePublication)
				return code
			}
		}
		emitShareCommandFailure(runtime, code, publicationErr)
		return code
	}
	if !result.Value.Ready {
		code := shareExitCode(result.FailureClass)
		if result.Err == nil && result.CleanupError == nil {
			code = ExitOK
		}
		if code != ExitOK {
			emitShareCommandFailure(runtime, code, result.Err)
		}
		return code
	}
	projected, err := commandprojection.ProjectShareResult(result)
	if err != nil {
		emitShareKnownFailure(runtime, ExitFailure, clievent.FailureUnexpected)
		return ExitFailure
	}
	event, err := clievent.NewSharingStopped(projected)
	if err != nil {
		emitShareKnownFailure(runtime, ExitFailure, clievent.FailureUnexpected)
		return ExitFailure
	}
	runtime.Finalize(event)
	code, ok := projected.ExitCode().ProcessCode()
	if !ok {
		return ExitFailure
	}
	return code
}

func emitShareCommandFailure(emitter shareCommandPublisher, exit int, cause error) {
	if emitter == nil {
		return
	}
	event, err := commandprojection.ProjectCommandFailure(
		clievent.CommandShare,
		clievent.ExitCode(exit),
		cause,
	)
	if err != nil {
		emitShareKnownFailure(emitter, ExitFailure, clievent.FailureUnexpected)
		return
	}
	if finalizer, ok := emitter.(shareCommandFinalizer); ok {
		finalizer.StageFinalization(event)
		return
	}
	emitter.Publish(event)
}

func emitShareKnownFailure(emitter shareCommandPublisher, exit int, code clievent.FailureCode) {
	if emitter == nil {
		return
	}
	event, err := clievent.NewCommandFailed(
		clievent.CommandShare,
		clievent.ExitCode(exit),
		mustShareFailure(code),
	)
	if err == nil {
		if finalizer, ok := emitter.(shareCommandFinalizer); ok {
			finalizer.StageFinalization(event)
			return
		}
		emitter.Publish(event)
	}
}

func emitShareReady(
	emitter shareCommandPublisher,
	sharingSubject clievent.SharingSubjectSelected,
	relayConnected clievent.RelayConnected,
) {
	if emitter == nil {
		return
	}
	emitter.Publish(clievent.NewReady(), sharingSubject, relayConnected)
}
