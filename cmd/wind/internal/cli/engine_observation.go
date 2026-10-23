package cli

import (
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/engine"
	appobs "github.com/windshare/windshare/engine/observation"
)

func observeEngineTask(runtime *commandRuntime, _ clievent.Command, value engine.Observation) {
	if event, ok := value.Event.(appobs.EngineTaskObserved); ok && runtime != nil {
		runtime.Observe(clievent.EngineTaskObserved{EngineTaskObserved: event})
	}
}
func observeEngineFact(runtime *commandRuntime, fact appobs.Event) {
	if runtime == nil {
		return
	}
	if _, ok := fact.(appobs.EngineTaskObserved); ok {
		return
	}
	if loss, ok := fact.(appobs.ObserverLossObserved); ok {
		if rejection, present := loss.Rejection(); present {
			runtime.ReportObservationRejection(loss.Category(), loss.Reason(), rejection)
		} else {
			runtime.ReportObserverLoss(loss.Category(), loss.Reason(), loss.Count())
		}
		return
	}
	event, err := clievent.FromObservation(fact)
	if err != nil {
		runtime.ReportObserverLoss(appobs.ObserverLossCommandAdapter, appobs.ObserverLossEventContract, 1)
		return
	}
	switch fact.(type) {
	case appobs.RelayConnected, appobs.RelayRecovering, appobs.ContentPathSelected, appobs.Fallback:
		runtime.Publish(event)
	default:
		runtime.Observe(event)
	}
}
