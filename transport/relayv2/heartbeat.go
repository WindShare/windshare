package relayv2

import (
	"errors"

	"github.com/windshare/windshare/internal/websocketheartbeat"
)

type HeartbeatConfig = websocketheartbeat.Config

var ErrHeartbeat = websocketheartbeat.ErrFailed

func isHeartbeatLifecycleStage(stage LifecycleStage) bool {
	return stage == LifecycleHeartbeatProbe || stage == LifecycleHeartbeatAcknowledged || stage == LifecycleHeartbeatFailed
}

func validateHeartbeatLifecycleTrace(event LifecycleTrace) LifecycleContractViolation {
	if hasLifecycleSession(event) || event.OperationID == 0 || event.HeartbeatRound == 0 {
		return LifecycleContractInvalidIdentity
	}
	if event.Terminal || event.Disposition != 0 || event.Dropped != 0 ||
		event.RetirementSource != LifecycleRetirementNone || event.DrainCause != LifecycleCauseNone ||
		event.Timeout <= 0 || event.Wait < 0 {
		return LifecycleContractInvalidStageFields
	}
	if (event.Stage == LifecycleHeartbeatFailed) != (event.Cause != LifecycleCauseNone) ||
		(event.Stage == LifecycleHeartbeatProbe && event.Wait != 0) {
		return LifecycleContractInvalidStageFields
	}
	return LifecycleContractValid
}

func (l *link) heartbeatLoop() {
	var operationID uint64
	err := websocketheartbeat.Run(l.ctx, l.socket, l.heartbeat, func(event websocketheartbeat.Event) {
		if event.Stage == websocketheartbeat.Probe {
			operationID = l.nextOperationID()
		}
		stage := LifecycleHeartbeatProbe
		if event.Stage == websocketheartbeat.Acknowledged {
			stage = LifecycleHeartbeatAcknowledged
		}
		if event.Stage == websocketheartbeat.Failed {
			stage = LifecycleHeartbeatFailed
		}
		l.trace(LifecycleTrace{
			OperationID: operationID, Stage: stage, Cause: lifecycleCause(event.Cause),
			RetirementSource: LifecycleRetirementNone, DrainCause: LifecycleCauseNone,
			HeartbeatRound: event.Round, Wait: event.Elapsed, Timeout: event.Timeout,
		})
	})
	if errors.Is(err, ErrHeartbeat) {
		l.stop(err)
	}
}
