package cli

import (
	"context"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/commandprojection"
	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/internal/testrun"
)

func (a *App) observeEngineShare(o *shareObservations, event engine.ShareObservation) {
	switch {
	case event.Catalog != nil:
		o.TraceCatalogStorage(*event.Catalog)
	case event.Prefetch != nil:
		o.TraceRootPrefetch(*event.Prefetch)
	case event.Revision != nil:
		o.TraceRevision(*event.Revision)
	case event.RelayAvailability != nil:
		o.ObserveRelayAvailability(*event.RelayAvailability)
	case event.RelayRecovery != nil:
		authority, err := commandprojection.RelayAuthority(event.RelayRecovery.Endpoint)
		if err != nil {
			o.projectionFailed(clievent.ObserverLossCommandAdapter, err)
			return
		}
		o.ObserveRelayRecovery(authority, event.RelayRecovery.Attempt)
		a.observeSenderRelayRecovery(event.RelayRecovery.Attempt)
	case event.RelayLifecycle != nil:
		o.TraceRelayLifecycle(*event.RelayLifecycle)
	case event.SenderAttempt != nil:
		if o.detailedDiagnosticsEnabled() {
			o.ObserveSenderAttempt(*event.SenderAttempt)
		}
		a.observeSenderPeerAttempt(*event.SenderAttempt)
	case event.PeerDiagnostic != nil:
		o.ObservePeerDiagnostic(*event.PeerDiagnostic)
	case event.Native != nil:
		projected, err := projectNativeObservation(clievent.CommandShare, *event.Native)
		o.emitProjected(clievent.ObserverLossNativeConnectivity, projected, err)
	case event.WebRTC != nil:
		o.TraceWebRTCLifecycle(*event.WebRTC)
	case event.Protocol != nil:
		o.protocolObservationContext(context.Background(), nil, *event.Protocol)
	case event.TerminalSend != nil:
		o.ObserveSenderTerminalSend(*event.TerminalSend)
	case event.SessionTerminal != nil:
		o.ObserveSenderSessionTerminated(*event.SessionTerminal)
	case event.Loss != nil:
		source, category := observerLossProtocolQueue, clievent.ObserverLossProtocolOperation
		switch event.Loss.Source {
		case engine.ShareLossProtocol:
		case engine.ShareLossNative:
			source, category = observerLossNativeQueue, clievent.ObserverLossNativeConnectivity
		case engine.ShareLossRelay:
			source, category = observerLossRelayQueue, clievent.ObserverLossRelayLifecycle
		case engine.ShareLossWebRTC:
			source, category = observerLossWebRTCQueue, clievent.ObserverLossWebRTCLifecycle
		case engine.ShareLossSenderAttempt:
			source, category = observerLossSenderAttemptCapacity, clievent.ObserverLossSenderAttempt
		case engine.ShareLossPeerDiagnostic:
			source, category = observerLossSenderDiagnosticDrain, clievent.ObserverLossSenderAttempt
		default:
			o.projectionFailed(clievent.ObserverLossCommandAdapter, commandprojection.ErrInvalidProjection)
			return
		}
		o.reportCumulativeLoss(source, category, clievent.ObserverLossStreamCapacity, event.Loss.Dropped)
	case event.Milestone == engine.ShareMilestoneStopping:
		a.recordProcessTrace(processTraceShareComponent, processTraceSenderStop, testrun.OutcomeStarted)
	case event.Milestone == engine.ShareMilestoneStopped:
		outcome := testrun.OutcomeSucceeded
		if event.Failure != nil {
			outcome = testrun.OutcomeFailed
		}
		a.recordProcessTrace(processTraceShareComponent, processTraceSenderStop, outcome)
	case event.Milestone == engine.ShareMilestoneSessionRetired:
		a.recordProcessTrace(processTraceShareComponent, processTraceSenderSessionRetired, testrun.OutcomeSucceeded)
	}
}

func (a *App) observeSenderRelayRecovery(attempt senderrelay.Attempt) {
	outcome := testrun.OutcomeFailed
	switch {
	case attempt.State == senderrelay.AttemptStarted && attempt.Number == 1 && attempt.Generation > 0:
		outcome = testrun.OutcomeStarted
	case attempt.State == senderrelay.AttemptSucceeded && attempt.Generation > 1:
		outcome = testrun.OutcomeSucceeded
	case attempt.State == senderrelay.AttemptFailed && attempt.Terminal && attempt.Generation > 0:
	default:
		return
	}
	a.processTrace.record(processTraceShareComponent, processTraceSenderRelayRecovery, outcome, struct {
		ConnectionGeneration uint64 `json:"connection_generation"`
	}{attempt.Generation})
}
