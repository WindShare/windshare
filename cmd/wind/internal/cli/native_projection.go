package cli

import (
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/internal/testrun"
)

func (a *App) observeSenderPeerAttempt(observation v2peer.SenderAttemptObservation) {
	if observation.Stage != v2peer.SenderAttemptAdmitted || observation.Lane == nil || observation.Route != transfer.LaneRouteDirect {
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
