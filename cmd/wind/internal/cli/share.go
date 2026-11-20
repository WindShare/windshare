package cli

import (
	"context"
	"errors"

	"github.com/windshare/windshare/adapters/filesystem"
	"github.com/windshare/windshare/cmd/wind/internal/accountcommand"
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/commandprojection"
	"github.com/windshare/windshare/cmd/wind/internal/networkcommand"
	"github.com/windshare/windshare/cmd/wind/internal/observationbridge"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/account"
	appobs "github.com/windshare/windshare/engine/observation"
)

func (a *App) runShare(ctx context.Context, args []string) int {
	request, parse := a.parseShareRequest(args)
	if parse != requestParseReady {
		return parse.exitCode()
	}
	if err := a.application.ConfigureTURN(request.sharing.TURNSettings()); err != nil {
		a.writeCompleteLine("share: TURN configuration: %v", err)
		return ExitUsage
	}
	runtime, err := a.newCommandRuntime(clievent.CommandShare, request.observation)
	if err != nil {
		if !errors.Is(err, errUserTraceOpen) {
			a.writeCompleteLine("share: command observation could not start")
		}
		return ExitFailure
	}
	observations := newShareProjection(runtime)
	defer func() {
		runtime.FinalizeStaged()
		runtime.Close()
	}()

	// Ctrl+C means explicitly stop publishing this share. Its cleanup lifetime
	// belongs to the engine and therefore outlives this command's canceled wait.
	current, err := a.application.StartShare(context.WithoutCancel(ctx), a.engineShareRequest(request, runtime))
	if err != nil {
		return runtime.failToStart(err)
	}
	if a.networkSetup != nil {
		networkcommand.ObserveStartup(a.networkSetup, runtime, clievent.CommandShare)
	}
	go func() {
		select {
		case <-ctx.Done():
			current.StopShare()
		case <-current.Done():
		}
	}()
	gate := &observationbridge.PublicationGate{}
	reader := observationbridge.Start(current.Observations(), gate, func(readContext context.Context, value engine.Observation) {
		gate.Commit(readContext, func() bool {
			observeEngineTask(runtime, value)
			a.observeEngineShare(observations, value.Event)
			return true
		})
	})

	finishAccountProgress := accountcommand.Watch(ctx, a.application.Accounts(), a.terminalOutput().canvas)
	defer finishAccountProgress()
	ready, readyErr, authRequired := awaitShareStartup(current)
	finishAccountProgress()
	var publicationErr error
	if !authRequired && readyErr == nil {
		publicationErr = a.publishEngineShare(ready, request.link, runtime, observations)
		if publicationErr != nil {
			// Service readiness precedes presentation. Even a partial output failure
			// must stop the share and join its work before this command returns.
			current.StopShare()
		}
	}
	result, waitErr := current.Wait(context.Background())
	completionContext, cancel := context.WithTimeout(context.Background(), observationCompletionTimeout)
	observations.reportReaderStatus(appobs.ObserverLossCommandAdapter, reader.Join(completionContext))
	cancel()
	if result.Observations.CapacityDropped > 0 {
		runtime.ReportObserverLoss(appobs.ObserverLossEngineObservation, appobs.ObserverLossStreamCapacity, result.Observations.CapacityDropped)
	}
	if waitErr != nil {
		emitShareCommandFailure(runtime, ExitFailure, waitErr)
		return ExitFailure
	}
	if authRequired {
		if accounts := a.application.Accounts(); accounts != nil {
			if snapshot, _ := accounts.Snapshot(); snapshot.State == account.RegistrationRequired || snapshot.State == account.EmailRequired {
				a.writeCompleteLine("%s", accountcommand.Describe(snapshot))
			}
		}
		emitShareKnownFailure(runtime, ExitNetwork, appobs.FailureRelayAccessDenied)
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
		Source: filesystem.ShareSource(request.paths), RelayIntent: request.relayIntent, Frontend: request.link.frontURL, ChunkSize: request.chunkSize,
		Diagnostics: runtime.diagnosticSink(),
	}
}

func (a *App) publishEngineShare(ready engine.ShareReady, presentation shareLinkPresentation, runtime *commandRuntime, observations *shareObservations) error {
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
	origin := ready.RelayOrigin
	observations.SetRelayOrigin(origin)
	subject, err := commandprojection.ProjectSharingSubject(ready.SelectedRootSummary)
	if err != nil {
		return err
	}
	connected, err := clievent.NewRelayConnected(clievent.CommandShare, origin)
	if err != nil {
		return err
	}
	emitShareReady(runtime, subject, connected)
	return nil
}

func shareExitCode(class engine.FailureClass) int {
	switch class {
	case engine.FailureNone:
		return ExitOK
	case engine.FailureUsage:
		return ExitUsage
	case engine.FailureNetwork:
		return ExitNetwork
	case engine.FailureSourceDrift:
		return ExitDrift
	default:
		return ExitFailure
	}
}
