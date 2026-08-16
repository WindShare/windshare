package cli

import (
	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
	"github.com/windshare/windshare/cmd/windshare/internal/commandprojection"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
	transportwebrtc "github.com/windshare/windshare/transport/webrtc"
)

type getObservation struct {
	runtime *commandRuntime
}

func (observation getObservation) observe(event clievent.Event) bool {
	return observation.runtime != nil && observation.runtime.Observe(event)
}

func (observation getObservation) publish(events ...clievent.Event) bool {
	return observation.runtime != nil && observation.runtime.Publish(events...)
}

func (observation getObservation) finalize(
	progress clievent.TransferProgress,
	settlement clievent.TransferSettled,
) bool {
	return observation.runtime != nil &&
		observation.runtime.PublishTransferFinalization(progress, settlement)
}

func (observation getObservation) loseLifecycle() {
	if observation.runtime != nil {
		observation.runtime.ReportObserverLoss(1, 0)
	}
}

func (observation getObservation) commandFailure(exit int, cause error) int {
	projectedExit, ok := getEventExit(exit)
	if !ok || projectedExit == clievent.ExitSuccess {
		projectedExit = clievent.ExitFailure
		exit = ExitFailure
	}
	event, err := commandprojection.ProjectCommandFailure(clievent.CommandGet, projectedExit, cause)
	if err != nil {
		observation.loseLifecycle()
		event = unexpectedGetFailure(projectedExit)
	}
	observation.publish(event)
	return exit
}

func (observation getObservation) commandFailureCode(exit int, code clievent.FailureCode) int {
	failure, err := clievent.NewFailure(code)
	projectedExit, validExit := getEventExit(exit)
	if err != nil || !validExit || projectedExit == clievent.ExitSuccess {
		return observation.commandFailure(ExitFailure, commandprojection.ErrInvalidProjection)
	}
	event, err := clievent.NewCommandFailed(clievent.CommandGet, projectedExit, failure)
	if err != nil {
		return observation.commandFailure(ExitFailure, commandprojection.ErrInvalidProjection)
	}
	observation.publish(event)
	return exit
}

func unexpectedGetFailure(exit clievent.ExitCode) clievent.CommandFailed {
	failure, _ := clievent.NewFailure(clievent.FailureUnexpected)
	event, _ := clievent.NewCommandFailed(clievent.CommandGet, exit, failure)
	return event
}

func getEventExit(exit int) (clievent.ExitCode, bool) {
	switch exit {
	case ExitOK:
		return clievent.ExitSuccess, true
	case ExitFailure:
		return clievent.ExitFailure, true
	case ExitUsage:
		return clievent.ExitUsage, true
	case ExitNetwork:
		return clievent.ExitNetwork, true
	case ExitDrift:
		return clievent.ExitDrift, true
	default:
		return 0, false
	}
}

func (observation getObservation) warning(cause error) {
	failure, present := commandprojection.ClassifyError(cause)
	if present {
		observation.warningFailure(failure)
	}
}

func (observation getObservation) warningCode(code clievent.FailureCode) {
	failure, err := clievent.NewFailure(code)
	if err != nil {
		observation.loseLifecycle()
		return
	}
	observation.warningFailure(failure)
}

func (observation getObservation) warningFailure(failure clievent.Failure) {
	event, err := clievent.NewWarning(clievent.CommandGet, failure)
	if err != nil {
		observation.loseLifecycle()
		return
	}
	observation.observe(event)
}

func (observation getObservation) relayConnected(endpoint v2.RelayEndpoint) {
	authority, err := commandprojection.RelayAuthority(endpoint)
	if err != nil {
		observation.loseLifecycle()
		return
	}
	event, err := clievent.NewRelayConnected(clievent.CommandGet, authority)
	if err != nil {
		observation.loseLifecycle()
		return
	}
	observation.publish(event)
}

func (observation getObservation) relayLifecycle(value relayv2.LifecycleTrace) {
	event, err := commandprojection.ProjectRelayLifecycle(clievent.CommandGet, value)
	if err != nil {
		observation.loseLifecycle()
		return
	}
	observation.observe(event)
}

func (observation getObservation) webRTCLifecycle(value transportwebrtc.LifecycleTrace) {
	event, err := commandprojection.ProjectWebRTCLifecycle(clievent.CommandGet, value)
	if err != nil {
		observation.loseLifecycle()
		return
	}
	observation.observe(event)
}

func (observation getObservation) filesystemOutput(value osfs.FilesystemOutputTrace) {
	event, err := commandprojection.ProjectFilesystemOutput(value)
	if err != nil {
		observation.loseLifecycle()
		return
	}
	observation.observe(event)
}

func (observation getObservation) transferLifecycle(value transfer.TransferLifecycleTrace) {
	event, err := commandprojection.ProjectTransferLifecycle(value)
	if err != nil {
		observation.loseLifecycle()
		return
	}
	observation.observe(event)
}

func (observation getObservation) protocolOperation(value sessionruntime.ProtocolOperationTrace) {
	event, err := commandprojection.ProjectProtocolOperation(clievent.CommandGet, value)
	if err != nil {
		observation.loseLifecycle()
		return
	}
	observation.observe(event)
}

func (observation getObservation) protocolTracer() sessionruntime.ProtocolOperationTracer {
	if observation.runtime == nil || !observation.runtime.protocolDiagnosticsEnabled() {
		return nil
	}
	return sessionruntime.ProtocolOperationTraceFunc(observation.protocolOperation)
}

func (observation getObservation) progress(
	receiveOperation receivecontract.OperationID,
	transferJob transfer.TransferJobID,
	value transfer.ReceiveProgressSnapshot,
) {
	event, err := commandprojection.ProjectTransferProgress(receiveOperation, transferJob, value)
	if err != nil {
		observation.loseLifecycle()
		return
	}
	observation.observe(event)
}

func (observation getObservation) contentPath(path clievent.ContentPath) {
	event, err := clievent.NewContentPathSelected(path)
	if err != nil {
		observation.loseLifecycle()
		return
	}
	observation.publish(event)
}

func (observation getObservation) fallback(code clievent.FailureCode) {
	failure, err := clievent.NewFailure(code)
	if err != nil {
		observation.loseLifecycle()
		return
	}
	event, err := clievent.NewFallback(
		clievent.CommandGet,
		clievent.TransportWebRTC,
		clievent.TransportRelay,
		failure,
	)
	if err != nil {
		observation.loseLifecycle()
		return
	}
	observation.publish(event)
}

func (observation getObservation) laneAdopted(
	session protocolsession.ProtocolSessionID,
	lane sessionruntime.LaneIdentity,
) {
	projectedSession, err := commandprojection.ProtocolSessionID(session)
	if err != nil {
		observation.loseLifecycle()
		return
	}
	projectedLane, err := commandprojection.LaneIdentity(lane)
	if err != nil {
		observation.loseLifecycle()
		return
	}
	event, err := clievent.NewLaneAdopted(
		clievent.CommandGet,
		projectedSession,
		projectedLane,
		clievent.TransportWebRTC,
	)
	if err != nil {
		observation.loseLifecycle()
		return
	}
	observation.observe(event)
}

func (observation getObservation) receiverTermination(value receiverPeerTerminationTrace) {
	if value.diagnosticsTruncated {
		observation.loseLifecycle()
	}
	for _, cause := range value.retainedCauseClasses {
		failure, ok := commandprojection.ProjectReceiverCauseClass(cause)
		if ok {
			observation.warningFailure(failure)
			return
		}
	}
	if value.peerShutdownFailed {
		observation.warningCode(clievent.FailurePeerShutdown)
	} else if value.channelDrainFailed {
		observation.warningCode(clievent.FailurePeerChannelDrain)
	}
}
