package cli

import (
	"context"
	"sync"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
	"github.com/windshare/windshare/cmd/windshare/internal/commandprojection"
	"github.com/windshare/windshare/connectivity/v2peer"
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
	StageFinalization(...clievent.Event) bool
}

type detailedDiagnosticsPreference interface {
	detailedDiagnosticsEnabled() bool
}

type shareObservations struct {
	observer shareEventObserver

	relayMu        sync.RWMutex
	relayAuthority clievent.RelayAuthority

	completionMu   sync.Mutex
	relayComplete  []func(context.Context) relayv2.LifecycleObservationCompletion
	webRTCChannels *webRTCObservationSet
	peerFactory    *v2peer.Factory
	relayGate      *observationPublicationGate
	webRTCGate     *observationPublicationGate
	peerGate       *observationPublicationGate
	completeOnce   sync.Once

	losses *observerLossAccumulator
}

func newShareObservations(observer shareEventObserver) *shareObservations {
	observations := &shareObservations{observer: observer}
	if preference, ok := observer.(detailedDiagnosticsPreference); ok && preference.detailedDiagnosticsEnabled() {
		observations.webRTCChannels = &webRTCObservationSet{}
		observations.losses = newObserverLossAccumulator(observer)
		observations.relayGate = &observationPublicationGate{}
		observations.webRTCGate = &observationPublicationGate{}
		observations.peerGate = &observationPublicationGate{}
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

func (observations *shareObservations) TraceRelayLifecycle(value relayv2.LifecycleTrace) {
	observations.TraceRelayLifecycleContext(context.Background(), value)
}

func (observations *shareObservations) TraceRelayLifecycleContext(ctx context.Context, value relayv2.LifecycleTrace) {
	if observations == nil || observations.observer == nil || ctx.Err() != nil {
		return
	}
	event, err := commandprojection.ProjectRelayLifecycle(clievent.CommandShare, value)
	observations.relayGate.commit(ctx, func() bool {
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
	observations.TraceWebRTCLifecycleContext(context.Background(), value)
}

func (observations *shareObservations) TraceWebRTCLifecycleContext(ctx context.Context, value wsrtc.LifecycleTrace) {
	if observations == nil || observations.observer == nil || ctx.Err() != nil {
		return
	}
	event, err := commandprojection.ProjectWebRTCLifecycle(clievent.CommandShare, value)
	observations.webRTCGate.commit(ctx, func() bool {
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
	observations.ObserveSenderAttemptContext(context.Background(), value)
}

func (observations *shareObservations) ObserveSenderAttemptContext(ctx context.Context, value v2peer.SenderAttemptObservation) {
	if observations == nil || observations.observer == nil || ctx.Err() != nil {
		return
	}
	event, err := commandprojection.ProjectSenderAttempt(clievent.CommandShare, value)
	observations.emitProjectedContext(ctx, observations.peerGate, clievent.ObserverLossSenderAttempt, event, err)
}

func (observations *shareObservations) ObserveSenderTerminal(value sessionruntime.SenderTerminalObservation) {
	event, err := commandprojection.ProjectSenderTerminal(value)
	observations.emitProjected(clievent.ObserverLossProtocolOperation, event, err)
}

func (observations *shareObservations) TraceProtocolOperation(value sessionruntime.ProtocolOperationTrace) {
	event, err := commandprojection.ProjectProtocolOperation(clievent.CommandShare, value)
	observations.emitProjected(clievent.ObserverLossProtocolOperation, event, err)
}

func (observations *shareObservations) protocolTracer() sessionruntime.ProtocolOperationTracer {
	if observations == nil || observations.observer == nil {
		return nil
	}
	preference, ok := observations.observer.(detailedDiagnosticsPreference)
	if !ok || !preference.detailedDiagnosticsEnabled() {
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
	gate *observationPublicationGate,
	category clievent.ObserverLossCategory,
	event clievent.Event,
	err error,
) bool {
	if observations == nil || observations.observer == nil {
		return false
	}
	return gate.commit(ctx, func() bool {
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

func (observations *shareObservations) relayTracer() relayv2.LifecycleTracer {
	if !observations.detailedDiagnosticsEnabled() {
		return nil
	}
	return observations
}

func (observations *shareObservations) webRTCTracer() wsrtc.LifecycleTracer {
	if !observations.detailedDiagnosticsEnabled() {
		return nil
	}
	return observations
}

func (observations *shareObservations) senderAttemptObserver() v2peer.SenderAttemptObserver {
	if !observations.detailedDiagnosticsEnabled() {
		return nil
	}
	return observations
}

func (observations *shareObservations) ObservePeerDiagnostic(value v2peer.PeerDiagnosticObservation) {
	observations.ObservePeerDiagnosticContext(context.Background(), value)
}

func (observations *shareObservations) ObservePeerDiagnosticContext(ctx context.Context, value v2peer.PeerDiagnosticObservation) {
	if observations == nil || observations.observer == nil {
		return
	}
	if ctx.Err() != nil {
		return
	}
	category, reason, cumulative, err := commandprojection.ProjectPeerDiagnostic(value)
	observations.peerGate.commit(ctx, func() bool {
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
		observations.losses.report(source, category, reason, cumulative)
		return
	}
	observations.observer.ReportObserverLoss(category, reason, cumulative)
}

func (observations *shareObservations) registerRelayCompletion(
	complete func(context.Context) relayv2.LifecycleObservationCompletion,
) {
	if observations == nil || !observations.detailedDiagnosticsEnabled() || complete == nil {
		return
	}
	observations.completionMu.Lock()
	observations.relayComplete = append(observations.relayComplete, complete)
	observations.completionMu.Unlock()
}

func (observations *shareObservations) registerPeerFactory(factory *v2peer.Factory) {
	if observations == nil || !observations.detailedDiagnosticsEnabled() || factory == nil {
		return
	}
	observations.completionMu.Lock()
	observations.peerFactory = factory
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
		relayComplete := append([]func(context.Context) relayv2.LifecycleObservationCompletion(nil), observations.relayComplete...)
		webRTC := observations.webRTCChannels
		peers := observations.peerFactory
		observations.completionMu.Unlock()

		var relay relayv2.LifecycleObservationCompletion
		relay.Drained = true
		for _, complete := range relayComplete {
			mergeRelayCompletion(&relay, complete(ctx))
		}
		observations.relayGate.revoke()
		observations.reportRelayCompletion(relay)
		webRTCCompletion := webRTC.complete(ctx)
		observations.webRTCGate.revoke()
		observations.reportWebRTCCompletion(webRTCCompletion)
		if peers != nil {
			completion := peers.CompleteObservations(ctx)
			observations.peerGate.revoke()
			observations.reportSenderCompletion(completion)
		} else {
			observations.peerGate.revoke()
		}
	})
}

func (observations *shareObservations) reportRelayCompletion(completion relayv2.LifecycleObservationCompletion) {
	observations.reportCumulativeLoss(observerLossRelayQueue, clievent.ObserverLossRelayLifecycle, clievent.ObserverLossTraceQueue, completion.Loss.QueueOverflow)
	observations.reportCumulativeLoss(observerLossRelayPanic, clievent.ObserverLossRelayLifecycle, clievent.ObserverLossEventContract, completion.Loss.ObserverPanic)
	observations.reportCumulativeLoss(observerLossRelayDrain, clievent.ObserverLossRelayLifecycle, clievent.ObserverLossAdapterCapacityTimeout,
		observationDrainLoss(completion.Loss.CallbackTimeout, completion.Loss.Undrained, completion.Drained))
}

func (observations *shareObservations) reportWebRTCCompletion(completion wsrtc.LifecycleObservationCompletion) {
	observations.reportCumulativeLoss(observerLossWebRTCQueue, clievent.ObserverLossWebRTCLifecycle, clievent.ObserverLossTraceQueue, completion.Loss.QueueOverflow)
	observations.reportCumulativeLoss(observerLossWebRTCPanic, clievent.ObserverLossWebRTCLifecycle, clievent.ObserverLossEventContract, completion.Loss.ObserverPanic)
	observations.reportCumulativeLoss(observerLossWebRTCDrain, clievent.ObserverLossWebRTCLifecycle, clievent.ObserverLossAdapterCapacityTimeout,
		observationDrainLoss(completion.Loss.CallbackTimeout, completion.Loss.Undrained, completion.Drained))
}

func (observations *shareObservations) reportSenderCompletion(completion v2peer.SenderObservationCompletion) {
	observations.reportCumulativeLoss(observerLossSenderAttemptCapacity, clievent.ObserverLossSenderAttempt, clievent.ObserverLossAdapterCapacityTimeout, completion.Attempts.Loss.Capacity)
	observations.reportCumulativeLoss(observerLossSenderAttemptPanic, clievent.ObserverLossSenderAttempt, clievent.ObserverLossEventContract, completion.Attempts.Loss.ObserverPanic)
	observations.reportCumulativeLoss(observerLossSenderAttemptDrain, clievent.ObserverLossSenderAttempt, clievent.ObserverLossAdapterCapacityTimeout,
		observationDrainLoss(completion.Attempts.Loss.CallbackTimeout, completion.Attempts.Loss.Undrained, completion.Attempts.Drained))
	observations.reportCumulativeLoss(observerLossSenderDiagnosticPanic, clievent.ObserverLossSenderAttempt, clievent.ObserverLossEventContract, completion.Diagnostics.Loss.ObserverPanic)
	observations.reportCumulativeLoss(observerLossSenderDiagnosticDrain, clievent.ObserverLossSenderAttempt, clievent.ObserverLossAdapterCapacityTimeout,
		observationDrainLoss(
			saturatingAdd(completion.Diagnostics.Loss.Capacity, completion.Diagnostics.Loss.CallbackTimeout),
			completion.Diagnostics.Loss.Undrained,
			completion.Diagnostics.Drained,
		))
}

func mergeRelayCompletion(total *relayv2.LifecycleObservationCompletion, next relayv2.LifecycleObservationCompletion) {
	total.Delivered = saturatingAdd(total.Delivered, next.Delivered)
	total.Loss.QueueOverflow = saturatingAdd(total.Loss.QueueOverflow, next.Loss.QueueOverflow)
	total.Loss.ObserverPanic = saturatingAdd(total.Loss.ObserverPanic, next.Loss.ObserverPanic)
	total.Loss.CallbackTimeout = saturatingAdd(total.Loss.CallbackTimeout, next.Loss.CallbackTimeout)
	total.Loss.Undrained = saturatingAdd(total.Loss.Undrained, next.Loss.Undrained)
	total.Drained = total.Drained && next.Drained
}

func mustShareFailure(code clievent.FailureCode) clievent.Failure {
	failure, err := clievent.NewFailure(code)
	if err != nil {
		panic("cli: invalid internal share failure code")
	}
	return failure
}
