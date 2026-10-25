package eventproject

import (
	v2peer "github.com/windshare/windshare/connectivity/v2peer"
	appobs "github.com/windshare/windshare/engine/observation"
)

func ProjectReceiverCauseClass(value v2peer.ReceiverCauseClass) (appobs.Failure, bool) {
	var code appobs.FailureCode
	switch value {
	case v2peer.ReceiverCauseRuntimeClosed:
		code = appobs.FailurePeerStopped
	case v2peer.ReceiverCauseConfiguration:
		code = appobs.FailurePeerConfiguration
	case v2peer.ReceiverCauseOperationMissing:
		code = appobs.FailurePeerOperationMissing
	case v2peer.ReceiverCauseNegotiationTimeout, v2peer.ReceiverCauseAdmissionTimeout:
		code = appobs.FailurePeerTimeout
	case v2peer.ReceiverCauseCandidateLimit:
		code = appobs.FailurePeerCandidates
	case v2peer.ReceiverCauseChannelAdmission:
		code = appobs.FailurePeerAdmission
	case v2peer.ReceiverCauseEventCapacity:
		code = appobs.FailurePeerEventCapacity
	case v2peer.ReceiverCauseNegotiation:
		code = appobs.FailurePeerNegotiation
	case v2peer.ReceiverCauseProtocol:
		code = appobs.FailurePeerProtocol
	case v2peer.ReceiverCauseDeadline:
		code = appobs.FailureDeadline
	case v2peer.ReceiverCausePeerShutdown:
		code = appobs.FailurePeerShutdown
	case v2peer.ReceiverCauseChannelDrain:
		code = appobs.FailurePeerChannelDrain
	case v2peer.ReceiverCauseUnknown:
		code = appobs.FailureUnexpected
	default:
		return appobs.Failure{}, false
	}
	return mustFailure(code), true
}
