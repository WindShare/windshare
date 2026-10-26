package sendersession

import (
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
)

var allowSenderPeerSettlement = PeerAdmissionControlFunc(
	func(protocolsession.OperationID, sessioncontract.LaneIdentity) bool { return true },
)
