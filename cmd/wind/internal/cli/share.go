package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/commandprojection"
	"github.com/windshare/windshare/cmd/wind/internal/observationbridge"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/engine"
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/internal/testrun"
)

func (a *App) runShare(ctx context.Context, args []string) int {
	request, parse := a.parseShareRequest(args)
	if parse != requestParseReady {
		return parse.exitCode()
	}
	if err := a.application.ConfigureTURN(request.sharing.TURNSettings()); err != nil {
		_, _ = fmt.Fprintf(a.stderrWriter(), "share: TURN configuration: %v\n", err)
		return ExitUsage
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
			a.observeEngineShare(observations, value.Event)
			return true
		})
	})

	ready, readyErr := current.Ready(context.Background())
	var publicationErr error
	if readyErr == nil {
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
	for _, loss := range result.Value.ObservationLosses {
		a.observeEngineShare(observations, engine.ShareLossSnapshot(loss))
	}
	if result.Observations.CapacityDropped > 0 {
		runtime.ReportObserverLoss(appobs.ObserverLossEngineObservation, appobs.ObserverLossStreamCapacity, result.Observations.CapacityDropped)
	}
	if waitErr != nil {
		emitShareCommandFailure(runtime, ExitFailure, waitErr)
		return ExitFailure
	}
	return reportShareResult(runtime, result, publicationErr)
}

func (a *App) engineShareRequest(request shareRequest, runtime *commandRuntime) engine.ShareRequest {
	return engine.ShareRequest{
		Source: shareFileSource(request.paths), Relays: slices.Clone(request.relays), ChunkSize: request.chunkSize,
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

func (a *App) publishEngineShare(ready engine.ShareReady, presentation shareLinkPresentation, runtime *commandRuntime, observations *shareObservations) error {
	payload, err := buildShareCapabilityPayload(ready.Capability, presentation)
	if err != nil {
		return errors.Join(errShareCapabilityEncoding, err)
	}
	defer clear(payload)
	if err := publishShareCapability(a.Stdout, payload); err != nil {
		return errors.Join(errShareCapabilityOutput, err)
	}
	a.recordProcessTrace(processTraceShareComponent, processTraceSenderReady, testrun.OutcomeSucceeded)
	if err := a.processTrace.err(); err != nil {
		return err
	}
	authority := ready.RelayAuthority
	observations.SetRelayAuthority(authority)
	subject, err := commandprojection.ProjectSharingSubject(ready.SelectedRootSummary)
	if err != nil {
		return err
	}
	connected, err := clievent.NewRelayConnected(clievent.CommandShare, authority)
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
