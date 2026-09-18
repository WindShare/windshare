package receive

import (
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/downloadmetrics"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/internal/testrun"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
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
type RelayObserved struct {
	event
	Value relayv2.LifecycleTrace
}
type WebRTCObserved struct {
	event
	Value wsrtc.LifecycleTrace
}
type FilesystemObserved struct {
	event
	Value osfs.FilesystemOutputTrace
}
type TransferObserved struct {
	event
	Value transfer.TransferLifecycleTrace
}
type ProtocolObserved struct {
	event
	Value sessionruntime.ProtocolObservation
}
type LaneObserved struct {
	event
	Value transfer.LaneSettlementSummary
}
type NativeObserved struct {
	event
	Value nativepeer.Observation
}
type PeerDiagnosticObserved struct {
	event
	Value v2peer.PeerDiagnosticObservation
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
	Lane    sessionruntime.LaneIdentity
}
type Milestone struct {
	event
	Component testrun.Component
	Name      testrun.Milestone
	Outcome   testrun.Outcome
}
type ObservationSource uint8

const (
	ObservationRelay ObservationSource = iota + 1
	ObservationWebRTC
	ObservationLane
	ObservationNative
	ObservationProtocol
	ObservationPeerTermination
	ObservationPeerDiagnostic
)

type ObservationLoss struct {
	event
	Source ObservationSource
	Count  uint64
}

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
