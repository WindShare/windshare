package webrtc

import "errors"

var (
	ErrNilDataChannel          = errors.New("webrtc: DataChannel must not be nil")
	ErrInvalidDataChannel      = errors.New("webrtc: DataChannel configuration is invalid")
	ErrInvalidFlowControl      = errors.New("webrtc: flow-control profile is invalid")
	ErrChannelNotOpen          = errors.New("webrtc: channel is not open")
	ErrChannelClosed           = errors.New("webrtc: channel is closed")
	ErrFrameBounds             = errors.New("webrtc: frame is outside the allowed size")
	ErrPeerProtocol            = errors.New("webrtc: peer violated the DataChannel protocol")
	ErrTerminalNotAcknowledged = errors.New("webrtc: terminal frame was not acknowledged")
	ErrRemoteClosed            = errors.New("webrtc: peer closed the DataChannel")
	ErrTransport               = errors.New("webrtc: DataChannel transport failed")
)
