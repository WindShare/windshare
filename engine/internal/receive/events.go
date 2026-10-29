package receive

import (
	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/downloadmetrics"
	"github.com/windshare/windshare/core/session/protocolsession"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

type event struct{}

func (event) EngineEvent() {}

type FailureCode uint8

const (
	FailureInvalidInput FailureCode = iota + 1
	FailureOutputContract
	FailureOutputFileAlreadyActive
	FailureOutputNeedsAttention
	FailureOutputOwnership
	FailureOutputRecoveryUnavailable
	FailureRetainedOutputMissing
	FailurePeerConfiguration
	FailurePeerNegotiation
	FailurePeerProtocol
	FailurePeerSignaling
	FailurePeerStopped
)

type ReceiverLocalStopReason uint8

const (
	ReceiverLocalStopNone ReceiverLocalStopReason = iota + 1
	ReceiverLocalStopCaller
	ReceiverLocalStopOutputAdmission
	ReceiverLocalStopRuntimeSessionFailure
	ReceiverLocalStopNormalCompletion
)

type ContentPath uint8

const (
	ContentPathRelay ContentPath = iota + 1
	ContentPathDirect
	ContentPathDirectAndRelay
)

type Warning struct {
	event
	Code  FailureCode
	Cause error
}
type RelayConnected struct {
	event
	Endpoint v2.RelayEndpoint
}
type RecoveryObserved struct {
	event
	Value relayset.ReceiverRecoveryObservation
}
type PeerTerminated struct {
	event
	Value     v2peer.ReceiverTerminationTrace
	LocalStop ReceiverLocalStopReason
}
type ProgressObserved struct {
	event
	Operation    receivecontract.OperationID
	Job          transfer.TransferJobID
	Value        transfer.ReceiveProgressSnapshot
	Connectivity downloadmetrics.Snapshot
}
type ContentPathObserved struct {
	event
	Path ContentPath
}
type FallbackObserved struct {
	event
	Code FailureCode
}
type LaneAdopted struct {
	event
	Session protocolsession.ProtocolSessionID
	Lane    session.LaneIdentity
}
type ObservationSource string

const (
	ObservationRelay ObservationSource = "relay"

	ObservationWebRTC ObservationSource = "webrtc"

	ObservationLane ObservationSource = "lane"

	ObservationNative ObservationSource = "native"

	ObservationProtocol ObservationSource = "protocol"

	ObservationPeerTermination ObservationSource = "peer_termination"

	ObservationPeerDiagnostic ObservationSource = "peer_diagnostic"
)

type AdmissionTrigger = receiverAdmissionTrigger
type AdmissionTerminalOwner = receiverAdmissionTerminalOwner
type GenerationChanged struct {
	event
	Operation         receivecontract.OperationID
	Job               transfer.TransferJobID
	Previous, Current protocolsession.ProtocolSessionID
}
type AdmissionObserved struct {
	event
	Operation     receivecontract.OperationID
	Job           transfer.TransferJobID
	Session       protocolsession.ProtocolSessionID
	Trigger       AdmissionTrigger
	TerminalOwner AdmissionTerminalOwner
	Cause         error
}

// RelayContentClaimed records ownership of an admission attempt, before resume.
// Completion and useful byte delivery are independent observations.
type RelayContentClaimed struct {
	event
	Operation receivecontract.OperationID
	Job       transfer.TransferJobID
	Session   protocolsession.ProtocolSessionID
	Trigger   AdmissionTrigger
}
