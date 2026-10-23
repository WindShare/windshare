package cli

import (
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/engine"
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/internal/testrun"
)

func (a *App) observeEngineShare(o *shareObservations, event engine.Event) {
	switch value := event.(type) {
	case appobs.Event:
		if _, ok := value.(appobs.EngineTaskObserved); ok {
			return
		}
		if attempt, ok := value.(appobs.PeerAttemptObserved); ok {
			a.observeSenderPeerAttempt(attempt)
		}
		if recovery, ok := value.(appobs.RelayRecovering); ok {
			a.observeSenderRelayRecovery(recovery)
		}
		if loss, ok := value.(appobs.ObserverLossObserved); ok {
			if rejection, present := loss.Rejection(); present {
				o.observer.ReportObservationRejection(loss.Category(), loss.Reason(), rejection)
			} else {
				o.observer.ReportObserverLoss(loss.Category(), loss.Reason(), loss.Count())
			}
			return
		}
		wrapped, err := clievent.FromObservation(value)
		if err != nil {
			o.observer.ReportObserverLoss(appobs.ObserverLossCommandAdapter, appobs.ObserverLossEventContract, 1)
			return
		}
		o.observer.Observe(wrapped)
	case appobs.LossSnapshot:
		o.reportCumulativeLoss(value.Source, value.Category, value.Reason, value.Count)
	case engine.ShareMilestoneObserved:
		switch value.Milestone {
		case engine.ShareMilestoneStopping:
			a.recordProcessTrace(processTraceShareComponent, processTraceSenderStop, testrun.OutcomeStarted)
		case engine.ShareMilestoneStopped:
			outcome := testrun.OutcomeSucceeded
			if value.Failed {
				outcome = testrun.OutcomeFailed
			}
			a.recordProcessTrace(processTraceShareComponent, processTraceSenderStop, outcome)
		case engine.ShareMilestoneSessionRetired:
			a.recordProcessTrace(processTraceShareComponent, processTraceSenderSessionRetired, testrun.OutcomeSucceeded)
		}
	}
}
func (a *App) observeSenderRelayRecovery(attempt appobs.RelayRecovering) {
	details, ok := attempt.Details()
	if !ok {
		return
	}
	outcome := testrun.OutcomeFailed
	switch {
	case attempt.State() == appobs.RelayRecoveryStarted && attempt.Attempt() == 1 && details.Generation > 0:
		outcome = testrun.OutcomeStarted
	case attempt.State() == appobs.RelayRecoverySucceeded && details.Generation > 1:
		outcome = testrun.OutcomeSucceeded
	case attempt.State() == appobs.RelayRecoveryFailed && details.Terminal && details.Generation > 0:
	default:
		return
	}
	a.processTrace.record(processTraceShareComponent, processTraceSenderRelayRecovery, outcome, map[string]uint64{"connection_generation": details.Generation})
}
