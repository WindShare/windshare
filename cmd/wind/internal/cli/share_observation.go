package cli

import (
	"context"
	"sync"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/commandprojection"
	"github.com/windshare/windshare/cmd/wind/internal/observationbridge"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/revisioncapacity"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/transport/relayv2"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
)

type shareEventObserver interface {
	Observe(clievent.Event) bool
	ReportObserverLoss(clievent.ObserverLossCategory, clievent.ObserverLossReason, uint64) bool
}

type shareCommandPublisher interface {
	Publish(...clievent.Event) bool
}

type shareCommandFinalizer interface {
	StageFinalization(clievent.TerminalEvent) bool
}

type detailedDiagnosticsPreference interface {
	detailedDiagnosticsEnabled() bool
}

type traceRecordingPreference interface {
	traceRecordingEnabled() bool
}

type shareObservations struct {
	observer shareEventObserver

	relayMu        sync.RWMutex
	relayAuthority clievent.RelayAuthority

	completionMu         sync.Mutex
	relayComplete        []func() relayv2.LifecycleObservationCompletion
	relayReaders         []*observationbridge.Reader[relayv2.LifecycleTrace]
	webRTCChannels       *webRTCObservationSet
	peerFactory          *v2peer.Factory
	peerAttemptReader    *observationbridge.Reader[v2peer.SenderAttemptObservation]
	peerDiagnosticReader *observationbridge.Reader[v2peer.PeerDiagnosticObservation]
	completeOnce         sync.Once

	losses *observationbridge.CumulativeLosses[observerLossSource]
}

func newShareObservations(observer shareEventObserver) *shareObservations {
	observations := &shareObservations{observer: observer}
	if preference, ok := observer.(detailedDiagnosticsPreference); ok && preference.detailedDiagnosticsEnabled() {
		observations.webRTCChannels = &webRTCObservationSet{}
		observations.losses = observationbridge.NewCumulativeLosses[observerLossSource](observer)
	}
	return observations
}

func (observations *shareObservations) SetRelayAuthority(authority clievent.RelayAuthority) {
	if observations == nil || !authority.Valid() {
		return
	}
	observations.relayMu.Lock()
	observations.relayAuthority = authority
	observations.relayMu.Unlock()
}

func (observations *shareObservations) RelayAuthority() clievent.RelayAuthority {
	if observations == nil {
		return clievent.RelayAuthority{}
	}
	observations.relayMu.RLock()
	defer observations.relayMu.RUnlock()
	return observations.relayAuthority
}

func (observations *shareObservations) TraceCatalogStorage(value liveshare.CatalogStorageTrace) {
	event, err := commandprojection.ProjectCatalogStorage(value)
	observations.emitProjected(clievent.ObserverLossCatalogStorage, event, err)
}

func (observations *shareObservations) TraceRootPrefetch(value liveshare.RootPrefetchTrace) {
	event, err := commandprojection.ProjectRootPrefetch(value)
	observations.emitProjected(clievent.ObserverLossRootPrefetch, event, err)
}

func (observations *shareObservations) TraceCapacity(value revisioncapacity.TraceEvent) {
	event, err := commandprojection.ProjectSenderCapacity(value)
	observations.emitProjected(clievent.ObserverLossSenderCapacity, event, err)
}

func (observations *shareObservations) TraceRevision(value content.RevisionTrace) {
	event, err := commandprojection.ProjectSenderRevision(value)
	observations.emitProjected(clievent.ObserverLossSenderRevision, event, err)
}

func (observations *shareObservations) revisionTracer() content.RevisionTracer {
	if !observations.detailedDiagnosticsEnabled() {
		return nil
	}
	return observations
}

func (observations *shareObservations) capacityTracer() revisioncapacity.Tracer {
	if !observations.detailedDiagnosticsEnabled() {
		return nil
	}
	return observations
}

func (observations *shareObservations) TraceRelayLifecycle(value relayv2.LifecycleTrace) {
	observations.relayLifecycleContext(context.Background(), nil, value)
}

func (observations *shareObservations) TraceRelayLifecycleContext(ctx context.Context, value relayv2.LifecycleTrace) {
	observations.relayLifecycleContext(ctx, nil, value)
}

func (observations *shareObservations) relayLifecycleContext(
	ctx context.Context,
	gate *observationbridge.PublicationGate,
	value relayv2.LifecycleTrace,
) {
	if observations == nil || observations.observer == nil || ctx.Err() != nil {
		return
	}
	event, err := commandprojection.ProjectRelayLifecycle(clievent.CommandShare, value)
	gate.Commit(ctx, func() bool {
		if err != nil {
			observations.projectionFailed(clievent.ObserverLossRelayLifecycle, err)
			return false
		}
		accepted := observations.observer.Observe(event)
		if value.Stage == relayv2.LifecycleTraceDropped {
			observations.reportCumulativeLoss(observerLossRelayQueue, clievent.ObserverLossRelayLifecycle, clievent.ObserverLossTraceQueue, value.Dropped)
		}
		return accepted
	})
}

func (observations *shareObservations) TraceWebRTCLifecycle(value wsrtc.LifecycleTrace) {
	observations.webRTCLifecycleContext(context.Background(), nil, value)
}

func (observations *shareObservations) TraceWebRTCLifecycleContext(ctx context.Context, value wsrtc.LifecycleTrace) {
	observations.webRTCLifecycleContext(ctx, nil, value)
}

func (observations *shareObservations) webRTCLifecycleContext(
	ctx context.Context,
	gate *observationbridge.PublicationGate,
	value wsrtc.LifecycleTrace,
) {
	if observations == nil || observations.observer == nil || ctx.Err() != nil {
		return
	}
	event, err := commandprojection.ProjectWebRTCLifecycle(clievent.CommandShare, value)
	gate.Commit(ctx, func() bool {
		if err != nil {
			observations.projectionFailed(clievent.ObserverLossWebRTCLifecycle, err)
			return false
		}
		accepted := observations.observer.Observe(event)
		if value.Transition == wsrtc.LifecycleTransitionTraceDropped {
			observations.reportCumulativeLoss(observerLossWebRTCQueue, clievent.ObserverLossWebRTCLifecycle, clievent.ObserverLossTraceQueue, value.Dropped)
		}
		return accepted
	})
}

func (observations *shareObservations) ObserveSenderAttempt(value v2peer.SenderAttemptObservation) {
	observations.senderAttemptContext(context.Background(), nil, value)
}

func (observations *shareObservations) ObserveSenderAttemptContext(ctx context.Context, value v2peer.SenderAttemptObservation) {
	observations.senderAttemptContext(ctx, nil, value)
}

func (observations *shareObservations) senderAttemptContext(
	ctx context.Context,
	gate *observationbridge.PublicationGate,
	value v2peer.SenderAttemptObservation,
) {
	if observations == nil || observations.observer == nil || ctx.Err() != nil {
		return
	}
	event, err := commandprojection.ProjectSenderAttempt(clievent.CommandShare, value)
	observations.emitProjectedContext(ctx, gate, clievent.ObserverLossSenderAttempt, event, err)
}

func (observations *shareObservations) ObserveSenderTerminalSend(
	value sessionruntime.SenderTerminalSendObserved,
) {
	event, err := commandprojection.ProjectSenderTerminalSend(value)
	observations.emitProjected(clievent.ObserverLossSenderTerminalSend, event, err)
}

func (observations *shareObservations) ObserveSenderSessionTerminated(
	value sessionruntime.SenderSessionTerminated,
) {
	event, err := commandprojection.ProjectSenderSessionTerminated(value)
	observations.emitProjected(clievent.ObserverLossSenderSessionTerminal, event, err)
}

func (observations *shareObservations) TraceProtocolOperation(value sessionruntime.ProtocolOperationTrace) {
	event, err := commandprojection.ProjectProtocolOperation(clievent.CommandShare, value)
	observations.emitProjected(clievent.ObserverLossProtocolOperation, event, err)
}

func (observations *shareObservations) protocolTracer() sessionruntime.ProtocolOperationTracer {
	if !observations.detailedDiagnosticsEnabled() {
		return nil
	}
	return observations
}

func (observations *shareObservations) terminalSendObserver() sessionruntime.SenderTerminalSendObserver {
	if !observations.traceRecordingEnabled() {
		return nil
	}
	return observations
}

func (observations *shareObservations) sessionTerminalObserver() sessionruntime.SenderSessionTerminalObserver {
	if !observations.traceRecordingEnabled() {
		return nil
	}
	return observations
}

func (observations *shareObservations) ObserveRelayRecovery(value senderRelayRecoveryAttempt) {
	var state clievent.RelayRecoveryState
	switch value.state {
	case senderRelayAttemptStarted:
		state = clievent.RelayRecoveryStarted
	case senderRelayAttemptSucceeded:
		state = clievent.RelayRecoverySucceeded
	case senderRelayAttemptFailed:
		state = clievent.RelayRecoveryFailed
	default:
		observations.projectionFailed(clievent.ObserverLossCommandAdapter, commandprojection.ErrInvalidProjection)
		return
	}
	var failure clievent.Failure
	if state == clievent.RelayRecoveryFailed {
		failure, _ = commandprojection.ClassifyError(value.failure)
		if !failure.Valid() {
			failure = mustShareFailure(clievent.FailureUnexpected)
		}
	}
	event, err := clievent.NewRelayRecovering(
		clievent.CommandShare,
		observations.RelayAuthority(),
		value.attempt,
		state,
		failure,
	)
	observations.emitProjected(clievent.ObserverLossCommandAdapter, event, err)
}

func (observations *shareObservations) emitProjected(category clievent.ObserverLossCategory, event clievent.Event, err error) {
	observations.emitProjectedContext(context.Background(), nil, category, event, err)
}

func (observations *shareObservations) emitProjectedContext(
	ctx context.Context,
	gate *observationbridge.PublicationGate,
	category clievent.ObserverLossCategory,
	event clievent.Event,
	err error,
) bool {
	if observations == nil || observations.observer == nil {
		return false
	}
	return gate.Commit(ctx, func() bool {
		if err != nil || event == nil {
			observations.projectionFailed(category, err)
			return false
		}
		return observations.observer.Observe(event)
	})
}

func (observations *shareObservations) projectionFailed(category clievent.ObserverLossCategory, cause error) {
	if observations == nil || observations.observer == nil {
		return
	}
	observations.observer.ReportObserverLoss(category, commandprojection.ObserverLossReason(cause), 1)
}

func (observations *shareObservations) detailedDiagnosticsEnabled() bool {
	if observations == nil || observations.observer == nil {
		return false
	}
	preference, ok := observations.observer.(detailedDiagnosticsPreference)
	return ok && preference.detailedDiagnosticsEnabled()
}

func (observations *shareObservations) traceRecordingEnabled() bool {
	if observations == nil || observations.observer == nil {
		return false
	}
	preference, ok := observations.observer.(traceRecordingPreference)
	return ok && preference.traceRecordingEnabled()
}

func (observations *shareObservations) relayObservationCapacity() int {
	if !observations.detailedDiagnosticsEnabled() {
		return 0
	}
	return relayv2.DefaultLifecycleObservationCapacity
}

func (observations *shareObservations) ObservePeerDiagnostic(value v2peer.PeerDiagnosticObservation) {
	observations.peerDiagnosticContext(context.Background(), nil, value)
}

func (observations *shareObservations) ObservePeerDiagnosticContext(ctx context.Context, value v2peer.PeerDiagnosticObservation) {
	observations.peerDiagnosticContext(ctx, nil, value)
}

func (observations *shareObservations) peerDiagnosticContext(
	ctx context.Context,
	gate *observationbridge.PublicationGate,
	value v2peer.PeerDiagnosticObservation,
) {
	if observations == nil || observations.observer == nil {
		return
	}
	if ctx.Err() != nil {
		return
	}
	category, reason, cumulative, err := commandprojection.ProjectPeerDiagnostic(value)
	gate.Commit(ctx, func() bool {
		if err != nil {
			observations.projectionFailed(clievent.ObserverLossCommandAdapter, err)
			return false
		}
		observations.reportCumulativeLoss(peerDiagnosticLossSource(value), category, reason, cumulative)
		return true
	})
}

func (observations *shareObservations) reportCumulativeLoss(
	source observerLossSource,
	category clievent.ObserverLossCategory,
	reason clievent.ObserverLossReason,
	cumulative uint64,
) {
	if observations == nil || observations.observer == nil {
		return
	}
	if observations.losses != nil {
		observations.losses.Report(source, category, reason, cumulative)
		return
	}
	observations.observer.ReportObserverLoss(category, reason, cumulative)
}

func (observations *shareObservations) attachRelayStream(stream <-chan relayv2.LifecycleTrace) {
	if observations == nil || !observations.detailedDiagnosticsEnabled() || stream == nil {
		return
	}
	gate := &observationbridge.PublicationGate{}
	reader := observationbridge.Start(stream, gate, func(ctx context.Context, value relayv2.LifecycleTrace) {
		observations.relayLifecycleContext(ctx, gate, value)
	})
	observations.completionMu.Lock()
	observations.relayReaders = append(observations.relayReaders, reader)
	observations.completionMu.Unlock()
}

func (observations *shareObservations) registerRelayCompletion(
	complete func() relayv2.LifecycleObservationCompletion,
) {
	if observations == nil || !observations.detailedDiagnosticsEnabled() || complete == nil {
		return
	}
	observations.completionMu.Lock()
	observations.relayComplete = append(observations.relayComplete, complete)
	observations.completionMu.Unlock()
}

func (observations *shareObservations) registerPeerFactory(
	factory *v2peer.Factory,
	process func(v2peer.SenderAttemptObservation),
) {
	if observations == nil || factory == nil || factory.SenderAttemptObservations() == nil && factory.PeerDiagnostics() == nil {
		return
	}
	attemptGate := &observationbridge.PublicationGate{}
	attemptReader := observationbridge.Start(
		factory.SenderAttemptObservations(),
		attemptGate,
		func(ctx context.Context, value v2peer.SenderAttemptObservation) {
			if observations.detailedDiagnosticsEnabled() {
				observations.senderAttemptContext(ctx, attemptGate, value)
			}
			if ctx.Err() == nil && process != nil {
				process(value)
			}
		},
	)
	diagnosticGate := &observationbridge.PublicationGate{}
	diagnosticReader := observationbridge.Start(
		factory.PeerDiagnostics(),
		diagnosticGate,
		func(ctx context.Context, value v2peer.PeerDiagnosticObservation) {
			observations.peerDiagnosticContext(ctx, diagnosticGate, value)
		},
	)
	observations.completionMu.Lock()
	observations.peerFactory = factory
	observations.peerAttemptReader = attemptReader
	observations.peerDiagnosticReader = diagnosticReader
	observations.completionMu.Unlock()
}

func (observations *shareObservations) completeWithin() {
	ctx, cancel := context.WithTimeout(context.Background(), observationCompletionTimeout)
	observations.complete(ctx)
	cancel()
}

func (observations *shareObservations) complete(ctx context.Context) {
	if observations == nil {
		return
	}
	observations.completeOnce.Do(func() {
		observations.completionMu.Lock()
		relayComplete := append([]func() relayv2.LifecycleObservationCompletion(nil), observations.relayComplete...)
		relayReaders := append([]*observationbridge.Reader[relayv2.LifecycleTrace](nil), observations.relayReaders...)
		webRTC := observations.webRTCChannels
		peers := observations.peerFactory
		peerAttemptReader := observations.peerAttemptReader
		peerDiagnosticReader := observations.peerDiagnosticReader
		observations.completionMu.Unlock()

		var relay relayv2.LifecycleObservationCompletion
		for _, complete := range relayComplete {
			mergeRelayCompletion(&relay, complete())
		}
		for _, reader := range relayReaders {
			status := reader.Join(ctx)
			observations.reportReaderStatus(clievent.ObserverLossRelayLifecycle, status)
		}
		observations.reportRelayCompletion(relay)
		webRTCCompletion, webRTCStatuses := webRTC.complete(ctx)
		observations.reportWebRTCCompletion(webRTCCompletion)
		for _, status := range webRTCStatuses {
			observations.reportReaderStatus(clievent.ObserverLossWebRTCLifecycle, status)
		}
		if peers != nil {
			completion := peers.CompleteObservations()
			attemptStatus := peerAttemptReader.Join(ctx)
			diagnosticStatus := peerDiagnosticReader.Join(ctx)
			observations.reportSenderCompletion(completion)
			observations.reportReaderStatus(clievent.ObserverLossSenderAttempt, attemptStatus)
			observations.reportReaderStatus(clievent.ObserverLossSenderAttempt, diagnosticStatus)
		}
	})
}

func (observations *shareObservations) reportRelayCompletion(completion relayv2.LifecycleObservationCompletion) {
	observations.reportCumulativeLoss(observerLossRelayQueue, clievent.ObserverLossRelayLifecycle, clievent.ObserverLossStreamCapacity, completion.Loss.CapacityDropped)
}

func (observations *shareObservations) reportWebRTCCompletion(completion wsrtc.LifecycleObservationCompletion) {
	observations.reportCumulativeLoss(observerLossWebRTCQueue, clievent.ObserverLossWebRTCLifecycle, clievent.ObserverLossStreamCapacity, completion.Loss.CapacityDropped)
}

func (observations *shareObservations) reportSenderCompletion(completion v2peer.SenderObservationCompletion) {
	observations.reportCumulativeLoss(observerLossSenderAttemptCapacity, clievent.ObserverLossSenderAttempt, clievent.ObserverLossStreamCapacity, completion.Attempts.Loss.CapacityDropped)
	observations.reportCumulativeLoss(observerLossSenderDiagnosticDrain, clievent.ObserverLossSenderAttempt, clievent.ObserverLossStreamCapacity, completion.Diagnostics.Loss.CapacityDropped)
}

func mergeRelayCompletion(total *relayv2.LifecycleObservationCompletion, next relayv2.LifecycleObservationCompletion) {
	total.Enqueued = saturatingAdd(total.Enqueued, next.Enqueued)
	total.Loss.CapacityDropped = saturatingAdd(total.Loss.CapacityDropped, next.Loss.CapacityDropped)
}

func (observations *shareObservations) reportReaderStatus(
	category clievent.ObserverLossCategory,
	status observationbridge.Status,
) {
	if observations == nil || observations.observer == nil || status.Joined {
		return
	}
	residue := status.Buffered
	if status.Active {
		residue = saturatingAdd(residue, 1)
	}
	if residue == 0 {
		residue = 1
	}
	observations.observer.ReportObserverLoss(category, clievent.ObserverLossReaderNotJoined, residue)
}

func mustShareFailure(code clievent.FailureCode) clievent.Failure {
	failure, err := clievent.NewFailure(code)
	if err != nil {
		panic("cli: invalid internal share failure code")
	}
	return failure
}
