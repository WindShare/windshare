package cli

import (
	"github.com/windshare/windshare/engine"
	appobs "github.com/windshare/windshare/engine/observation"
)

func (a *App) observeEngineShare(o *shareObservations, event engine.Event) {
	value, ok := event.(appobs.Event)
	if !ok {
		return
	}
	if _, ok := value.(appobs.EngineTaskObserved); ok {
		return
	}
	if loss, ok := value.(appobs.ObserverLossObserved); ok {
		if rejection, present := loss.Rejection(); present {
			o.observer.ReportObservationRejection(loss.Category(), loss.Reason(), rejection)
		} else {
			o.observer.ReportObserverLoss(loss.Category(), loss.Reason(), loss.Count())
		}
		return
	}
	if err := value.Validate(); err != nil {
		o.observer.ReportObserverLoss(appobs.ObserverLossCommandAdapter, appobs.ObserverLossEventContract, 1)
		return
	}
	o.observer.Observe(value)
}
