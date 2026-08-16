package cli

import (
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/internal/testrun"
)

func (a *App) newSenderPeerFactory() (*v2peer.Factory, error) {
	return v2peer.NewFactory(v2peer.Config{
		Configuration: v2peer.DefaultConfiguration(),
		Observer:      v2peer.SenderAttemptObserverFunc(a.observeSenderPeerAttempt),
		OnError: func(error) {
			a.logf("share: direct peer lane failed; relay service remains available")
		},
	})
}

func (a *App) observeSenderPeerAttempt(observation v2peer.SenderAttemptObservation) {
	if observation.Stage != v2peer.SenderAttemptAdmitted || observation.Lane == nil {
		return
	}
	// SenderAttemptAdmitted is emitted only after the authenticated runtime owns
	// the attached lane, making it the sender-side counterpart to receiver Ready.
	a.recordProcessTrace(
		processTraceShareComponent,
		processTraceSenderDirectLane,
		testrun.OutcomeSucceeded,
	)
}
