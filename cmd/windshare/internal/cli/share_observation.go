package cli

import (
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
	ReportObserverLoss(lifecycle, progress uint64) bool
}

type shareCommandPublisher interface {
	Publish(...clievent.Event) bool
}

type shareObservations struct {
	observer shareEventObserver

	relayMu        sync.RWMutex
	relayAuthority clievent.RelayAuthority

	projectionFailureOnce sync.Once
}

func newShareObservations(observer shareEventObserver) *shareObservations {
	return &shareObservations{observer: observer}
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
	observations.emitProjected(event, err)
}

func (observations *shareObservations) TraceRootPrefetch(value liveshare.RootPrefetchTrace) {
	event, err := commandprojection.ProjectRootPrefetch(value)
	observations.emitProjected(event, err)
}

func (observations *shareObservations) TraceRelayLifecycle(value relayv2.LifecycleTrace) {
	event, err := commandprojection.ProjectRelayLifecycle(clievent.CommandShare, value)
	observations.emitProjected(event, err)
}

func (observations *shareObservations) TraceWebRTCLifecycle(value wsrtc.LifecycleTrace) {
	event, err := commandprojection.ProjectWebRTCLifecycle(clievent.CommandShare, value)
	observations.emitProjected(event, err)
}

func (observations *shareObservations) ObserveSenderAttempt(value v2peer.SenderAttemptObservation) {
	event, err := commandprojection.ProjectSenderAttempt(clievent.CommandShare, value)
	observations.emitProjected(event, err)
}

func (observations *shareObservations) ObserveSenderTerminal(value sessionruntime.SenderTerminalObservation) {
	event, err := commandprojection.ProjectSenderTerminal(value)
	observations.emitProjected(event, err)
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
		observations.projectionFailed()
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
	observations.emitProjected(event, err)
}

func (observations *shareObservations) ObservePeerFallback(cause error) {
	failure, _ := commandprojection.ClassifyError(cause)
	if !failure.Valid() {
		failure = mustShareFailure(clievent.FailureUnexpected)
	}
	event, err := clievent.NewFallback(
		clievent.CommandShare,
		clievent.TransportWebRTC,
		clievent.TransportRelay,
		failure,
	)
	observations.emitProjected(event, err)
}

func (observations *shareObservations) emitProjected(event clievent.Event, err error) {
	if observations == nil || observations.observer == nil {
		return
	}
	if err != nil || event == nil {
		observations.projectionFailed()
		return
	}
	observations.observer.Observe(event)
}

func (observations *shareObservations) projectionFailed() {
	if observations == nil || observations.observer == nil {
		return
	}
	observations.projectionFailureOnce.Do(func() {
		observations.observer.ReportObserverLoss(1, 0)
		warning, err := clievent.NewWarning(
			clievent.CommandShare,
			mustShareFailure(clievent.FailureUnexpected),
		)
		if err == nil {
			observations.observer.Observe(warning)
		}
	})
}

func mustShareFailure(code clievent.FailureCode) clievent.Failure {
	failure, err := clievent.NewFailure(code)
	if err != nil {
		panic("cli: invalid internal share failure code")
	}
	return failure
}
