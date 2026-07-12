package harness

import (
	"time"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/relay/protocol"
)

const (
	DefaultAddress  = "127.0.0.1:17845"
	ChannelLabel    = "windshare-frame-channel"
	ChannelProtocol = "windshare-v1"

	lowWaterMarkBytes  uint64 = 256 * 1024
	highWaterMarkBytes uint64 = 1024 * 1024
	maxBurstMessages          = 256
	terminalFrameBytes        = 64

	clientProbeMarker    byte = 0x41
	clientBurstMarker    byte = 0x42
	clientTerminalMarker byte = 0x43
	serverProbeMarker    byte = 0x51
	serverBurstMarker    byte = 0x52
	serverTerminalMarker byte = 0x53

	backpressureTimeout = 15 * time.Second
)

var spikeSessionID = protocol.SessionID{1, 2, 3, 4, 5, 6, 7, 8}

type PublicConfig struct {
	SessionID          string `json:"sessionId"`
	ChannelLabel       string `json:"channelLabel"`
	ChannelProtocol    string `json:"channelProtocol"`
	MaxFrameSize       int    `json:"maxFrameSize"`
	LowWaterMark       uint64 `json:"lowWaterMark"`
	HighWaterMark      uint64 `json:"highWaterMark"`
	MaxBurstMessages   int    `json:"maxBurstMessages"`
	TerminalFrameBytes int    `json:"terminalFrameBytes"`
	ClientProbeMarker  byte   `json:"clientProbeMarker"`
	ClientBurstMarker  byte   `json:"clientBurstMarker"`
	ClientTerminal     byte   `json:"clientTerminalMarker"`
	ServerProbeMarker  byte   `json:"serverProbeMarker"`
	ServerBurstMarker  byte   `json:"serverBurstMarker"`
	ServerTerminal     byte   `json:"serverTerminalMarker"`
}

func publicConfig() PublicConfig {
	return PublicConfig{
		SessionID:          spikeSessionID.String(),
		ChannelLabel:       ChannelLabel,
		ChannelProtocol:    ChannelProtocol,
		MaxFrameSize:       session.MaxFrameSize,
		LowWaterMark:       lowWaterMarkBytes,
		HighWaterMark:      highWaterMarkBytes,
		MaxBurstMessages:   maxBurstMessages,
		TerminalFrameBytes: terminalFrameBytes,
		ClientProbeMarker:  clientProbeMarker,
		ClientBurstMarker:  clientBurstMarker,
		ClientTerminal:     clientTerminalMarker,
		ServerProbeMarker:  serverProbeMarker,
		ServerBurstMarker:  serverBurstMarker,
		ServerTerminal:     serverTerminalMarker,
	}
}

func patternedFrame(marker byte, size int) []byte {
	frame := make([]byte, size)
	if size == 0 {
		return frame
	}
	frame[0] = marker
	for i := 1; i < len(frame); i++ {
		frame[i] = byte((i*31 + 17) % 251)
	}
	return frame
}

func validPattern(frame []byte, marker byte, size int) bool {
	if len(frame) != size || size == 0 || frame[0] != marker {
		return false
	}
	for i := 1; i < len(frame); i++ {
		if frame[i] != byte((i*31+17)%251) {
			return false
		}
	}
	return true
}
