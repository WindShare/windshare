package session

import (
	"errors"
	"github.com/windshare/windshare/core/session/protocolsession"
)

const (
	DefaultActiveOperations     = 256
	DefaultRecentTombstones     = protocolsession.OperationReplayWindow
	SessionStoppedCode          = protocolsession.SessionTerminalCodeLast
	MaximumTerminalMessageBytes = protocolsession.MaxSessionTerminalMessageBytes
)

// PeerStopped reports whether err carries the peer's authenticated session
// terminal announcing that its runtime stopped, which ends the share for good.
func PeerStopped(err error) bool {
	var peer *protocolsession.PeerSessionTerminalError
	if !errors.As(err, &peer) {
		return false
	}
	terminal, decodeErr := peer.Terminal()
	return decodeErr == nil && terminal.Code == SessionStoppedCode
}

var (
	ErrRuntimeConfig = errors.New("session runtime configuration is invalid")
	ErrRuntimeClosed = errors.New("session runtime is closed")
	ErrHandshake     = errors.New("session runtime handshake failed")
)

var ErrOperationMissing = errors.New("session runtime operation is not active")

var ErrOperationOverflow = errors.New("session runtime operation response queue is full")
