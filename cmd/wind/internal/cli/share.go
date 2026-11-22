package cli

import (
	"context"
	"errors"

	"github.com/windshare/windshare/adapters/filesystem"
	"github.com/windshare/windshare/cmd/wind/internal/accountcommand"
	"github.com/windshare/windshare/cmd/wind/internal/humanoutput"
	"github.com/windshare/windshare/cmd/wind/internal/networkcommand"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/account"
)

func (a *App) runShare(ctx context.Context, args []string) (code int) {
	request, parse := a.parseShareRequest(args)
	if parse != requestParseReady {
		return parse.exitCode()
	}
	runtime, err := a.newCommandRuntime(commandShare, request.observation)
	if err != nil {
		if !errors.Is(err, errUserTraceOpen) {
			a.writeCompleteLine("share: command observation could not start")
		}
		return ExitFailure
	}
	defer runtime.Close()
	closeApp, err := a.ensureApplication(runtime.diagnosticSink())
	if err != nil {
		return ExitFailure
	}
	defer closeApp(&code)

	// Ctrl+C means explicitly stop publishing this share. Its cleanup lifetime
	// belongs to the engine and therefore outlives this command's canceled wait.
	current, err := a.application.StartShare(context.WithoutCancel(ctx), a.engineShareRequest(request, runtime))
	if err != nil {
		return runtime.failToStart(err)
	}
	if a.networkSetup != nil {
		networkcommand.ObserveStartup(a.networkSetup, runtime.ObserveNetworkSetup)
	}
	go func() {
		select {
		case <-ctx.Done():
			current.StopShare()
		case <-current.Done():
		}
	}()
	observationsDone := make(chan struct{})
	go func() {
		defer close(observationsDone)
		for value := range current.Observations() {
			runtime.ObserveEngine(value)
		}
	}()

	finishAccountProgress := accountcommand.Watch(ctx, a.application.Accounts(), a.terminalOutput().canvas)
	defer finishAccountProgress()
	ready, readyErr, authRequired := awaitShareStartup(current)
	finishAccountProgress()
	var publicationErr error
	if !authRequired && readyErr == nil {
		publicationErr = a.publishEngineShare(ready, request.link, runtime)
		if publicationErr != nil {
			// Service readiness precedes presentation. Even a partial output failure
			// must stop the share and join its work before this command returns.
			current.StopShare()
		}
	}
	result, waitErr := current.Wait(context.Background())
	// Engine closes Observations() before resolving Done/Wait, so draining finishes promptly.
	<-observationsDone
	runtime.AddDropped(result.Observations.CapacityDropped)
	if waitErr != nil {
		runtime.StageCommandFailure(ExitFailure, classifyFailure(waitErr))
		return ExitFailure
	}
	if authRequired {
		if accounts := a.application.Accounts(); accounts != nil {
			snapshot, _ := accounts.Snapshot()
			switch snapshot.State {
			case account.RegistrationRequired, account.EmailRequired:
				a.writeCompleteLine("%s", accountcommand.Describe(snapshot))
			case account.StorageError:
				if snapshot.Reason != "" {
					a.writeCompleteLine("Account: %s", snapshot.Reason)
				}
				runtime.StageCommandFailure(ExitFailure, engine.Failure{Code: engine.FailureAccountStorageError})
				return ExitFailure
			}
		}
		runtime.StageCommandFailure(ExitNetwork, engine.Failure{Code: engine.FailureRelayAccessDenied})
		return ExitNetwork
	}
	return reportShareResult(runtime, result, publicationErr)
}

// awaitShareStartup resolves initial publication for the CLI. Unlike the
// desktop host, a foreground terminal command cannot complete account setup
// concurrently, so waiting when every candidate relay requires user action
// would block the prompt indefinitely.
func awaitShareStartup(current *engine.ShareTask) (engine.ShareReady, error, bool) {
	for {
		changed := current.Changes()
		snapshot := current.Snapshot()
		switch snapshot.Startup {
		case engine.ShareReadyReached:
			return snapshot.Ready, nil, false
		case engine.ShareStartupFailed:
			return engine.ShareReady{}, snapshot.StartupError, false
		case engine.ShareAwaitingAuthorization:
			if current.CancelBeforeReady() {
				return engine.ShareReady{}, nil, true
			}
			// Publication or failure won the race against cancellation; return it.
			ready, err := current.Ready(context.Background())
			return ready, err, false
		}
		select {
		case <-changed:
		case <-current.Done():
		}
	}
}

func (a *App) engineShareRequest(request shareRequest, runtime *commandRuntime) engine.ShareRequest {
	return engine.ShareRequest{
		Source:      filesystem.ShareSource(request.paths),
		RelayIntent: request.relayIntent,
		Frontend:    request.link.frontURL,
		ChunkSize:   request.chunkSize,
		Diagnostics: runtime.diagnosticSink(),
	}
}

func (a *App) publishEngineShare(ready engine.ShareReady, presentation shareLinkPresentation, runtime *commandRuntime) error {
	if ready.Frontend != "" {
		presentation.frontURL = ready.Frontend
	}
	payload, err := buildShareCapabilityPayload(ready.Capability, presentation)
	if err != nil {
		return errors.Join(errShareCapabilityEncoding, err)
	}
	defer clear(payload)
	if err := publishShareCapability(a.Stdout, payload); err != nil {
		return errors.Join(errShareCapabilityOutput, err)
	}
	runtime.PublishShareReady(ready.SelectedRootSummary, ready.RelayOrigin)
	return nil
}

func reportShareResult(runtime *commandRuntime, result engine.TaskResult[engine.ShareResult], publicationErr error) int {
	if publicationErr != nil {
		// The engine settles service shutdown separately. Its stop outcome and
		// cleanup failures cannot turn an output error into success or usage error.
		switch {
		case errors.Is(publicationErr, errShareCapabilityEncoding):
			runtime.StageCommandFailure(ExitUsage, engine.Failure{Code: engine.FailureCapabilityInvalid})
			return ExitUsage
		case errors.Is(publicationErr, errShareCapabilityOutput):
			runtime.StageCommandFailure(ExitFailure, engine.Failure{Code: engine.FailurePublication})
			return ExitFailure
		}
		runtime.StageCommandFailure(ExitFailure, classifyFailure(publicationErr))
		return ExitFailure
	}
	code := exitCodeForClass(result.FailureClass)
	if !result.Value.Ready && (result.Err != nil || result.CleanupError != nil) {
		if code != ExitOK {
			cause := result.Err
			if cause == nil {
				cause = result.CleanupError
			}
			runtime.StageCommandFailure(code, classifyFailure(cause))
		}
		return code
	}
	stoppedCleanly := code == ExitOK
	var failure *engine.Failure
	if !stoppedCleanly {
		cause := result.Err
		if cause == nil {
			cause = result.CleanupError
		}
		f := classifyFailure(cause)
		failure = &f
	}
	runtime.StageShareResult(code, humanoutput.ShareResult{
		Published:      result.Value.Ready,
		StoppedCleanly: stoppedCleanly,
		Elapsed:        result.Value.Elapsed,
		Failure:        failure,
	})
	return code
}
