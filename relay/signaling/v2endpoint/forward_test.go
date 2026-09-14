package v2endpoint

import v2 "github.com/windshare/windshare/relay/protocol/v2"

func (peer *connection) takeForward() ([]byte, bool) {
	frame, _, ok := peer.takeForwardWithTrace()
	return frame, ok
}

func (peer *connection) enqueueForward(sessionID v2.RelaySessionID, encoded []byte) bool {
	accepted, _ := peer.tryForward(sessionID, encoded)
	return accepted
}
