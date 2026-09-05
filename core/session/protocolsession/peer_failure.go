package protocolsession

const (
	PeerOperationCodeFirst            uint16 = 0x5000
	PeerOperationCodeLast             uint16 = 0x5fff
	PeerOperationCodeICE              uint16 = 0x5005
	PeerOperationCodeSTUN             uint16 = 0x5006
	PeerOperationCodeTransport        uint16 = 0x5007
	PeerOperationCodeDTLS             uint16 = 0x5008
	PeerOperationCodePolicy           uint16 = 0x5009
	PeerOperationCodeAuthentication   uint16 = 0x500a
	PeerOperationCodeSessionInvariant uint16 = 0x500b
)

type PeerFailureRecoveryScope string

const (
	PeerFailureAttemptTransient PeerFailureRecoveryScope = "attempt-transient"
	PeerFailurePathTerminal     PeerFailureRecoveryScope = "path-terminal"
	PeerFailureSessionTerminal  PeerFailureRecoveryScope = "session-terminal"
)

// PeerFailureScope grants recovery authority only to known typed reasons. Text
// and Retryable never revive an operation, and an unknown reason cannot confer
// session termination authority or an automatic retry.
func PeerFailureScope(code uint16) PeerFailureRecoveryScope {
	switch code {
	case PeerOperationCodeNegotiation, PeerOperationCodeTimeout, PeerOperationCodeICE, PeerOperationCodeSTUN, PeerOperationCodeTransport, PeerOperationCodeDTLS:
		return PeerFailureAttemptTransient
	case PeerOperationCodeAuthentication, PeerOperationCodeSessionInvariant:
		return PeerFailureSessionTerminal
	default:
		return PeerFailurePathTerminal
	}
}
