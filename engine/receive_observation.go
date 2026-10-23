package engine

import "github.com/windshare/windshare/engine/internal/receive"

type ReceiveGenerationChanged = receive.GenerationChanged
type ReceiveAdmissionObserved = receive.AdmissionObserved
type ReceiveAdmissionTrigger = receive.AdmissionTrigger
type ReceiveAdmissionTerminalOwner = receive.AdmissionTerminalOwner
type ReceiveMilestone = receive.Milestone
type ReceiveObservationLoss = receive.ObservationLoss
type ReceiveObservationSource = receive.ObservationSource
type ReceiveLocalStopReason = receive.ReceiverLocalStopReason
type ReceiveContentPath = receive.ContentPath

const (
	ReceiveFailureInvalidInput              = receive.FailureInvalidInput
	ReceiveFailureOutputContract            = receive.FailureOutputContract
	ReceiveFailureOutputFileAlreadyActive   = receive.FailureOutputFileAlreadyActive
	ReceiveFailureOutputNeedsAttention      = receive.FailureOutputNeedsAttention
	ReceiveFailureOutputOwnership           = receive.FailureOutputOwnership
	ReceiveFailureOutputRecoveryUnavailable = receive.FailureOutputRecoveryUnavailable
	ReceiveFailurePeerConfiguration         = receive.FailurePeerConfiguration
	ReceiveFailurePeerNegotiation           = receive.FailurePeerNegotiation
	ReceiveFailurePeerProtocol              = receive.FailurePeerProtocol
	ReceiveFailurePeerSignaling             = receive.FailurePeerSignaling
	ReceiveFailurePeerStopped               = receive.FailurePeerStopped

	ReceiveLocalStopNone                  = receive.ReceiverLocalStopNone
	ReceiveLocalStopCaller                = receive.ReceiverLocalStopCaller
	ReceiveLocalStopOutputAdmission       = receive.ReceiverLocalStopOutputAdmission
	ReceiveLocalStopRuntimeSessionFailure = receive.ReceiverLocalStopRuntimeSessionFailure
	ReceiveLocalStopNormalCompletion      = receive.ReceiverLocalStopNormalCompletion

	ReceiveContentPathRelay          = receive.ContentPathRelay
	ReceiveContentPathDirect         = receive.ContentPathDirect
	ReceiveContentPathDirectAndRelay = receive.ContentPathDirectAndRelay

	ReceiveObservationRelay           = receive.ObservationRelay
	ReceiveObservationWebRTC          = receive.ObservationWebRTC
	ReceiveObservationLane            = receive.ObservationLane
	ReceiveObservationNative          = receive.ObservationNative
	ReceiveObservationProtocol        = receive.ObservationProtocol
	ReceiveObservationPeerTermination = receive.ObservationPeerTermination
	ReceiveObservationPeerDiagnostic  = receive.ObservationPeerDiagnostic
)
