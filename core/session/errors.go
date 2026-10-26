package sessioncontract

import (
	"errors"
	"github.com/windshare/windshare/core/session/protocolsession"
)

const (
	DefaultActiveOperations     = 256
	DefaultTrackedOperations    = 4_096
	SessionStoppedCode          = protocolsession.SessionTerminalCodeLast
	MaximumTerminalMessageBytes = protocolsession.MaxSessionTerminalMessageBytes
)

var (
	ErrRuntimeConfig = errors.New("session runtime configuration is invalid")
	ErrRuntimeClosed = errors.New("session runtime is closed")
	ErrHandshake     = errors.New("session runtime handshake failed")
)

var ErrOperationMissing = errors.New("session runtime operation is not active")

var ErrOperationOverflow = errors.New("session runtime operation response queue is full")
