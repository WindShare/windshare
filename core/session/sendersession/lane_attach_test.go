package sendersession

import (
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
)

var allowSenderPeerSettlement = PeerAdmissionControlFunc(
	func(protocolsession.OperationID, session.LaneIdentity) bool { return true },
)
