package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/commandprojection"
	"github.com/windshare/windshare/cmd/wind/internal/observationbridge"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/internal/testrun"
)

func (a *App) runShare(ctx context.Context, args []string) int {
	request, parse := a.parseShareRequest(args)
	if parse != requestParseReady {
		return parse.exitCode()
	}
	runtime, err := a.newCommandRuntime(clievent.CommandShare, request.observation)
	if err != nil {
		if !errors.Is(err, errUserTraceOpen) {
			_, _ = fmt.Fprintln(a.stderrWriter(), "share: command observation could not start")
		}
		return ExitFailure
	}
	observations := newShareProjection(runtime)
	defer func() {
		runtime.FinalizeStaged()
		runtime.Close()
	}()
	releaseCapacityTrace := a.revisionCapacityTrace.Bind(observations.capacityTracer())
	defer releaseCapacityTrace()

	// Ctrl+C means explicitly stop publishing this share. Its cleanup lifetime
	// belongs to the engine and therefore outlives this command's canceled wait.
	current, err := a.application.StartShare(context.WithoutCancel(ctx), a.engineShareRequest(request, runtime))
	if err != nil {
		emitShareCommandFailure(runtime, ExitFailure, err)
		return ExitFailure
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
			observeEngineTask(runtime, clievent.CommandShare, value)
			if event, ok := value.Event.(engine.ShareObservation); ok {
				a.observeEngineShare(observations, event)
			}
			return true
		})
	})

	ready, readyErr := current.Ready(context.Background())
	var publicationErr error
	if readyErr == nil {
		publicationErr = a.publishEngineShare(current, ready, request.link, runtime, observations)
	}
	result, waitErr := current.Wait(context.Background())
	completionContext, cancel := context.WithTimeout(context.Background(), observationCompletionTimeout)
	observations.reportReaderStatus(clievent.ObserverLossCommandAdapter, reader.Join(completionContext))
	cancel()
	for _, loss := range result.Value.ObservationLosses {
		a.observeEngineShare(observations, engine.ShareObservation{Loss: &loss})
	}
	if result.Observations.CapacityDropped > 0 {
		runtime.ReportObserverLoss(clievent.ObserverLossCommandAdapter, clievent.ObserverLossStreamCapacity, result.Observations.CapacityDropped)
	}
	if waitErr != nil {
		emitShareCommandFailure(runtime, ExitFailure, waitErr)
		return ExitFailure
	}
	return reportShareResult(runtime, result, publicationErr)
}

func (a *App) engineShareRequest(request shareRequest, runtime *commandRuntime) engine.ShareRequest {
	return engine.ShareRequest{
		Source: shareFileSource(request.paths), RelayURLs: append([]string(nil), request.relayURLs...), ChunkSize: request.chunkSize,
		Diagnostics: runtime.detailedDiagnosticsEnabled(), TraceLifecycle: runtime.traceRecordingEnabled() || a.processTrace != nil,
	}
}

func shareFileSource(paths []string) liveshare.FileSourceFactory {
	selected := append([]string(nil), paths...)
	return liveshare.FileSourceFactoryFunc(func(ctx context.Context, source liveshare.FileSourceContext) (liveshare.FileSource, error) {
		return osfs.NewSelectedFileSource(ctx, osfs.SelectedCatalogSourceConfig{
			Paths: selected, SyntheticRoot: source.SyntheticRoot, Identities: osfs.CatalogIdentitySourceFunc(source.NewIdentity),
		})
	})
}

func (a *App) publishEngineShare(current *engine.ShareTask, ready engine.ShareReady, presentation shareLinkPresentation, runtime *commandRuntime, observations *shareObservations) error {
	payload, err := buildShareCapabilityPayload(ready.Capability, presentation)
	if err != nil {
		failure := &engine.SharePublicationError{Stage: engine.SharePublicationEncoding, Cause: err}
		current.Activate(failure)
		return failure
	}
	defer clear(payload)
	if err := publishShareCapability(a.Stdout, payload); err != nil {
		failure := &engine.SharePublicationError{Stage: engine.SharePublicationOutput, Cause: err}
		current.Activate(failure)
		return failure
	}
	current.Activate(nil)
	if err := current.Activated(context.Background()); err != nil {
		return err
	}
	a.recordProcessTrace(processTraceShareComponent, processTraceSenderReady, testrun.OutcomeSucceeded)
	if err := a.processTrace.err(); err != nil {
		current.StopShare()
		return &engine.SharePublicationError{Stage: engine.SharePublicationReadiness, Cause: err}
	}
	authority, err := commandprojection.RelayAuthority(ready.RelayEndpoint)
	if err != nil {
		current.StopShare()
		return err
	}
	observations.SetRelayAuthority(authority)
	subject, err := commandprojection.ProjectSharingSubject(ready.SelectedRootSummary)
	if err != nil {
		current.StopShare()
		return err
	}
	connected, err := clievent.NewRelayConnected(clievent.CommandShare, authority)
	if err != nil {
		current.StopShare()
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
