package cli

import (
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/internal/testrun"
)

func (a *App) observeSenderPeerAttempt(observation appobs.PeerAttemptObserved) {
	if observation.Stage() != appobs.PeerAttemptAdmitted {
		return
	}
	route, ok := observation.Route()
	if !ok || route != appobs.LaneRouteDirect {
		return
	}
	// SenderAttemptAdmitted is emitted only after authenticated runtime ownership,
	// making it the sender-side synchronization counterpart to receiver Ready.
	a.recordProcessTrace(
		processTraceShareComponent,
		processTraceSenderDirectLane,
		testrun.OutcomeSucceeded,
	)
}
